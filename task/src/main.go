// chonkpilot-task —— 任务与会话编排工具插件（执行 / LLM / 状态 / 终止 / 提问 / 续跑 / 子聊天）。
//
// 工具集：
//   - task_run        执行本机命令 / 脚本（前台、带超时；多 runtime；cwd 缺省取调用上下文的 work_dir）
//   - task_llm        委派全能力子代理（宿主 plugin/v1/agent.delegate）
//   - task_status     查询任务结果 / 任务树（宿主 plugin/v1/task.result + 数据面 task.tree/tasks）
//   - task_stop       终止任务（数据面 task.stop，payload = {task_id,cascade?}）
//   - ask_user        向用户提问（宿主 plugin/v1/ask.user；非阻塞占位返回）
//   - next_run        登记下一轮排队输入（宿主 plugin/v1/next.run）
//   - subchat_open    打开子聊天 / 群聊（宿主 plugin/v1/subchat.open）
//   - subchat_finish  子 agent 自报完成（宿主 plugin/v1/subchat.finish）
//
// 2026-11-08：ask_user / next_run / subchat_open / subchat_finish 由 llm-server 进程内
// `tools/register` 迁入本插件 —— 宿主只保留能力（turn / ask / inbox / 会话生命周期），
// 插件做工具适配（schema + 能力调用）。
//
// 契约源（2026-10-28 P2-3）：本仓 `task/tools/task/*.tool.md`，经 `//go:embed` 打进插件二进制
// （**不**放主仓 `capability/`，也不依赖宿主扫描；本插件自己是 MCP server）。
// 执行实现：`chonkpilot-util/scriptrun`（2026-10-31 由本插件 lib 上收 util）。
//
// 配置（插件自身，不分四级）：默认超时 / 默认工作目录 / shell / 输出上限 / 解释器路径。
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/scriptrun"
)

const pluginVersion = "1.0.0"

// toolsFS 是工具契约（`*.tool.md`）的 embed 源：插件自持，产物不带契约目录。
//
//go:embed all:tools
var toolsFS embed.FS

// toolsRoot 是 embed 内契约目录名（`tools/task/`）。
const toolsRoot = "tools/task"

// 复用层别名：参数取值 / 信封解包统一由 pluginsdk 提供。
var (
	asString = pluginsdk.Str
	toInt    = pluginsdk.Int
	unwrap   = pluginsdk.Unwrap
)

