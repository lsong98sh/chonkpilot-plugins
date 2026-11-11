// chonkpilot-crontab —— 定时任务插件（crontab_add / update / list / toggle / delete）。
//
// 形态：独立 exe（插件桥 UDS + 自建 MCP HTTP）；任务为**本机全局**，不区分项目。
// 动作三选一：`run`（执行命令）/ `notify`（系统通知）/ `agent`（完整智能体，全工具面）。
// 调度：周期 cron 或一次性 at/delay；1s ticker 扫描（单 goroutine），同任务不重入；
// 一次性任务执行一次后自动停用。状态落 <插件数据目录>/tasks.json。
// UI：任务页 / 状态栏徽标 / 配置页（webui，embed）；数字变化经 plugin-event 主动推送，零轮询。
package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
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
	"github.com/lsong98sh/chonkpilot-util/strx"
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
	// 同一宿主桥同时提供 agent 动作（plugin/v1/agent.run）。
	bridge := newBridgeSessions(p)
	run.sessions = bridge
	run.agents = bridge
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
		"crontab_add":    t.add,
		"crontab_update": t.update,
		"crontab_list":   t.list,
		"crontab_toggle": t.toggle,
		"crontab_delete": t.delete,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			continue
		}
		p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), withJSONError(fn))
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

// withJSONError 把 handler 的 Go error 转成 `{"ok":false,"error":"…"}` 文本。
// MCP 仍置 isError=true（SDK 对非 nil error 的统一行为），正文因此同时满足契约的
// 机器可解析字段与调用方的错误信号。
func withJSONError(fn pluginsdk.ToolFunc) pluginsdk.ToolFunc {
	return func(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
		out, err := fn(ctx, args, cx)
		if err == nil {
			return out, nil
		}
		b, _ := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
		return nil, errors.New(string(b))
	}
}

// ── 工具：crontab_add ─────────────────────────

