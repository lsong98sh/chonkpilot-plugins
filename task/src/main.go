// chonkpilot-task —— 任务系列工具插件（执行 / LLM / 状态 / 终止）。
//
// 四个工具（工具名保持不变）：
//   - task_run    执行本机命令 / 脚本（前台、带超时；多 runtime；cwd 缺省取调用上下文的 work_dir）
//   - task_llm    单次 LLM 调用（宿主 plugin/v1/llm.step，插件拿不到 apiKey）
//   - task_status 读取任务树 / 任务列表（数据面 task.tree / task.tasks）
//   - task_stop   终止任务（数据面 task.stop，payload = {task_id,cascade?}）
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
	"runtime"
	"sort"
	"strings"
	"time"

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
		"task_run":    t.run,
		"task_llm":    t.llm,
		"task_status": t.status,
		"task_stop":   t.stop,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			slog.Warn("task: 契约无对应实现，跳过", "tool", tool.Name)
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
	maxKB := cfg.MaxOutputKB
	if maxKB <= 0 {
		maxKB = 256
	}

	res, err := scriptrun.Run(ctx, scriptrun.Options{
		Runtime:      asString(args["runtime"]),
		Command:      asString(args["command"]),
		File:         asString(args["file"]),
		Cwd:          cwd,
		InstanceID:   cx.InstanceID,
		Requires:     stringSlice(args["requires"]),
		Interpreter:  asString(args["interpreter"]),
		Env:          envMap(args["env"]),
		Args:         stringSlice(args["args"]),
		Interpreters: cfg.interpreterMap(),
		Timeout:      time.Duration(timeout) * time.Second,
		MaxOutput:    maxKB << 10,
		// C5（2026-11-03）：进程级写隔离（Low IL + Job），白名单来自 `_meta.chonkpilot.sandbox`。
		Sandbox: sandboxOptions(cx, cwd),
	})
	if err != nil {
		return nil, err
	}
	return res, nil
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
		"purpose":      cx.Purpose,
	}
	if n := toInt(args["waitSec"]); n > 0 {
		in["wait_sec"] = n
	}
	if d, ok := args["detach"].(bool); ok {
		in["detach"] = d
	}
	var out map[string]any
	if err := t.p.CallInto(ctx, "plugin/v1/agent.delegate", in, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ── task_status ─────────────────────────

// status 查询任务：`task_id` 非空 → 单任务（可等待 / 附已产出）；空 → 会话任务树 + 列表。
//
// 与 `tool_result` 同口径（2026-11-04）：终态返回结果；仍在运行 → `state=running` +
// `output`（已产出的部分输出）；`waitSec` > 0 时先等待至多该秒数。
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
	return map[string]any{
		"ok":    true,
		"limit": limit,
		"tree":  unwrap(treeRaw),
		"tasks": trimTasks(unwrap(tasksRaw), limit),
	}, nil
}

// statusOne 查单个任务（任务节点 / gateway 执行 id / 委派子轮次 id 均可）：
// 宿主 `plugin/v1/task.result` 返回权威状态 + 部分输出；waitSec 内轮询至终态。
func (t *taskPlugin) statusOne(ctx context.Context, args map[string]any, cx pluginsdk.CallContext, id string) (any, error) {
	if cx.Turn == "" {
		return nil, fmt.Errorf("缺少会话轮次（turn）：单任务查询必须在会话轮次内调用")
	}
	wait := toInt(args["waitSec"])
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
				"message": "任务仍在运行（可加大 waitSec 继续等待）", "output": partial}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// trimTasks 截断任务列表到 limit 条（缺省 / 非数组 → 原样返回）。
func trimTasks(raw any, limit int) any {
	list, ok := raw.([]any)
	if !ok || limit <= 0 || len(list) <= limit {
		return raw
	}
	return list[:limit]
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
	params := map[string]any{"task_id": taskID}
	if v, ok := args["cascade"].(bool); ok {
		params["cascade"] = v
	}
	raw, err := t.p.DataWrite(ctx, "task", "stop", cx.InstanceID, params)
	if err != nil {
		return nil, fmt.Errorf("终止任务失败: %w", err)
	}
	return map[string]any{"ok": true, "result": unwrap(raw)}, nil
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
