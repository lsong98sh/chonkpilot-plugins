// chonkpilot-mcp —— MCP 工具系列插件（检索 / 加载 / 调用）。
//
// 三个工具（全部走宿主能力面，不直连 gateway 内部）：
//   - mcp_find   在工具 / 提示（技能）/ 资源中检索（query 本地匹配）
//   - mcp_load   按名称加载一个条目的完整定义（工具 = 输入 schema）
//   - mcp_invoke 调用一个工具（plugin/v1/tools.call，参数 {name,arguments}）
//
// 配置（插件自身，不分四级）：默认返回条数 / 是否包含非 hot 工具 / 默认调用超时。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

// 复用层别名：参数取值 / 信封解包统一由 pluginsdk 提供。
var (
	asString = pluginsdk.Str
	toInt    = pluginsdk.Int
	unwrap   = pluginsdk.Unwrap
)

type mcpPlugin struct {
	p   *pluginsdk.Plugin
	cfg *configManager
}

// entry 是检索结果条目。
type entry struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Description string         `json:"description,omitempty"`
	Server      string         `json:"server,omitempty"`
	Hot         bool           `json:"hot,omitempty"`
	Schema      map[string]any `json:"input_schema,omitempty"`
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "mcp",
		Name:    "MCP 工具",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("mcp: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	m := &mcpPlugin{p: p, cfg: newConfigManager(p)}
	p.Tool("mcp_find", "在可用工具 / 提示（技能）/ 资源中检索；query 为空则列出该类型全部（受 limit 限制）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "关键词（名称/描述包含匹配，忽略大小写）"},
				"type":  map[string]any{"type": "string", "description": "tool（默认）/prompt/resource/all"},
				"limit": map[string]any{"type": "integer", "description": "最多返回条数（缺省用配置）"},
			},
		}, m.find)

	p.Tool("mcp_load", "按名称加载一个条目的完整定义（工具返回 input_schema）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "条目名称（工具名 / 提示名 / 资源 uri）"},
			},
			"required": []string{"name"},
		}, m.load)

	p.Tool("mcp_invoke", "按名称调用一个工具并返回其结果（参数 = 工具参数对象）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "description": "工具名（LLM 可见名）"},
				"arguments":  map[string]any{"type": "object", "description": "工具参数"},
				"timeoutSec": map[string]any{"type": "integer", "description": "超时秒数（缺省用配置）"},
			},
			"required": []string{"name"},
		}, m.invoke)

	for _, n := range []string{"mcp_find", "mcp_load", "mcp_invoke"} {
		p.HotTool(n)
	}
	p.OnRequest("status", m.info)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/mcp/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := m.cfg.Init(context.Background()); err != nil {
		slog.Warn("mcp: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range m.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("mcp: 运行结束", "err", err)
	}
}

// ── mcp_find ─────────────────────────