func (t *crontabPlugin) add(_ context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if err := rejectUnknownArgs(args, "cron", "at", "delay", "action", "sandbox"); err != nil {
		return nil, err
	}
	now := time.Now()
	sched, err := parseScheduleInput(args["cron"], args["at"], args["delay"], now)
	if err != nil {
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
	if err := validateWorkDirForAction(cfg, cx.WorkDir, spec.Type); err != nil {
		return nil, err
	}
	action, err := spec.Canonical()
	if err != nil {
		return nil, err
	}
	next := sched.RunAt
	if !sched.Once {
		next, err = nextRun(sched.Cron, now)
		if err != nil {
			return nil, err
		}
	}
	task := Task{
		ID:          newTaskID(),
		Cron:        sched.Cron,
		Once:        sched.Once,
		RunAt:       sched.RunAt,
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
	// 上限判定与写入在同一把锁内，避免并发 add 越过 max-tasks。
	if err := t.store.AddLimited(task, cfg.MaxTasks); err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	if cfg.CreateToast && t.notifier != nil {
		body := fmt.Sprintf("已创建：%s\n%s", task.Description, scheduleLabel(task))
		// 通知走 PowerShell，可能阻塞数秒；必须在落盘成功后异步执行，
		// 否则会拖长 tool 调用并触发 gateway 超时重试（重复建任务）。
		go func() {
			if nerr := t.notifier.Notify("ChonkPilot 定时任务", body, ""); nerr != nil && t.p != nil {
				t.p.Logger().Info("crontab: 创建通知失败", "err", nerr)
			}
		}()
	}
	return map[string]any{"ok": true, "task": taskView(task)}, nil
}

// ── 工具：crontab_update ──────────────────────

func (t *crontabPlugin) update(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	if err := rejectUnknownArgs(args, "task_id", "cron", "at", "delay", "action", "sandbox"); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	existing, ok := t.store.Get(id)
	if !ok {
		return nil, ErrTaskNotFound
	}
	// 显式空串不是「保持不变」：空的调度字段没有合法语义。保持原值请省略该键。
	// null 与省略同义（兼容客户端把未填的可选字段序列化为 null）。
	cronText, hasCron, err := optionalTextArg(args, "cron", false)
	if err != nil {
		return nil, err
	}
	atText, hasAt, err := optionalTextArg(args, "at", true)
	if err != nil {
		return nil, err
	}
	delayText, hasDelay, err := optionalTextArg(args, "delay", false)
	if err != nil {
		return nil, err
	}
	scheduleSet := hasCron || hasAt || hasDelay
	now := time.Now()
	var sched scheduleInput
	if scheduleSet {
		s, err := parseScheduleInput(cronText, atText, delayText, now)
		if err != nil {
			return nil, err
		}
		sched = s
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
	// 动作改到 run/agent 时，复用任务创建时的 work_dir；目录失效则**在写入前**拒绝，
	// 避免把已有任务改成「首次触发即停用」的死任务。
	finalType := existing.Action.Type
	if spec != nil {
		finalType = spec.Type
	}
	if err := validateWorkDirForAction(t.cfg.Settings(), existing.CreatedBy.WorkDir, finalType); err != nil {
		return nil, err
	}
	changed := make([]string, 0, 4)
	if hasCron {
		changed = append(changed, "cron")
	}
	if hasAt {
		changed = append(changed, "at")
	}
	if hasDelay {
		changed = append(changed, "delay")
	}
	if spec != nil {
		changed = append(changed, "action")
	}
	if sandboxSet {
		changed = append(changed, "sandbox")
	}
	// UpdateTx：mutate / 落盘任一失败都回滚内存，避免「报告失败但状态已改」。
	updated, err := t.store.UpdateTx(id, func(cur *Task) error {
		if scheduleSet {
			cur.Cron, cur.Once, cur.RunAt = sched.Cron, sched.Once, sched.RunAt
			// 改期 = 重新武装：一次性任务已执行/错过导致的终态停用，在显式改期后恢复启用。
			if cur.DisabledReason == ReasonOnceCompleted || cur.DisabledReason == ReasonOnceMissed {
				cur.Enabled = true
				cur.DisabledReason = ""
				changed = append(changed, "enabled")
			}
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
		// 只有调度真的变化时才重算 next_run_at；仅改 action/sandbox 不推进到期时间，
		// 避免「任务已到期但尚未被 tick 认领」时丢掉本次触发。停用任务保持 0。
		if !cur.Enabled {
			cur.NextRunAt = 0
		} else if scheduleSet {
			if cur.Once {
				cur.NextRunAt = cur.RunAt
			} else {
				n, err := nextRun(cur.Cron, now)
				if err != nil {
					return err
				}
				cur.NextRunAt = n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	return map[string]any{"ok": true, "changed": changed, "task": taskView(updated)}, nil
}

// ── 工具：crontab_list ────────────────────────

func (t *crontabPlugin) list(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	if err := rejectUnknownArgs(args, "task_id", "enabled", "limit", "offset", "output_tail_bytes", "order"); err != nil {
		return nil, err
	}
	filter, hasFilter, err := optionalBoolArg(args, "enabled")
	if err != nil {
		return nil, err
	}
	taskID, hasTaskID, err := optionalTextArg(args, "task_id", false)
	if err != nil {
		return nil, err
	}
	order, hasOrder, err := optionalTextArg(args, "order", false)
	if err != nil {
		return nil, err
	}
	order = strings.ToLower(order)
	if !hasOrder {
		order = "desc" // 默认新任务优先：limit 截断时最可能隐藏旧任务，而不是刚创建的任务
	}
	if order != "asc" && order != "desc" {
		return nil, errors.New(`order 必须是 "asc" 或 "desc"`)
	}
	limit := 50
	if v, ok, err := optionalIntArg(args, "limit"); err != nil {
		return nil, err
	} else if ok {
		if v < 1 {
			return nil, errors.New("limit 必须 >= 1")
		}
		limit = v
		if limit > 200 {
			limit = 200
		}
	}
	offset := 0
	if v, ok, err := optionalIntArg(args, "offset"); err != nil {
		return nil, err
	} else if ok {
		if v < 0 {
			return nil, errors.New("offset 必须 >= 0")
		}
		offset = v
	}
	// output_tail_bytes 控制 list 返回的 last_run.output_tail 上限（0 = 不返回该字段）。
	tailBytes := 512
	if v, ok, err := optionalIntArg(args, "output_tail_bytes"); err != nil {
		return nil, err
	} else if ok {
		if v < 0 {
			return nil, errors.New("output_tail_bytes 必须 >= 0")
		}
		tailBytes = v
		if tailBytes > outputTailMax {
			tailBytes = outputTailMax
		}
	}

	tasks := t.store.List()
	if order == "desc" {
		for i, j := 0, len(tasks)-1; i < j; i, j = i+1, j-1 {
			tasks[i], tasks[j] = tasks[j], tasks[i]
		}
	}
	views := make([]map[string]any, 0, len(tasks))
	matched := 0
	for _, task := range tasks {
		if hasTaskID && task.ID != taskID {
			continue
		}
		if hasFilter && task.Enabled != filter {
			continue
		}
		matched++
		if matched <= offset {
			continue
		}
		if len(views) < limit {
			views = append(views, taskViewWithTail(task, tailBytes))
		}
	}
	out := map[string]any{
		"ok": true, "tasks": views, "count": len(views),
		"total": matched, "truncated": matched > offset+len(views), "limit": limit,
		"offset": offset, "order": order, "output_tail_bytes": tailBytes,
	}
	if hasTaskID {
		out["task_id"] = taskID
	}
	return out, nil
}

// ── 工具：toggle / delete ─────────────────────

// toggle 启用或停用任务（`enabled=true/false` 必填）。
func (t *crontabPlugin) toggle(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	if err := rejectUnknownArgs(args, "task_id", "enabled"); err != nil {
		return nil, err
	}
	enabled, ok, err := optionalBoolArg(args, "enabled")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("enabled 不能为空（必须为 true 或 false）")
	}
	return t.setEnabled(args, enabled)
}

// setEnabled 是 toggle 的内部实现（UI toggle 也复用）。
// 参数校验由调用方完成（工具入口校验 task_id/enabled；UI 入口自行解析）。
func (t *crontabPlugin) setEnabled(args map[string]any, enabled bool) (any, error) {
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	// 启用前校验 run/agent 的工作目录：目录缺失时直接拒绝，
	// 避免返回 enabled:true 后下一轮又被 work_dir_missing 自动停用。
	if enabled {
		task, ok := t.store.Get(id)
		if !ok {
			return nil, ErrTaskNotFound
		}
		if err := validateWorkDirForAction(t.cfg.Settings(), task.CreatedBy.WorkDir, task.Action.Type); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	changed := false
	updated, err := t.store.UpdateTx(id, func(cur *Task) error {
		oldEnabled, oldNext, oldReason := cur.Enabled, cur.NextRunAt, cur.DisabledReason
		if enabled {
			// 已启用且已有 next_run_at → 保持原值，确保重复 enable 真幂等；
			// 若已到期但尚未被 tick 认领，保留到期时间让本轮照常执行。
			if !cur.Enabled || cur.NextRunAt == 0 {
				if cur.Once {
					if cur.RunAt <= 0 {
						return errors.New("一次性任务缺少有效的 run_at，请先修改执行时间（crontab_update）或新建任务")
					}
					if !cur.Enabled && cur.RunAt <= now.Unix() {
						return fmt.Errorf("一次性任务已过期（run_at=%s），请先修改执行时间（crontab_update）或新建任务",
							formatUnix(cur.RunAt))
					}
					cur.NextRunAt = cur.RunAt
				} else {
					n, err := nextRun(cur.Cron, now)
					if err != nil {
						return err
					}
					cur.NextRunAt = n
				}
			}
			cur.Enabled = true
			cur.DisabledReason = "" // 用户重新启用 → 清掉 work_dir_missing 等自动停用原因
		} else {
			cur.Enabled = false
			cur.NextRunAt = 0
		}
		changed = cur.Enabled != oldEnabled || cur.NextRunAt != oldNext || cur.DisabledReason != oldReason
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	return map[string]any{"ok": true, "task_id": id, "enabled": updated.Enabled, "changed": changed, "running": updated.RunningSince > 0, "task": taskView(updated)}, nil
}

func (t *crontabPlugin) delete(_ context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	if err := rejectUnknownArgs(args, "task_id"); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(asString(args["task_id"]))
	if id == "" {
		return nil, errors.New("task_id 不能为空")
	}
	task, err := t.store.Delete(id)
	if err != nil {
		return nil, err
	}
	t.notifyChanged(context.Background())
	remaining := len(t.store.List())
	return map[string]any{
		"ok": true, "task_id": id, "deleted": true,
		"remaining": remaining, "count": remaining, // count 保留为 remaining 的旧别名
		"running": task.RunningSince > 0, // 删除不中止在飞执行：命令仍会跑完并写 runs.jsonl
	}, nil
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
	if t.p == nil {
		return
	}
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
		"once":        t.Once,
		"next_run_at": t.NextRunAt,
		"enabled":     t.Enabled,
		"type":        t.Action.Type,
		"run_count":   t.RunCount,
		"created_at":  t.CreatedAt,
		"work_dir":    t.CreatedBy.WorkDir,
		// running = 已有一次执行被认领且尚未收口；delete/toggle/update 不会取消它。
		"running": t.RunningSince > 0,
		// sandbox = 任务级开关（null = 继承插件配置）；effective_sandbox 由 `detail` 动作实时解析。
		"sandbox": t.Sandbox,
		// 调度时间是插件进程本地时区；同时给出带偏移的本地时间，避免 LLM 自行换算 epoch。
		"timezone": timezoneLabel(t.NextRunAt, t.RunAt),
	}
	if t.Once {
		v["schedule_type"] = "once"
		v["run_at"] = t.RunAt
		if t.RunAt > 0 {
			v["run_at_local"] = localRFC3339(t.RunAt)
		}
	} else {
		v["schedule_type"] = "cron"
	}
	if t.NextRunAt > 0 {
		v["next_run_at_local"] = localRFC3339(t.NextRunAt)
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

// taskViewWithTail 把任务转为 UI / 工具返回值，并按 tailBytes 截断 last_run.output_tail。
// tailBytes < 0 视为不限；tailBytes == 0 时不返回 output_tail 字段。
func taskViewWithTail(t Task, tailBytes int) map[string]any {
	v := taskView(t)
	if t.LastRun == nil || tailBytes == 0 {
		if v["last_run"] != nil {
			if rec, ok := v["last_run"].(*RunRecord); ok {
				cp := *rec
				cp.OutputTail = ""
				v["last_run"] = cp
			}
		}
		return v
	}
	if rec, ok := v["last_run"].(*RunRecord); ok && tailBytes > 0 {
		cp := *rec
		cp.OutputTail = strx.TruncBytes(cp.OutputTail, tailBytes)
		v["last_run"] = cp
	}
	return v
}

// localRFC3339 把 unix 秒转为本地时区 RFC3339（含偏移）。
func localRFC3339(ts int64) string {
	return time.Unix(ts, 0).In(time.Local).Format(time.RFC3339)
}

// timezoneLabel 返回本地时区缩写（优先取计划时间的 zone，避免 DST 误标）。
func timezoneLabel(ts ...int64) string {
	for _, n := range ts {
		if n > 0 {
			if name, _ := time.Unix(n, 0).In(time.Local).Zone(); name != "" {
				return name
			}
		}
	}
	name, _ := time.Now().In(time.Local).Zone()
	return name
}

// rejectUnknownArgs 拒绝契约未声明的顶层参数（schema additionalProperties=false 的运行时兜底）。
func rejectUnknownArgs(args map[string]any, allowed ...string) error {
	allow := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		allow[k] = true
	}
	var unknown []string
	for k := range args {
		if !allow[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("不支持的参数: %s（允许：%s）", strings.Join(unknown, ", "), strings.Join(allowed, ", "))
}

// optionalTextArg 读取可选的字符串参数（update 的 cron/at/delay）：
//   - 键缺失 / null → 未提供（保持不变）；
//   - 显式空串 / 全空白 → 报错（空值没有合法调度语义，保持原值请省略）；
//   - allowNumber=true 时接受 JSON number（at 的 unix 秒）。
func optionalTextArg(args map[string]any, key string, allowNumber bool) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return "", false, fmt.Errorf("%s 不能为空（保持不变请省略该字段）", key)
		}
		return s, true, nil
	case float64:
		if !allowNumber {
			return "", false, fmt.Errorf("%s 必须是字符串", key)
		}
		return strconv.FormatInt(int64(v), 10), true, nil
	case int:
		if !allowNumber {
			return "", false, fmt.Errorf("%s 必须是字符串", key)
		}
		return strconv.Itoa(v), true, nil
	case int64:
		if !allowNumber {
			return "", false, fmt.Errorf("%s 必须是字符串", key)
		}
		return strconv.FormatInt(v, 10), true, nil
	default:
		if allowNumber {
			return "", false, fmt.Errorf("%s 必须是字符串或 unix 秒整数", key)
		}
		return "", false, fmt.Errorf("%s 必须是字符串", key)
	}
}

// optionalBoolArg 读取可选布尔参数（接受 bool 与 true/false/1/0/yes/no/on/off 字符串）；
// 键存在但值非法 → 报错，不再静默忽略。
func optionalBoolArg(args map[string]any, key string) (bool, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return false, false, nil
	}
	b, valid := boolArg(raw)
	if !valid {
		return false, false, fmt.Errorf("%s 必须是布尔值（true/false）", key)
	}
	return b, true, nil
}

// optionalIntArg 读取可选整数参数（接受 JSON 整数/整值浮点/数字字符串）；
// 键存在但值非法 → 报错，不再静默按默认值处理。
func optionalIntArg(args map[string]any, key string) (int, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case float64:
		if v != float64(int64(v)) {
			return 0, false, fmt.Errorf("%s 必须是整数", key)
		}
		return int(v), true, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false, fmt.Errorf("%s 必须是整数", key)
		}
		return n, true, nil
	default:
		return 0, false, fmt.Errorf("%s 必须是整数", key)
	}
}

// validateWorkDirForAction 校验 run/agent 动作的工作目录：
//   - notify 不需要项目目录（可在无项目的全局会话创建）；
//   - run/agent 必须有存在且可访问的项目目录，且命中配置白名单。
//
// 在写入前拒绝，避免创建「成功但首次触发即 work_dir_missing 停用」的死任务。
func validateWorkDirForAction(cfg settings, dir, actionType string) error {
	if actionType == ActionNotify {
		return nil
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("run/agent 动作需要项目工作目录：当前会话没有 work_dir，请在项目中创建，或改用 notify 动作")
	}
	if !allowedWorkDir(cfg, dir) {
		return fmt.Errorf("work_dir 不在允许的目录白名单内: %s", dir)
	}
	if !workDirExists(dir) {
		return fmt.Errorf("work_dir 不存在或不是目录: %s", dir)
	}
	return nil
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

// scheduleLabel 返回任务的执行计划文案（创建通知 / UI 用）。
func scheduleLabel(t Task) string {
	if t.Once {
		return "一次性执行：" + formatUnix(t.RunAt)
	}
	return "下次执行：" + formatUnix(t.NextRunAt)
}
