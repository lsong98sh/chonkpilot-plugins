// chonkpilot-crontab —— 定时任务插件（crontab_add / update / list / enable / disable / delete）。
//
// 形态：独立 exe（插件桥 UDS + 自建 MCP HTTP）；任务为**本机全局**，不区分项目。
// 动作二选一：`run`（执行命令，可选完成通知）/ `notify`（系统通知，param = HTML）。
// 调度：1s ticker 扫描（单 goroutine），同任务不重入；状态落 <插件数据目录>/tasks.json。
// UI：任务页 / 状态栏徽标 / 配置页（webui，embed）；数字变化经 plugin-event 主动推送，零轮询。
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/safepath"
)

const pluginVersion = "1.0.0"

//go:embed all:tools
var toolsFS embed.FS

//go:embed all:webui
var webuiFS embed.FS

const (
	toolsRoot   = "tools/crontab"
	uiAssetRoot = "webui"
)

// 复用层别名：参数取值统一由 pluginsdk 提供。
var asString = pluginsdk.Str

// crontabPlugin 是插件运行时（工具 + UI 动作 + 调度共享同一 store）。
type crontabPlugin struct {
	p        *pluginsdk.Plugin
	store    *Store
	cfg      *configManager
	notifier Notifier
	sched    *scheduler
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "crontab",
		Name:    "定时任务",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("crontab: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	store := newStore(dataDir())
	if err := store.Load(); err != nil {
		p.Logger().Warn("crontab: 读取任务文件失败（按空任务启动）", "err", err)
	}
	cfg := newConfigManager(p)
	if err := cfg.Init(context.Background()); err != nil {
		p.Logger().Warn("crontab: 读取配置失败（按默认值运行）", "err", err)
	}
	notifier := newSystemNotifier(
		func(format string, args ...any) { p.Logger().Info(fmt.Sprintf(format, args...)) },
		func() string { return cfg.Settings().NotifyFallback },
	)
	run := &runner{cfg: cfg, notifier: notifier, logf: func(format string, args ...any) {
		p.Logger().Info(fmt.Sprintf(format, args...))
	}}
	run.resolveSandbox = func(ctx context.Context, workDir string) ([]string, error) {
		return resolveSandbox(ctx, p, workDir)
	}
	// SANDBOX-WRITE-1 P4（C11）：会话归属经宿主能力面（插件不直接开库）。
	run.sessions = newBridgeSessions(p)
	// SANDBOX-WRITE-1 P3：执行前确保项目已打开（未打开 → 宿主隐藏打开，C13/C17）。
	run.ensureWorkdir = func(ctx context.Context, workDir string) (string, error) {
		return ensureWorkdir(ctx, p, workDir)
	}
	t := &crontabPlugin{
		p: p, store: store, cfg: cfg, notifier: notifier,
		sched: newScheduler(store, cfg, run, func(format string, args ...any) {
			p.Logger().Info(fmt.Sprintf(format, args...))
		}),
	}

	contracts, err := loadContracts()
	if err != nil {
		p.Logger().Error("crontab: 加载工具契约失败", "err", err)
		os.Exit(1)
	}
	if missing := registerTools(p, t, contracts); len(missing) > 0 {
		p.Logger().Error("crontab: 契约缺少实现", "tools", strings.Join(missing, ","))
		os.Exit(1)
	}

	// UI 动作面（配置页 + 任务页 + 徽标）。
	for name, fn := range cfg.Actions() {
		p.OnRequest(name, fn)
	}
	p.OnRequest("list", t.uiList)
	p.OnRequest("toggle", t.uiToggle)
	p.OnRequest("delete", t.uiDelete)
	p.OnRequest("runs", t.uiRuns)
	p.OnRequest("detail", t.uiDetail)
	p.OnRequest("badge", t.uiBadge)
	p.OnRequest("status", t.info)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	t.sched.Start()

	if err := p.Run(ctx); err != nil {
		p.Logger().Warn("crontab: 运行结束", "err", err)
	}
	cancel()
	t.sched.Stop()
}

// loadContracts 读取 embed 内的工具契约（按名称排序）。
func loadContracts() ([]contract.Tool, error) {
	entries, err := fs.ReadDir(toolsFS, toolsRoot)
	if err != nil {
		return nil, fmt.Errorf("读取契约目录 %s: %w", toolsRoot, err)
	}
	out := make([]contract.Tool, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tool.md") {
			continue
		}
		raw, err := fs.ReadFile(toolsFS, toolsRoot+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		tool, err := contract.Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if tool.Name == "" {
			continue
		}
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// registerTools 按契约注册工具（handler 表按工具名分派），返回缺少实现的工具名。
func registerTools(p *pluginsdk.Plugin, t *crontabPlugin, tools []contract.Tool) []string {
	handlers := map[string]pluginsdk.ToolFunc{
		"crontab_add":     t.add,
		"crontab_update":  t.update,
		"crontab_list":    t.list,
		"crontab_enable":  t.enable,
		"crontab_disable": t.disable,
		"crontab_delete":  t.delete,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			continue
		}
		p.Tool(tool.Name, tool.Description, tool.Schema, fn)
		if tool.Hot {
			p.HotTool(tool.Name)
		}
		seen[tool.Name] = true
	}
	var missing []string
	for name := range handlers {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// ── 工具：crontab_add ─────────────────────────

func (t *crontabPlugin) add(_ context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	cronExpr := strings.TrimSpace(asString(args["cron"]))
	if _, err := parseCron(cronExpr); err != nil {
		return nil, err
	}
	spec, err := ParseActionInput(args["action"])
	if err != nil {
		return nil, err
	}
	sandbox, _, err := parseSandboxArg(args["sandbox"])
	if err != nil {
		return nil, err
	}
	cfg := t.cfg.Settings()
	if len(t.store.List()) >= cfg.MaxTasks {
		return nil, fmt.Errorf("任务数已达上限（%d）", cfg.MaxTasks)
	}
	if !allowedWorkDir(cfg, cx.WorkDir) {
		return nil, fmt.Errorf("work_dir 不在允许的目录白名单内: %s", cx.WorkDir)
	}
	action, err := spec.Canonical()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	next, err := nextRun(cronExpr, now)
	if err != nil {
		return nil, err
	}
	task := Task{
		ID:          newTaskID(),
		Cron:        cronExpr,
		Action:      action,
		Description: spec.Description(),
		Enabled:     true,
		CreatedAt:   now.Unix(),
		CreatedBy: CreatedBy{
			InstanceID: cx.InstanceID,
			SessionID:  cx.Session,
			WorkDir:    cx.WorkDir,
		},
		NextRunAt: next,
		Sandbox:   sandbox,
	}
	if err := t.store.Add(task); err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	if cfg.CreateToast {
		body := fmt.Sprintf("已创建：%s\n下次执行：%s", task.Description, formatUnix(next))
		if nerr := t.notifier.Notify("ChonkPilot 定时任务", body, ""); nerr != nil {
			t.p.Logger().Info("crontab: 创建通知失败", "err", nerr)
		}
	}
	return map[string]any{"ok": true, "task": taskView(task)}, nil
}

// ── 工具：crontab_update ──────────────────────

func (t *crontabPlugin) update(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	if _, ok := t.store.Get(id); !ok {
		return nil, ErrTaskNotFound
	}
	newCron := ""
	if raw, ok := args["cron"]; ok {
		newCron = strings.TrimSpace(asString(raw))
		if newCron != "" {
			if _, err := parseCron(newCron); err != nil {
				return nil, err
			}
		}
	}
	var spec *ActionSpec
	if raw, ok := args["action"]; ok && raw != nil {
		s, err := ParseActionInput(raw)
		if err != nil {
			return nil, err
		}
		spec = s
	}
	sandbox, sandboxSet, err := parseSandboxArg(args["sandbox"])
	if err != nil {
		return nil, err
	}
	var updated Task
	err = t.store.Update(id, func(cur *Task) error {
		if newCron != "" {
			cur.Cron = newCron
		}
		if spec != nil {
			action, err := spec.Canonical()
			if err != nil {
				return err
			}
			cur.Action = action
			cur.Description = spec.Description()
		}
		if sandboxSet {
			cur.Sandbox = sandbox // nil = 恢复「继承插件配置」
		}
		if cur.Enabled {
			n, err := nextRun(cur.Cron, time.Now())
			if err != nil {
				return err
			}
			cur.NextRunAt = n
		}
		updated = *cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	return map[string]any{"ok": true, "task": taskView(updated)}, nil
}

// ── 工具：crontab_list ────────────────────────

func (t *crontabPlugin) list(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	filter, hasFilter := boolArg(args["enabled"])
	tasks := t.store.List()
	views := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		if hasFilter && task.Enabled != filter {
			continue
		}
		views = append(views, taskView(task))
	}
	return map[string]any{"ok": true, "tasks": views, "count": len(views)}, nil
}

// ── 工具：enable / disable / delete ──────────

func (t *crontabPlugin) enable(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	return t.setEnabled(args, true)
}

func (t *crontabPlugin) disable(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	return t.setEnabled(args, false)
}

func (t *crontabPlugin) setEnabled(args map[string]any, enabled bool) (any, error) {
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	var updated Task
	err := t.store.Update(id, func(cur *Task) error {
		cur.Enabled = enabled
		if enabled {
			cur.DisabledReason = "" // 用户重新启用 → 清掉 work_dir_missing 等自动停用原因
			n, err := nextRun(cur.Cron, time.Now())
			if err != nil {
				return err
			}
			cur.NextRunAt = n
		} else {
			cur.NextRunAt = 0
		}
		updated = *cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	return map[string]any{"ok": true, "task_id": id, "enabled": updated.Enabled, "task": taskView(updated)}, nil
}

func (t *crontabPlugin) delete(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	if _, err := t.store.Delete(id); err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	return map[string]any{"ok": true, "task_id": id, "count": len(t.store.List())}, nil
}

// ── UI 动作（宿主 POST /plugins/crontab/api/<action>）─────────

func (t *crontabPlugin) uiList(_ context.Context, _ map[string]any) (any, error) {
	tasks := t.store.List()
	views := make([]map[string]any, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, taskView(task))
	}
	return map[string]any{"ok": true, "tasks": views, "count": len(views)}, nil
}

func (t *crontabPlugin) uiToggle(_ context.Context, payload map[string]any) (any, error) {
	id := strings.TrimSpace(asString(payload["task_id"]))
	enabled, ok := boolArg(payload["enabled"])
	if id == "" || !ok {
		return nil, errors.New("task_id 与 enabled 必填")
	}
	return t.setEnabled(map[string]any{"task_id": id}, enabled)
}

func (t *crontabPlugin) uiDelete(ctx context.Context, payload map[string]any) (any, error) {
	return t.delete(ctx, payload, pluginsdk.CallContext{})
}

func (t *crontabPlugin) uiRuns(_ context.Context, payload map[string]any) (any, error) {
	limit := intInt(payload["limit"])
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	taskID := strings.TrimSpace(asString(payload["task_id"]))
	return map[string]any{"ok": true, "runs": t.store.RecentRunsFor(taskID, limit)}, nil
}

// uiDetail 返回任务详情：任务本体 + **沙箱实际状态**（开/关/不可用原因）+ 最近运行记录。
// 沙箱状态按当前配置与宿主能力**实时解析**（不落库，避免展示过期结论）。
func (t *crontabPlugin) uiDetail(ctx context.Context, payload map[string]any) (any, error) {
	id := strings.TrimSpace(asString(payload["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	task, ok := t.store.Get(id)
	if !ok {
		return nil, ErrTaskNotFound
	}
	cfg := t.cfg.Settings()
	enabled := effectiveSandbox(task, cfg)
	status := map[string]any{
		"enabled": enabled,
		"source":  "config",
		"default": cfg.Sandbox,
	}
	if task.Sandbox != nil {
		status["source"] = "task"
	}
	if enabled {
		if !workDirExists(task.CreatedBy.WorkDir) {
			status["available"] = false
			status["reason"] = ReasonWorkDirMissing
		} else if dirs, err := t.resolveSandboxDirs(ctx, task.CreatedBy.WorkDir); err != nil {
			status["available"] = false
			status["reason"] = "sandbox_unavailable: " + err.Error()
		} else {
			status["available"] = true
			status["writable_dirs"] = dirs
		}
	}
	return map[string]any{
		"ok":      true,
		"task":    taskView(task),
		"sandbox": status,
		"runs":    t.store.RecentRunsFor(id, 5),
		// 会话归属（P4 / C12）：任务 → 会话数（1）→ 最近一轮（标题 / 时间 / 状态）。
		// 只读、不建会话；未执行过 → {available:false}。
		"session": sessionInfo(ctx, t.p, task),
	}, nil
}

// resolveSandboxDirs 经宿主能力面取该 work_dir 的可写白名单（详情页展示用）。
func (t *crontabPlugin) resolveSandboxDirs(ctx context.Context, workDir string) ([]string, error) {
	return resolveSandbox(ctx, t.p, workDir)
}

func (t *crontabPlugin) uiBadge(_ context.Context, _ map[string]any) (any, error) {
	tasks := t.store.List()
	enabled := 0
	var next int64
	for _, task := range tasks {
		if !task.Enabled {
			continue
		}
		enabled++
		if task.NextRunAt > 0 && (next == 0 || task.NextRunAt < next) {
			next = task.NextRunAt
		}
	}
	return map[string]any{"ok": true, "count": len(tasks), "enabled": enabled, "next_run_at": next}, nil
}

func (t *crontabPlugin) info(_ context.Context, _ map[string]any) (any, error) {
	cfg := t.cfg.Settings()
	return map[string]any{
		"ok":                true,
		"version":           pluginVersion,
		"tasks":             len(t.store.List()),
		"defaultTimeoutSec": cfg.DefaultTimeoutSec,
		"missedGraceSec":    cfg.MissedGraceSec,
		"maxTasks":          cfg.MaxTasks,
		"createToast":       cfg.CreateToast,
		"notifyFallback":    cfg.NotifyFallback,
		"sandbox":           cfg.Sandbox,
	}, nil
}

// notifyChanged 主动推送计数变化（plugin-event → UI 徽标 / 任务页；零轮询）。
func (t *crontabPlugin) notifyChanged(ctx context.Context) {
	tasks := t.store.List()
	enabled := 0
	for _, task := range tasks {
		if task.Enabled {
			enabled++
		}
	}
	payload := map[string]any{"kind": "crontab-changed", "count": len(tasks), "enabled": enabled}
	if err := t.p.PublishUI(ctx, "", payload); err != nil {
		t.p.Logger().Info("crontab: 推送变更事件失败", "err", err)
	}
}

// ── 资源面（插件自持 webui）───────────────────

func uiAssetGet(_ context.Context, payload map[string]any) (any, error) {
	raw, _ := payload["path"].(string)
	rel, ok := normalizeUIAssetPath(raw)
	if !ok {
		return nil, fmt.Errorf("invalid_path: %q", raw)
	}
	b, err := fs.ReadFile(webuiFS, uiAssetRoot+"/"+rel)
	if err != nil {
		return nil, fmt.Errorf("not_found: %s", rel)
	}
	return map[string]any{"data": b, "size": len(b)}, nil
}

// normalizeUIAssetPath 把宿主传入路径归一为 webui 根内相对路径（越界一律拒绝）。
func normalizeUIAssetPath(p string) (string, bool) {
	s := strings.TrimSpace(p)
	if s == "" || strings.ContainsAny(s, `\:`) || strings.HasPrefix(s, "/") {
		return "", false
	}
	s = strings.TrimPrefix(s, uiAssetRoot+"/")
	if s == "" || s == "." || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "", false
	}
	clean := path.Clean(s)
	if clean != s || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// ── 辅助 ────────────────────────────────────

// taskView 把任务转为 UI / 工具返回值。
func taskView(t Task) map[string]any {
	v := map[string]any{
		"id":          t.ID,
		"description": t.Description,
		"cron":        t.Cron,
		"next_run_at": t.NextRunAt,
		"enabled":     t.Enabled,
		"type":        t.Action.Type,
		"run_count":   t.RunCount,
		"created_at":  t.CreatedAt,
		"work_dir":    t.CreatedBy.WorkDir,
		// sandbox = 任务级开关（null = 继承插件配置）；effective_sandbox 由 `detail` 动作实时解析。
		"sandbox": t.Sandbox,
	}
	if t.DisabledReason != "" {
		v["disabled_reason"] = t.DisabledReason
	}
	// 会话归属（P4 / C11）：仅执行过才有值（未执行 → 不写键，UI 显示「尚无会话」）。
	if t.SessionID != "" {
		v["session_id"] = t.SessionID
	}
	if t.DialogID != "" {
		v["dialog_id"] = t.DialogID
	}
	if t.LastRun != nil {
		v["last_run"] = t.LastRun
	}
	return v
}

// parseSandboxArg 解析任务级 sandbox 三态参数：
//   - 缺省 / null → (nil, false)（新增 = 继承；更新 = 不修改）；
//   - true / false → (ptr, true)；
//   - "inherit" → (nil, true)（更新时显式恢复「继承插件配置」）。
func parseSandboxArg(v any) (*bool, bool, error) {
	if v == nil {
		return nil, false, nil
	}
	if s, ok := v.(string); ok && strings.EqualFold(strings.TrimSpace(s), "inherit") {
		return nil, true, nil
	}
	b, ok := boolArg(v)
	if !ok {
		return nil, false, errors.New("sandbox 必须是布尔值（true/false）或 \"inherit\"（继承插件配置）")
	}
	return &b, true, nil
}

// newTaskID 生成 `ct_<8 hex>`。
func newTaskID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "ct_" + strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return "ct_" + hex.EncodeToString(b)
}

// allowedWorkDir 判定 work_dir 是否命中配置的目录白名单（空白名单 = 不限）。
func allowedWorkDir(cfg settings, dir string) bool {
	if len(cfg.AllowedWorkDirs) == 0 {
		return true
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return false
	}
	for _, root := range cfg.AllowedWorkDirs {
		if safepath.IsWithin(dir, root) {
			return true
		}
	}
	return false
}

// boolArg 解析布尔参数（bool / "true"/"false"）。
func boolArg(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		if s == "true" || s == "1" || s == "yes" || s == "on" {
			return true, true
		}
		if s == "false" || s == "0" || s == "no" || s == "off" {
			return false, true
		}
	}
	return false, false
}

// intInt 把 JSON 数字转为 int。
func intInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	}
	return 0
}

// formatUnix 把 unix 秒格式化为本地时间（"2006-01-02 15:04:05"）。
func formatUnix(ts int64) string {
	if ts <= 0 {
		return "-"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}
