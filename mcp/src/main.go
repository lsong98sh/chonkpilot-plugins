// chonkpilot-mcp —— MCP 工具系列插件（检索 / 加载 / 调用）。
//
// 三个工具（全部走宿主能力面，不直连 gateway 内部）：
//   - mcp_find   在工具 / 技能（提示）/ 资源中检索（query 本地匹配；type=skill 是 prompt 别名）
//   - mcp_load   按名称加载一个条目的完整定义（tool = input_schema；skill/prompt = 正文；resource = content）
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

// entry 是检索结果条目。kind 是资产类别（tool/skill/prompt/resource）；
// type 与 kind 同值（T2 契约要求条目至少含 name/kind/type/description）。
type entry struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Type        string         `json:"type"`
	Description string         `json:"description"`
	Server      string         `json:"server,omitempty"`
	Hot         bool           `json:"hot,omitempty"`
	URI         string         `json:"uri,omitempty"`
	MimeType    string         `json:"mimeType,omitempty"`
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
	p.Tool("mcp_find", "在可用工具 / 技能（提示）/ 资源中检索；query 为空则列出该类型全部（受 limit 限制）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "关键词（名称/描述包含匹配，忽略大小写）"},
				"type":  map[string]any{"type": "string", "description": "tool（默认）/skill/prompt/resource/all（skill 与 prompt 同义）"},
				"limit": map[string]any{"type": "integer", "description": "最多返回条数（缺省用配置）"},
			},
		}, m.find)

	p.Tool("mcp_load", "按名称加载一个条目的完整定义：tool 返回 input_schema；skill/prompt 返回正文 content；resource 返回 content。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "条目名称（工具名 / 技能或提示名 / 资源名或 uri）"},
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
	kind, err := normalizeKind(asString(args["type"]))
	if err != nil {
		return nil, err
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
	for _, group := range listGroups(kind) {
		switch group {
		case "tool":
			tools, err := m.tools(ctx)
			if err != nil {
				return nil, err
			}
			all = append(all, tools...)
		case "prompt":
			all = append(all, m.promptEntries(ctx, kind)...)
		case "resource":
			all = append(all, m.resourceEntries(ctx)...)
		}
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
	e, ok := m.lookupEntry(ctx, name)
	if !ok {
		return nil, fmt.Errorf("未找到条目 %q", name)
	}
	if e.Kind == "tool" {
		return mergeLoad(e, "", ""), nil
	}
	method, payload := loadRequest(e)
	if method == "" {
		return nil, fmt.Errorf("不支持加载条目 %q（kind=%s）", name, e.Kind)
	}
	detail, err := m.callDetail(ctx, method, payload)
	if err != nil {
		return nil, fmt.Errorf("读取 %s %q 内容失败: %w", e.Kind, name, err)
	}
	content := asString(detail["content"])
	mime := asString(detail["mimeType"])
	if mime == "" {
		mime = asString(detail["mimetype"])
	}
	return mergeLoad(e, content, mime), nil
}

// lookupEntry 按名称查找条目：工具 → 提示/技能 → 资源（同旧行为，tool 优先）。
// 列表读取失败按「该组无结果」处理，最终未命中时返回明确错误。
func (m *mcpPlugin) lookupEntry(ctx context.Context, name string) (entry, bool) {
	if tools, err := m.tools(ctx); err == nil {
		for _, e := range tools {
			if e.Name == name {
				return e, true
			}
		}
	}
	for _, e := range m.promptEntries(ctx, "prompt") {
		if e.Name == name {
			return e, true
		}
	}
	for _, e := range m.resourceEntries(ctx) {
		if e.Name == name || (e.URI != "" && e.URI == name) {
			return e, true
		}
	}
	return entry{}, false
}

// callDetail 调宿主详情能力面并返回结果对象（信封已解包）。
func (m *mcpPlugin) callDetail(ctx context.Context, method string, payload map[string]any) (map[string]any, error) {
	raw, err := m.p.Call(ctx, method, payload)
	if err != nil {
		return nil, err
	}
	obj, ok := unwrap(raw).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("宿主 %s 返回非对象结果", method)
	}
	return obj, nil
}

// ── mcp_invoke ─────────────────────────

func (m *mcpPlugin) invoke(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
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
	// 宿主准入（49 §2.19）三条件之一要求 `work_dir`：取调用上下文（承载窗口的会话工作目录），
	// 缺它会被判 `not_a_chonkpilot_project` 而拒绝。
	raw, err := m.p.Call(cctx, "plugin/v1/tools.call", map[string]any{
		"name": name, "arguments": arguments, "work_dir": cx.WorkDir,
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
		if e, ok := entryFromTool(t, includeNonHot); ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// promptEntries 读取 prompts/list 并归类为 skill / prompt。
// requested 是请求 type：宿主条目未给 type 时用它兜底（skill 请求 → skill；其余 → prompt）。
func (m *mcpPlugin) promptEntries(ctx context.Context, requested string) []entry {
	items := m.listItems(ctx, "plugin/v1/prompts.list")
	out := make([]entry, 0, len(items))
	for _, item := range items {
		if e := entryFromPrompt(item, requested); e.Name != "" {
			out = append(out, e)
		}
	}
	return out
}

// resourceEntries 读取 resources/list 并归一化为 resource 条目。
func (m *mcpPlugin) resourceEntries(ctx context.Context) []entry {
	items := m.listItems(ctx, "plugin/v1/resources.list")
	out := make([]entry, 0, len(items))
	for _, item := range items {
		if e := entryFromResource(item); e.Name != "" {
			out = append(out, e)
		}
	}
	return out
}

// listItems 读取一组列表能力面并取其中的数组（形状差异全部容错）。
func (m *mcpPlugin) listItems(ctx context.Context, method string) []map[string]any {
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
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if t, ok := item.(map[string]any); ok {
			out = append(out, t)
		}
	}
	return out
}

// ── 纯逻辑（可单测，不依赖真实桥）──────────────────────────────────

// normalizeKind 归一 mcp_find 的 type：空 = tool；skill 是 prompt 的别名（两者都保留）。
func normalizeKind(raw string) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(raw))
	switch kind {
	case "":
		return "tool", nil
	case "tool", "skill", "prompt", "resource", "all":
		return kind, nil
	}
	return "", fmt.Errorf("type 只允许 tool/skill/prompt/resource/all（收到 %q）", raw)
}

// listGroups 返回某 type 需查询的宿主列表组（skill/prompt 都落到 prompt 列表）。
func listGroups(kind string) []string {
	switch kind {
	case "tool", "resource":
		return []string{kind}
	case "skill", "prompt":
		return []string{"prompt"}
	case "all":
		return []string{"tool", "prompt", "resource"}
	}
	return nil
}

// entryFromTool 归一 tools/list 条目；非 hot 且未开启 includeNonHot 时丢弃。
func entryFromTool(t map[string]any, includeNonHot bool) (entry, bool) {
	e := entry{
		Name:        asString(t["name"]),
		Kind:        "tool",
		Type:        "tool",
		Description: asString(t["description"]),
	}
	if s, ok := t["inputSchema"].(map[string]any); ok {
		e.Schema = s
	} else if s, ok := t["input_schema"].(map[string]any); ok {
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
	if e.Name == "" {
		return entry{}, false
	}
	if !e.Hot && !includeNonHot {
		return entry{}, false
	}
	return e, true
}

// entryFromPrompt 归一 prompts/list 条目：宿主 type=skill → skill，type=prompt → prompt；
// 宿主未给 type（或其他值）时用请求值兜底（skill 请求 → skill，其余 → prompt）。
func entryFromPrompt(t map[string]any, requested string) entry {
	kind := strings.ToLower(strings.TrimSpace(asString(t["type"])))
	if kind != "skill" && kind != "prompt" {
		kind = requested
		if kind != "skill" {
			kind = "prompt"
		}
	}
	return entry{
		Name:        asString(t["name"]),
		Kind:        kind,
		Type:        kind,
		Description: asString(t["description"]),
	}
}

// entryFromResource 归一 resources/list 条目（name 缺失时回落 uri；保留 uri 供 read）。
func entryFromResource(t map[string]any) entry {
	uri := asString(t["uri"])
	name := asString(t["name"])
	if name == "" {
		name = uri
	}
	return entry{
		Name:        name,
		Kind:        "resource",
		Type:        "resource",
		Description: asString(t["description"]),
		URI:         uri,
		MimeType:    asString(t["mimeType"]),
	}
}

// loadRequest 返回加载条目需二次调用的宿主方法与载荷（tool 无需二次读取 → method 为空）。
func loadRequest(e entry) (string, map[string]any) {
	switch e.Kind {
	case "skill", "prompt":
		return "plugin/v1/prompts.get", map[string]any{"name": e.Name}
	case "resource":
		uri := e.URI
		if uri == "" {
			uri = e.Name
		}
		return "plugin/v1/resources.read", map[string]any{"uri": uri}
	}
	return "", nil
}

// mergeLoad 组装 mcp_load 返回：tool → input_schema；skill/prompt/resource → content（+ text 兼容）。
func mergeLoad(e entry, content, mimeType string) map[string]any {
	if e.Type == "" {
		e.Type = e.Kind
	}
	out := map[string]any{
		"ok":    true,
		"kind":  e.Kind,
		"type":  e.Type,
		"name":  e.Name,
		"entry": e,
	}
	if e.Description != "" {
		out["description"] = e.Description
	}
	if e.Server != "" {
		out["server"] = e.Server
	}
	switch e.Kind {
	case "tool":
		out["input_schema"] = e.Schema
	case "skill", "prompt":
		out["content"] = content
		out["text"] = content
	case "resource":
		if e.URI != "" {
			out["uri"] = e.URI
		}
		out["content"] = content
		out["text"] = content
		if mimeType != "" {
			out["mimeType"] = mimeType
		}
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