type taskPlugin struct {
	p   *pluginsdk.Plugin
	cfg *configManager
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "task",
		Name:    "任务工具",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("task: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	t := &taskPlugin{p: p, cfg: newConfigManager(p)}

	// 契约 → 工具注册（schema / description / hot 均取自 md；hot 标记沿用 HotTool）。
	contracts, err := loadContracts()
	if err != nil {
		slog.Error("task: 加载工具契约失败", "err", err)
		os.Exit(1)
	}
	if missing := registerTools(p, t, contracts); len(missing) > 0 {
		slog.Error("task: 契约缺少实现", "tools", strings.Join(missing, ","))
		os.Exit(1)
	}

	p.OnRequest("status", t.info)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/task/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := t.cfg.Init(context.Background()); err != nil {
		slog.Warn("task: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range t.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("task: 运行结束", "err", err)
	}
}

// loadContracts 读取 embed 内的契约（`tools/task/*.tool.md`，按名称排序）。
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
// hot 标记沿用既有 HotTool 注册方式（契约 `hot: true` → p.HotTool）。
func registerTools(p *pluginsdk.Plugin, t *taskPlugin, tools []contract.Tool) []string {
	handlers := map[string]pluginsdk.ToolFunc{
		"task_run":       t.run,
		"task_llm":       t.llm,
		"task_status":    t.status,
		"task_stop":      t.stop,
		"ask_user":       t.askUser,
		"next_run":       t.nextRun,
		"subchat_open":   t.subchatOpen,
		"subchat_finish": t.subchatFinish,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			slog.Warn("task: 契约无对应实现，跳过", "tool", tool.Name)
			continue
		}
		p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), fn)
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

// ── task_run ─────────────────────────

// run 执行 task_run：参数归一 → scriptrun 执行 → 结果（形状沿用旧 task_run）。
func (t *taskPlugin) run(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	cfg := t.cfg.Settings()
	cwd := strings.TrimSpace(asString(args["cwd"]))
	if cwd == "" {
		cwd = cfg.DefaultCwd
	}
	if cwd == "" {
		cwd = cx.WorkDir
	}
	// P0（2026-11-08）：cwd 只作为执行目录，并限制在 work_dir 子树内（越界拒绝）；
	// 它不再进入沙箱可写白名单（见 sandboxOptions）。
	cwd, err := resolveCwd(cwd, cx.WorkDir, cx.InstanceID)
	if err != nil {
		return nil, err
	}
	timeout := cfg.DefaultTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 600 {
		timeout = 600
	}
	cfgMaxKB := cfg.MaxOutputKB
	if cfgMaxKB <= 0 {
		cfgMaxKB = 256
	}
	maxKB := cfgMaxKB
	// 2026-11-09：调用级输出上限（可选）。参数夹在 [1, 配置上限]：超配置夹到配置值，
	// 非正数忽略；输出超限时完整 stdout+stderr 落盘到 CaptureDir。
	if n := toInt(args["maxOutputKB"]); n > 0 {
		maxKB = n
		if maxKB > cfgMaxKB {
			maxKB = cfgMaxKB
		}
	}
	// P0（2026-11-08）：requires 安装到项目内依赖目录（work_dir/.chonkpilot/deps），
	// 不再以插件进程权限写全局 site-packages / node_modules；缺少 work_dir 时明确报错。
	requires := stringSlice(args["requires"])
	depsDir := ""
	if len(requires) > 0 {
		var derr error
		depsDir, derr = depsDirFor(cx.WorkDir)
		if derr != nil {
			return nil, derr
		}
	}

	res, err := scriptrun.Run(ctx, scriptrun.Options{
		Runtime:      asString(args["runtime"]),
		Command:      asString(args["command"]),
		File:         asString(args["file"]),
		Cwd:          cwd,
		InstanceID:   cx.InstanceID,
		Requires:     requires,
		DepsDir:      depsDir,
		Interpreter:  asString(args["interpreter"]),
		Env:          envMap(args["env"]),
		Args:         stringSlice(args["args"]),
		Interpreters: cfg.interpreterMap(),
		Timeout:      time.Duration(timeout) * time.Second,
		MaxOutput:    maxKB << 10,
		// 2026-11-09：输出超限时完整 stdout+stderr 落盘（不自动删除）。有 work_dir 用项目内
		// `.chonkpilot/tmp`，否则系统临时目录。
		CaptureDir: captureDirFor(cx.WorkDir),
		// C5（2026-11-03）：进程级写隔离（Low IL + Job），白名单只来自 `_meta.chonkpilot.sandbox`。
		Sandbox: sandboxOptions(cx),
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// depsDirFor 返回 requires 的项目内依赖目录：<work_dir>/.chonkpilot/deps。
// 缺少 work_dir → 明确错误（fail-closed，拒绝安装到全局 site-packages/node_modules）。
func depsDirFor(workDir string) (string, error) {
	wd := strings.TrimSpace(workDir)
	if wd == "" {
		return "", fmt.Errorf("requires 需要会话工作目录（work_dir）以隔离依赖（拒绝安装到全局 site-packages/node_modules）")
	}
	return filepath.Join(wd, ".chonkpilot", "deps"), nil
}

// captureDirFor 返回 task_run 输出超限时的落盘目录（2026-11-09）：
// 有 work_dir → `<work_dir>/.chonkpilot/tmp`；否则系统临时目录。
// 目录仅在输出真的超过阈值时才按需创建（scriptrun 内部 MkdirAll）。
func captureDirFor(workDir string) string {
	if wd := strings.TrimSpace(workDir); wd != "" {
		return filepath.Join(wd, ".chonkpilot", "tmp")
	}
	return os.TempDir()
}

// stringSlice 解析字符串数组参数（容忍 []any / []string；忽略非字符串项与空串）。
func stringSlice(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case []string:
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// envMap 解析 env 参数（值字符串化）。
func envMap(raw any) map[string]string {
	m, ok := raw.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// ── task_llm ─────────────────────────

// llm 委派一个**全能力子代理**（2026-11-04）：完整子会话 / 子轮次 + 全工具面（可按 agent
// 定义收窄），与主轮次同构，可再派生下一级；结果为子代理最终回复。
//
// 插件只做适配：能力在宿主 `plugin/v1/agent.delegate`（进程内唯一 turn 状态与任务层）。
// `waitSec` > 0 → 最多等该秒数；0/缺省 → 等到收尾；`detach=true` → 立即返回 running，
// 之后用 `task_status(task_id=...)` 取结果（任务节点 id 随返回体给出）。
func (t *taskPlugin) llm(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	prompt := strings.TrimSpace(asString(args["prompt"]))
	if prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：task_llm 必须在会话轮次内调用")
	}
	in := map[string]any{
		"instance_id":  cx.InstanceID,
		"turn":         cx.Turn,
		"session":      cx.Session,
		"tool_call_id": cx.ToolCallID,
		"prompt":       prompt,
		"agent":        strings.TrimSpace(asString(args["agent"])),
		"session_id":   strings.TrimSpace(asString(args["session_id"])),
		"title":        strings.TrimSpace(asString(args["title"])),
		"display_name": cx.DisplayName,
	}
	if n := toInt(args["waitSec"]); n > 0 {
		in["wait_sec"] = n
	}
	if d, ok := args["detach"].(bool); ok {
		in["detach"] = d
	}
	var out map[string]any
	// P0（2026-11-08）：delegate 是长等待能力，不能沿用 SDK 缺省 15s 桥超时；按 waitSec
	// 计算本次调用超时（有上限），否则 >15s 的子代理必然返回 context deadline exceeded。
	if err := t.p.CallIntoWithTimeout(ctx, delegateTimeout(toInt(args["waitSec"]), args["detach"] == true),
		"plugin/v1/agent.delegate", in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

const (
	// delegateMaxWait 是 task_llm 单次桥调用的上限（与契约 timeout: 600 对齐）。
	delegateMaxWait = 600 * time.Second
	// delegateWaitBuffer 是 waitSec 到期与桥超时之间的余量：宿主先返回 running，插件后到点。
	delegateWaitBuffer = 15 * time.Second
	// delegateDetachTimeout 是 detach=true 时等待“受理”应答的上限（宿主应立即返回 running）。
	delegateDetachTimeout = 30 * time.Second
)

// delegateTimeout 按 waitSec / detach 计算 `plugin/v1/agent.delegate` 的本次桥调用超时：
//   - detach=true → 30s（只等受理）；
//   - waitSec>0 → waitSec + 15s 余量，封顶 600s；
//   - waitSec<=0 → 600s（“等到收尾”的合理有界默认，仍受上限保护）。
func delegateTimeout(waitSec int, detach bool) time.Duration {
	if detach {
		return delegateDetachTimeout
	}
	if waitSec <= 0 {
		return delegateMaxWait
	}
	t := time.Duration(waitSec)*time.Second + delegateWaitBuffer
	if t > delegateMaxWait {
		return delegateMaxWait
	}
	return t
}

// ── task_status ─────────────────────────

// status 查询任务：`task_id` 非空 → 单任务（可等待 / 附已产出）；空 → 会话任务树 + 列表。
//
// 与 `tool_result` 同口径（2026-11-04）：终态返回结果；仍在运行 → `state=running` +
// `output`（已产出的部分输出）；`timeout` > 0 时先等待至多该秒数。
func (t *taskPlugin) status(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.InstanceID == "" {
		return nil, fmt.Errorf("缺少会话实例（instance_id）")
	}
	if id := strings.TrimSpace(asString(args["task_id"])); id != "" {
		return t.statusOne(ctx, args, cx, id)
	}
	cfg := t.cfg.Settings()
	limit := toInt(args["limit"])
	if limit <= 0 {
		limit = cfg.StatusLimit
	}
	if limit <= 0 {
		limit = 50
	}
	treeRaw, err := t.p.DataRead(ctx, "task", "tree", cx.InstanceID, map[string]any{"session_id": cx.Session})
	if err != nil {
		return nil, fmt.Errorf("读取任务树失败: %w", err)
	}
	tasksRaw, err := t.p.DataRead(ctx, "task", "tasks", cx.InstanceID, map[string]any{"session_id": cx.Session})
	if err != nil {
		return nil, fmt.Errorf("读取任务列表失败: %w", err)
	}
	tree, _, treeTrunc := trimListField(unwrap(treeRaw), "nodes", limit)
	tasks, totalTasks, tasksTrunc := trimListField(unwrap(tasksRaw), "list", limit)
	return map[string]any{
		"ok":        true,
		"limit":     limit,
		"total":     totalTasks,
		"truncated": treeTrunc || tasksTrunc,
		"tree":      tree,
		"tasks":     tasks,
	}, nil
}

// statusOne 查单个任务（任务节点 / gateway 执行 id / 委派子轮次 id 均可）：
// 宿主 `plugin/v1/task.result` 返回权威状态 + 部分输出；timeout 内轮询至终态。
func (t *taskPlugin) statusOne(ctx context.Context, args map[string]any, cx pluginsdk.CallContext, id string) (any, error) {
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：单任务查询必须在会话轮次内调用")
	}
	wait := toInt(args["timeout"])
	var deadline time.Time
	if wait > 0 {
		deadline = time.Now().Add(time.Duration(wait) * time.Second)
	}
	query := func() (string, string, string, bool, error) {
		var out struct {
			Text    string `json:"text"`
			State   string `json:"state"`
			Found   bool   `json:"found"`
			Partial string `json:"partial"`
		}
		err := t.p.CallInto(ctx, "plugin/v1/task.result", map[string]any{
			"instance_id": cx.InstanceID, "turn": cx.Turn, "id": id,
		}, &out)
		return out.Text, out.State, out.Partial, out.Found, err
	}
	for {
		text, state, partial, found, err := query()
		if err != nil {
			return nil, fmt.Errorf("查询任务失败: %w", err)
		}
		if !found {
			return map[string]any{"ok": false, "task_id": id, "state": "not_found",
				"message": "未找到任务 " + id}, nil
		}
		if state != "running" {
			return map[string]any{"ok": true, "task_id": id, "state": state,
				"result": text, "output": partial}, nil
		}
		if wait <= 0 || time.Now().After(deadline) {
			return map[string]any{"ok": true, "task_id": id, "state": "running",
				"message": "任务仍在运行（可加大 timeout 继续等待）", "output": partial}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// trimListField 截断对象形状 `{key:[...]}` 中的数组到 limit 条（P0，2026-11-08）：
// 任务树实际返回 `{nodes:[...]}`、任务列表返回 `{list:[...]}`，旧实现只认 `[]any` →
// limit 永不生效。返回 (截断后的对象, 原始条数, 是否截断)；非对象/非数组原样返回。
func trimListField(raw any, key string, limit int) (any, int, bool) {
	switch v := raw.(type) {
	case map[string]any:
		arr, ok := v[key].([]any)
		if !ok {
			return v, 0, false
		}
		total := len(arr)
		truncated := limit > 0 && total > limit
		if truncated {
			arr = arr[:limit]
		}
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = val
		}
		out[key] = arr
		return out, total, truncated
	case []any:
		// 容错：仍接受数组形状（旧版/测试）。
		total := len(v)
		truncated := limit > 0 && total > limit
		if truncated {
			return v[:limit], total, true
		}
		return v, total, false
	default:
		return raw, 0, false
	}
}

// ── 会话编排工具（2026-11-08 自查户进程内迁入）────────────────────
//
// 宿主只保留能力（turn.context / ask 通道 / inbox / 会话生命周期）；本插件做工具适配。
// 返回体统一是 {text, is_error}（宿主能力直出原结果文本），故用 pluginsdk.Text 原样返回，
// 不做 JSON 编码（保持契约文本与迁移前逐字一致）。

// capText 是宿主 `plugin/v1/*` 会话编排能力的结果信封。
type capText struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

// askUser 把提问交回宿主 ask 通道：广播 session-ask → 立即返回占位结果（非阻塞），
// 用户答复经 ask-user-reply 以同一 turn 续跑。任务节点由 turn 侧建，答复到达才收尾。
func (t *taskPlugin) askUser(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：ask_user 必须在会话轮次内调用")
	}
	if err := validateAskArgs(args); err != nil {
		return nil, err
	}
	var out capText
	if err := t.p.CallInto(ctx, "plugin/v1/ask.user", map[string]any{
		"instance_id":  cx.InstanceID,
		"turn":         cx.Turn,
		"tool_call_id": cx.ToolCallID,
		"args":         args,
	}, &out); err != nil {
		return nil, err
	}
	if out.IsError {
		msg := strings.TrimSpace(out.Text)
		if msg == "" {
			msg = "ask 能力返回错误"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return pluginsdk.Text(out.Text), nil
}

// ask_user 参数上限（与契约 schema 同步）：Ask 卡片是轻量抉择，不是长文输入框。
const (
	askMaxQuestionRunes   = 2000
	askMaxContextRunes    = 4000
	askMaxOptions         = 20
	askMaxOptionTextRunes = 500
	askMaxRecommended     = 20
)

// validateAskArgs 在插件边界校验 ask_user 参数（schema 之外的运行时兜底）：
// question 去空白后非空且不超长；options/recommended 有界且元素形状合法。
// 失败返回 Go error → MCP isError=true（宿主 turn 侧据此落 failed / 任务节点 error）。
func validateAskArgs(args map[string]any) error {
	question := strings.TrimSpace(asString(args["question"]))
	if question == "" {
		return fmt.Errorf("question 不能为空")
	}
	if utf8.RuneCountInString(question) > askMaxQuestionRunes {
		return fmt.Errorf("question 过长（最多 %d 字符）", askMaxQuestionRunes)
	}
	args["question"] = question // 统一去首尾空白后再下发（展示与历史一致）

	if v, ok := args["context"]; ok && v != nil {
		ctxText, ok := v.(string)
		if !ok {
			return fmt.Errorf("context 必须是字符串")
		}
		if utf8.RuneCountInString(ctxText) > askMaxContextRunes {
			return fmt.Errorf("context 过长（最多 %d 字符）", askMaxContextRunes)
		}
	}

	if v, ok := args["options"]; ok && v != nil {
		options, ok := v.([]any)
		if !ok {
			return fmt.Errorf("options 必须是数组")
		}
		if len(options) > askMaxOptions {
			return fmt.Errorf("options 最多 %d 项", askMaxOptions)
		}
		for i, item := range options {
			switch opt := item.(type) {
			case string:
				if strings.TrimSpace(opt) == "" {
					return fmt.Errorf("options[%d] 不能为空字符串", i)
				}
				if utf8.RuneCountInString(opt) > askMaxOptionTextRunes {
					return fmt.Errorf("options[%d] 过长（最多 %d 字符）", i, askMaxOptionTextRunes)
				}
			case map[string]any:
				hasText := false
				valueText, labelText := "", ""
				for _, key := range []string{"value", "label"} {
					raw, exists := opt[key]
					if !exists || raw == nil {
						continue
					}
					text, ok := raw.(string)
					if !ok {
						return fmt.Errorf("options[%d].%s 必须是字符串", i, key)
					}
					switch key {
					case "value":
						valueText = text
					case "label":
						labelText = text
					}
					if strings.TrimSpace(text) != "" {
						hasText = true
					}
					if utf8.RuneCountInString(text) > askMaxOptionTextRunes {
						return fmt.Errorf("options[%d].%s 过长（最多 %d 字符）", i, key, askMaxOptionTextRunes)
					}
				}
				if !hasText {
					return fmt.Errorf("options[%d] 的 value/label 不能同时为空", i)
				}
				// 显式空 value + 非空 label：前端按 value 取值会得到空串 → 归一为 label。
				if strings.TrimSpace(valueText) == "" && strings.TrimSpace(labelText) != "" {
					opt["value"] = labelText
				}
			default:
				return fmt.Errorf("options[%d] 必须是字符串或 {value,label} 对象", i)
			}
		}
	}

	if v, ok := args["recommended"]; ok && v != nil {
		switch rec := v.(type) {
		case string:
			if utf8.RuneCountInString(rec) > askMaxOptionTextRunes {
				return fmt.Errorf("recommended 过长（单个最多 %d 字符）", askMaxOptionTextRunes)
			}
		case []any:
			if len(rec) > askMaxRecommended {
				return fmt.Errorf("recommended 最多 %d 项", askMaxRecommended)
			}
			for i, item := range rec {
				text, ok := item.(string)
				if !ok {
					return fmt.Errorf("recommended[%d] 必须是字符串", i)
				}
				if utf8.RuneCountInString(text) > askMaxOptionTextRunes {
					return fmt.Errorf("recommended[%d] 过长（最多 %d 字符）", i, askMaxOptionTextRunes)
				}
			}
		default:
			return fmt.Errorf("recommended 必须是字符串或字符串数组")
		}
	}
	return nil
}

// nextRun 把下一轮提示词登记为排队输入（NEXTRUN-1）；仅主会话可用，由宿主判定。
func (t *taskPlugin) nextRun(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：next_run 必须在会话轮次内调用")
	}
	var out capText
	if err := t.p.CallInto(ctx, "plugin/v1/next.run", map[string]any{
		"instance_id": cx.InstanceID,
		"turn":        cx.Turn,
		"args":        args,
	}, &out); err != nil {
		return nil, err
	}
	// 业务失败（空/超长 prompt、非主会话、数据门面/计数/inbox 失败）→ 工具错误：
	// worker 落 failed + 任务节点 error；"未启用 / 到限"仍是 isErr=false 的业务文案。
	if out.IsError {
		msg := strings.TrimSpace(out.Text)
		if msg == "" {
			msg = "next_run 登记失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return pluginsdk.Text(out.Text), nil
}

// subchatOpen 建固定参数子聊天 / 群聊；宿主负责会话与首轮落库、开窗事件，插件只转发。
func (t *taskPlugin) subchatOpen(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：subchat_open 必须在会话轮次内调用")
	}
	var out capText
	if err := t.p.CallInto(ctx, "plugin/v1/subchat.open", map[string]any{
		"instance_id":  cx.InstanceID,
		"turn":         cx.Turn,
		"display_name": cx.DisplayName,
		"args":         args,
	}, &out); err != nil {
		return nil, err
	}
	// 参数 / 建会话 / 建首轮等业务失败 → 工具错误（worker 落 failed + 任务节点 error）。
	if out.IsError {
		msg := strings.TrimSpace(out.Text)
		if msg == "" {
			msg = "打开子聊天失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return pluginsdk.Text(out.Text), nil
}

// subchatFinish 子 agent 自报达成 → 会话 lifecycle=ready（建议态；用户拍板才结束）。
func (t *taskPlugin) subchatFinish(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	_ = args
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：subchat_finish 必须在会话轮次内调用")
	}
	var out capText
	if err := t.p.CallInto(ctx, "plugin/v1/subchat.finish", map[string]any{
		"instance_id": cx.InstanceID,
		"turn":        cx.Turn,
	}, &out); err != nil {
		return nil, err
	}
	// 非 sub_chat / 会话不可用 / 置 ready 失败 → 工具错误（worker 落 failed + 任务节点 error）。
	if out.IsError {
		msg := strings.TrimSpace(out.Text)
		if msg == "" {
			msg = "subchat_finish 失败"
		}
		return nil, fmt.Errorf("%s", msg)
	}
	return pluginsdk.Text(out.Text), nil
}

// ── task_stop ─────────────────────────

func (t *taskPlugin) stop(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.InstanceID == "" {
		return nil, fmt.Errorf("缺少会话实例（instance_id）")
	}
	taskID := strings.TrimSpace(asString(args["task_id"]))
	if taskID == "" {
		return nil, fmt.Errorf("task_id 不能为空")
	}
	cascade := true // 缺省保持既有“连带子任务”语义；显式 false = 只停目标任务
	if v, ok := args["cascade"].(bool); ok {
		cascade = v
	}
	// P0（2026-11-08）：改走 `plugin/v1/task.stop`（带 request_id → 宿主返回 v.Result）。
	// 旧 data.write 路径要求 `{req_id,ok,result}` 同主题应答，而 task-stop 成功只写 v.Result
	// → 成功停止被报成 15s/30s 超时。此处成功可在毫秒级拿到 {cancelled, task_id}。
	var out struct {
		Cancelled bool   `json:"cancelled"`
		TaskID    string `json:"task_id"`
	}
	if err := t.p.CallIntoWithTimeout(ctx, 30*time.Second, "plugin/v1/task.stop", map[string]any{
		"request_id":  fmt.Sprintf("task-stop-%d", time.Now().UnixNano()),
		"instance_id": cx.InstanceID,
		"task_id":     taskID,
		"cascade":     cascade,
	}, &out); err != nil {
		return nil, fmt.Errorf("终止任务失败: %w", err)
	}
	if !out.Cancelled {
		return nil, fmt.Errorf("终止任务失败: 任务未被取消（%s）", taskID)
	}
	return map[string]any{
		"ok":        true,
		"cancelled": true,
		"task_id":   taskID,
		"state":     "cancelled",
	}, nil
}

func (t *taskPlugin) info(_ context.Context, _ map[string]any) (any, error) {
	cfg := t.cfg.Settings()
	return map[string]any{
		"ok":                true,
		"version":           pluginVersion,
		"defaultTimeoutSec": cfg.DefaultTimeoutSec,
		"defaultCwd":        cfg.DefaultCwd,
		"shell":             cfg.Shell,
		"maxOutputKB":       cfg.MaxOutputKB,
		"statusLimit":       cfg.StatusLimit,
		"interpreters":      cfg.interpreterMap(),
		"goos":              runtime.GOOS,
	}, nil
}

// 参数取值 / 信封解包见文件头的复用层别名（pluginsdk.Str/Int/Unwrap）。