func (m *mcpPlugin) find(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := m.cfg.Settings()
	kind := strings.ToLower(strings.TrimSpace(asString(args["type"])))
	if kind == "" {
		kind = "tool"
	}
	if kind != "tool" && kind != "prompt" && kind != "resource" && kind != "all" {
		return nil, fmt.Errorf("type 只允许 tool/prompt/resource/all（收到 %q）", kind)
	}
	limit := cfg.DefaultLimit
	if n := toInt(args["limit"]); n > 0 {
		limit = n
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	query := strings.ToLower(strings.TrimSpace(asString(args["query"])))

	all := make([]entry, 0, 64)
	if kind == "tool" || kind == "all" {
		tools, err := m.tools(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, tools...)
	}
	if kind == "prompt" || kind == "all" {
		all = append(all, m.simpleList(ctx, "plugin/v1/prompts.list", "prompt")...)
	}
	if kind == "resource" || kind == "all" {
		all = append(all, m.simpleList(ctx, "plugin/v1/resources.list", "resource")...)
	}

	out := make([]entry, 0, limit)
	for _, e := range all {
		if query != "" && !strings.Contains(strings.ToLower(e.Name), query) &&
			!strings.Contains(strings.ToLower(e.Description), query) {
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return map[string]any{"ok": true, "type": kind, "count": len(out), "entries": out}, nil
}

// ── mcp_load ─────────────────────────

func (m *mcpPlugin) load(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	name := strings.TrimSpace(asString(args["name"]))
	if name == "" {
		return nil, errors.New("name 不能为空")
	}
	all := make([]entry, 0, 64)
	tools, err := m.tools(ctx)
	if err == nil {
		all = append(all, tools...)
	}
	all = append(all, m.simpleList(ctx, "plugin/v1/prompts.list", "prompt")...)
	all = append(all, m.simpleList(ctx, "plugin/v1/resources.list", "resource")...)
	for _, e := range all {
		if e.Name == name {
			return map[string]any{"ok": true, "entry": e}, nil
		}
	}
	return nil, fmt.Errorf("未找到条目 %q", name)
}

// ── mcp_invoke ─────────────────────────

func (m *mcpPlugin) invoke(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := m.cfg.Settings()
	name := strings.TrimSpace(asString(args["name"]))
	if name == "" {
		return nil, errors.New("name 不能为空")
	}
	arguments, _ := args["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	timeout := cfg.InvokeTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	if timeout < 5 {
		timeout = 5
	}
	if timeout > 1800 {
		timeout = 1800
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	raw, err := m.p.Call(cctx, "plugin/v1/tools.call", map[string]any{
		"name": name, "arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "tool": name, "result": unwrap(raw)}, nil
}

// tools 读取 tools/list 并归一化（含 _meta.hot / server）。
func (m *mcpPlugin) tools(ctx context.Context) ([]entry, error) {
	raw, err := m.p.Call(ctx, "plugin/v1/tools.list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("读取工具列表失败: %w", err)
	}
	root := unwrap(raw)
	obj, _ := root.(map[string]any)
	list, _ := obj["tools"].([]any)
	includeNonHot := m.cfg.Settings().IncludeNonHot
	out := make([]entry, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		e := entry{
			Name:        asString(t["name"]),
			Kind:        "tool",
			Description: asString(t["description"]),
		}
		if s, ok := t["inputSchema"].(map[string]any); ok {
			e.Schema = s
		}
		if meta, ok := t["_meta"].(map[string]any); ok {
			e.Hot, _ = meta["hot"].(bool)
			if srv, ok := meta["server"].(map[string]any); ok {
				e.Server = asString(srv["alias"])
				if e.Server == "" {
					e.Server = asString(srv["node"])
				}
			}
		}
		if !e.Hot && !includeNonHot {
			continue
		}
		if e.Name != "" {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// simpleList 读取 prompts/resources 列表（形状差异全部容错）。
func (m *mcpPlugin) simpleList(ctx context.Context, method, kind string) []entry {
	raw, err := m.p.Call(ctx, method, map[string]any{})
	if err != nil {
		return nil
	}
	root := unwrap(raw)
	obj, _ := root.(map[string]any)
	var list []any
	for _, key := range []string{"items", "prompts", "resources", "entries"} {
		if v, ok := obj[key].([]any); ok {
			list = v
			break
		}
	}
	out := make([]entry, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := asString(t["name"])
		if name == "" {
			name = asString(t["uri"])
		}
		if name == "" {
			continue
		}
		out = append(out, entry{Name: name, Kind: kind, Description: asString(t["description"])})
	}
	return out
}

func (m *mcpPlugin) info(_ context.Context, _ map[string]any) (any, error) {
	cfg := m.cfg.Settings()
	return map[string]any{
		"ok":               true,
		"version":          pluginVersion,
		"defaultLimit":     cfg.DefaultLimit,
		"includeNonHot":    cfg.IncludeNonHot,
		"invokeTimeoutSec": cfg.InvokeTimeoutSec,
	}, nil
}

// 参数取值 / 信封解包见文件头的复用层别名（pluginsdk.Str/Int/Unwrap）。
