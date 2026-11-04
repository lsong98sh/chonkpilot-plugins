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

func (t *taskPlugin) llm(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	prompt := strings.TrimSpace(asString(args["prompt"]))
	if prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	timeout := 120
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	text, err := t.p.LLM(ctx, prompt, strings.TrimSpace(asString(args["system"])), time.Duration(timeout)*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": text}, nil
}

// ── task_status ─────────────────────────

func (t *taskPlugin) status(ctx context.Context, _ map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.InstanceID == "" {
		return nil, fmt.Errorf("缺少会话实例（instance_id）")
	}
	cfg := t.cfg.Settings()
	limit := cfg.StatusLimit
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
		"tasks": unwrap(tasksRaw),
	}, nil
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
