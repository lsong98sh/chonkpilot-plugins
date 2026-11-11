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
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

//go:embed all:tools
var toolsFS embed.FS

// toolsRoot 是 embed 内契约目录（`tools/mcp/*.tool.md`）。
const toolsRoot = "tools/mcp"

// hostCallTimeout 是单次宿主能力面调用的上限。
// SDK 缺省 15s 会把 mcp_invoke 的同步等待截断（timeoutSec 失效），故放宽到
// invokeTimeoutMax 之上；外层 gateway 仍按 `.tool.md` 的 timeout 裁决。
const hostCallTimeout = time.Duration(invokeTimeoutMax+60) * time.Second

// invokeResultMaxBytes 是 mcp_invoke 单次返回的上限。目标工具/资源的正文可能远大于
// LLM 上下文预算（如 mcp_load 读资源）；超出时截断并标记 truncated/total_bytes，
// 避免一次元调用把上下文打爆。
const invokeResultMaxBytes = 1 << 20

// metaToolTimeout 是 find/load 这类只读元工具的宿主往返上限（多次列表/详情调用共用）。
// 与 `.tool.md` 的 timeout 对齐，避免 SDK 放宽后无界等待。
const metaToolTimeout = 60 * time.Second

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
		Timeout: hostCallTimeout,
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

	// 契约唯一源：embed 的 `tools/mcp/*.tool.md`（schema / async / timeout / hot 均取自契约）。
	tools, err := loadContracts()
	if err != nil {
		slog.Error("mcp: 加载工具契约失败", "err", err)
		os.Exit(1)
	}
	if len(tools) == 0 {
		slog.Error("mcp: 未找到任何契约（tools/mcp/*.tool.md）")
		os.Exit(1)
	}
	if missing := registerTools(p, m, tools); len(missing) > 0 {
		slog.Warn("mcp: 契约缺少对应实现", "tools", missing)
	}

	p.OnRequest("status", m.info)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/mcp/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = m.cfg.Init(initCtx)
	cancel()
	if err != nil {
		slog.Warn("mcp: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range m.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("mcp: 运行结束", "err", err)
	}
}

// loadContracts 读取 embed 内的工具契约（`tools/mcp/*.tool.md`，按名称排序）。
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
func registerTools(p *pluginsdk.Plugin, m *mcpPlugin, tools []contract.Tool) []string {
	handlers := map[string]pluginsdk.ToolFunc{
		"mcp_find":   m.find,
		"mcp_load":   m.load,
		"mcp_invoke": m.invoke,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			slog.Warn("mcp: 契约无对应实现，跳过", "tool", tool.Name)
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

// ── mcp_find ─────────────────────────

func (m *mcpPlugin) find(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, metaToolTimeout)
	defer cancel()

	cfg := m.cfg.Settings()
	kind, err := normalizeKind(asString(args["type"]))
	if err != nil {
		return nil, err
	}
	limit := findLimit(toInt(args["limit"]), cfg.DefaultLimit)
	query := strings.ToLower(strings.TrimSpace(asString(args["query"])))
	matchTerms := splitTerms(asString(args["match"]))

	groups := listGroups(kind)
	byGroup := make(map[string][]entry, len(groups))
	for _, group := range groups {
		switch group {
		case "tool":
			tools, err := m.tools(ctx)
			if err != nil {
				return nil, err
			}
			byGroup[group] = tools
		case "prompt":
			list, err := m.promptEntries(ctx, kind)
			if err != nil {
				return nil, err
			}
			byGroup[group] = list
		case "resource":
			list, err := m.resourceEntries(ctx)
			if err != nil {
				return nil, err
			}
			byGroup[group] = list
		}
	}

	total := 0
	for _, group := range groups {
		list := filterEntries(byGroup[group], query)
		rankEntries(list, matchTerms)
		byGroup[group] = list
		total += len(list)
	}
	out := interleave(groups, byGroup, limit)
	return map[string]any{
		"ok":        true,
		"type":      kind,
		"count":     len(out),
		"total":     total,
		"truncated": total > len(out),
		"entries":   out,
	}, nil
}

// ── mcp_load ─────────────────────────

func (m *mcpPlugin) load(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, metaToolTimeout)
	defer cancel()

	name := strings.TrimSpace(asString(args["name"]))
	if name == "" {
		return nil, errors.New("name 不能为空")
	}
	e, ok, lerr := m.lookupEntry(ctx, name)
	if !ok {
		if lerr != nil {
			return nil, fmt.Errorf("读取条目列表失败: %w", lerr)
		}
		return nil, fmt.Errorf("未找到条目 %q", name)
	}
	if e.Kind == "tool" {
		return mergeLoad(e, "", ""), nil
	}
	method, payload := loadRequest(e, cx.InstanceID)
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
// 加载按名称**全量**查工具（不受检索配置 include-non-hot 影响）：显式点了名却因检索过滤
// 而“未找到”是误导。列表读取失败时继续尝试后面的组；最终未命中则带回首个错误，
// 便于调用方区分「真没有」与「列表读取失败」。
func (m *mcpPlugin) lookupEntry(ctx context.Context, name string) (entry, bool, error) {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	tools, err := m.listTools(ctx, true)
	keep(err)
	for _, e := range tools {
		if e.Name == name {
			return e, true, nil
		}
	}
	prompts, err := m.promptEntries(ctx, "prompt")
	keep(err)
	for _, e := range prompts {
		if e.Name == name {
			return e, true, nil
		}
	}
	resources, err := m.resourceEntries(ctx)
	keep(err)
	for _, e := range resources {
		if e.Name == name || (e.URI != "" && e.URI == name) {
			return e, true, nil
		}
	}
	return entry{}, false, firstErr
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
	name := strings.TrimSpace(asString(args["name"]))
	if name == "" {
		return nil, errors.New("name 不能为空")
	}
	// 禁止自调用（递归 / 绕过场景工具白名单）。只允许 1 层元工具调用。
	if strings.EqualFold(name, "mcp_invoke") {
		return nil, errors.New("禁止 mcp_invoke 自调用（不支持嵌套 mcp_invoke）")
	}
	arguments, err := invokeArguments(args)
	if err != nil {
		return nil, err
	}

	cfg := m.cfg.Settings()
	timeout := cfg.InvokeTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	if timeout < invokeTimeoutMin {
		timeout = invokeTimeoutMin
	}
	if timeout > invokeTimeoutMax {
		timeout = invokeTimeoutMax
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	// `request_id` 让桥走「命令类 + 等待」同步分支（bridge/forwardMQ），拿回目标工具的
	// 真实结果/错误/超时/取消，而不是 `{accepted:true}` 受理回执。
	payload := invokeRequest(name, arguments, cx)
	raw, err := m.p.Call(cctx, "plugin/v1/tools.call", payload)
	if err != nil {
		return nil, err
	}
	return invokeOutcome(name, unwrap(raw))
}

// tools 读取 tools/list 并按插件配置过滤（检索用；加载走 listTools(true)）。
func (m *mcpPlugin) tools(ctx context.Context) ([]entry, error) {
	return m.listTools(ctx, m.cfg.Settings().IncludeNonHot)
}

// listTools 读取 tools/list 并归一化（含 _meta.hot / server）。
func (m *mcpPlugin) listTools(ctx context.Context, includeNonHot bool) ([]entry, error) {
	raw, err := m.p.Call(ctx, "plugin/v1/tools.list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("读取工具列表失败: %w", err)
	}
	return toolsFromResult(unwrap(raw), includeNonHot), nil
}

// promptEntries 读取 prompts/list 并归类为 skill / prompt。
// requested 是请求 type：宿主条目未给 type 时用它兜底（skill 请求 → skill；其余 → prompt）。
func (m *mcpPlugin) promptEntries(ctx context.Context, requested string) ([]entry, error) {
	items, err := m.listItems(ctx, "plugin/v1/prompts.list")
	if err != nil {
		return nil, fmt.Errorf("读取提示列表失败: %w", err)
	}
	out := make([]entry, 0, len(items))
	for _, item := range items {
		if e := entryFromPrompt(item, requested); e.Name != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// resourceEntries 读取 resources/list 并归一化为 resource 条目。
func (m *mcpPlugin) resourceEntries(ctx context.Context) ([]entry, error) {
	items, err := m.listItems(ctx, "plugin/v1/resources.list")
	if err != nil {
		return nil, fmt.Errorf("读取资源列表失败: %w", err)
	}
	out := make([]entry, 0, len(items))
	for _, item := range items {
		if e := entryFromResource(item); e.Name != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

// listItems 读取一组列表能力面并取其中的数组（形状差异容错；读取失败返回错误，
// 不再静默当成 0 条——否则 find 返回空结果、load 误报“未找到”）。
func (m *mcpPlugin) listItems(ctx context.Context, method string) ([]map[string]any, error) {
	raw, err := m.p.Call(ctx, method, map[string]any{})
	if err != nil {
		return nil, err
	}
	return itemsFromResult(unwrap(raw)), nil
}

// toolsFromResult 从 tools/list 结果中提取条目（纯函数，便于单测过滤语义）。
func toolsFromResult(root any, includeNonHot bool) []entry {
	obj, _ := root.(map[string]any)
	list, _ := obj["tools"].([]any)
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
	return out
}

// itemsFromResult 从 prompts/list / resources/list 结果中提取数组（兼容多种键名）。
func itemsFromResult(root any) []map[string]any {
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

// mcp_find 的 limit 边界（与插件配置 default-limit 的 1–200 区间一致）。
const (
	defaultFindLimit = 20
	maxFindLimit     = 200
)

// findLimit 归一 mcp_find 的 limit：<=0 用配置默认；仍非法 → 20；硬顶 200。
func findLimit(n, cfgDefault int) int {
	if n <= 0 {
		n = cfgDefault
	}
	if n <= 0 {
		n = defaultFindLimit
	}
	if n > maxFindLimit {
		n = maxFindLimit
	}
	return n
}

// splitTerms 把检索意图拆成小写词（空白分隔；空串 → nil）。
func splitTerms(raw string) []string {
	return strings.Fields(strings.ToLower(strings.TrimSpace(raw)))
}

// filterEntries 按 query 过滤（名称/描述子串，忽略大小写）；query 空 = 不过滤。
func filterEntries(list []entry, query string) []entry {
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]entry, 0, len(list))
	for _, e := range list {
		if query == "" || strings.Contains(strings.ToLower(e.Name), query) ||
			strings.Contains(strings.ToLower(e.Description), query) {
			out = append(out, e)
		}
	}
	return out
}

// matchScore 计算条目与检索意图的匹配分：命中名称 2 分，命中描述 1 分（按词累加）。
func matchScore(e entry, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	name := strings.ToLower(e.Name)
	desc := strings.ToLower(e.Description)
	score := 0
	for _, term := range terms {
		switch {
		case strings.Contains(name, term):
			score += 2
		case strings.Contains(desc, term):
			score++
		}
	}
	return score
}

// rankEntries 按 match 分数稳定降序重排（同分保持宿主顺序；无意图时不重排）。
func rankEntries(list []entry, terms []string) {
	if len(terms) == 0 || len(list) < 2 {
		return
	}
	sort.SliceStable(list, func(i, j int) bool {
		return matchScore(list[i], terms) > matchScore(list[j], terms)
	})
}

// interleave 按 order 轮转取各组条目，直到 limit；避免 type=all 时工具先占满额度。
func interleave(order []string, groups map[string][]entry, limit int) []entry {
	out := make([]entry, 0, limit)
	if limit <= 0 {
		return out
	}
	for i := 0; len(out) < limit; i++ {
		added := false
		for _, group := range order {
			list := groups[group]
			if i >= len(list) {
				continue
			}
			out = append(out, list[i])
			added = true
			if len(out) >= limit {
				break
			}
		}
		if !added {
			break
		}
	}
	return out
}

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
// skill/prompt/resource 必须携带调用上下文的 instance_id：能力面节点注册在 instance scope，
// 缺它会只在 global scope 查找，导致项目/用户级技能与资源恒 not found。
func loadRequest(e entry, instanceID string) (string, map[string]any) {
	switch e.Kind {
	case "skill", "prompt":
		return "plugin/v1/prompts.get", map[string]any{"name": e.Name, "instance_id": instanceID}
	case "resource":
		uri := e.URI
		if uri == "" {
			uri = e.Name
		}
		return "plugin/v1/resources.read", map[string]any{"uri": uri, "instance_id": instanceID}
	}
	return "", nil
}

// invokeArguments 取目标工具参数：缺省 → {}；显式非 object（含 null）→ 参数错误。
func invokeArguments(args map[string]any) (map[string]any, error) {
	raw, ok := args["arguments"]
	if !ok {
		return map[string]any{}, nil
	}
	if m, ok := raw.(map[string]any); ok {
		return m, nil
	}
	return nil, fmt.Errorf("arguments 必须是 JSON 对象（收到 %T）", raw)
}

// invokeRequest 构造 plugin/v1/tools.call 载荷：同步 request_id + 调用上下文透传。
func invokeRequest(name string, arguments map[string]any, cx pluginsdk.CallContext) map[string]any {
	p := map[string]any{
		"name":        name,
		"arguments":   arguments,
		"work_dir":    cx.WorkDir,
		"instance_id": cx.InstanceID,
		"request_id":  invokeRequestID(cx),
	}
	if cx.Session != "" {
		p["session"] = cx.Session
	}
	if cx.Turn != "" {
		p["turn"] = cx.Turn
	}
	if cx.ToolCallID != "" {
		p["tool_call_id"] = cx.ToolCallID
	}
	if cx.DisplayName != "" {
		p["display_name"] = cx.DisplayName
	}
	if cx.TopSession != "" {
		p["top_session"] = cx.TopSession
	}
	if cx.Parent != "" {
		p["parent"] = cx.Parent
	}
	if cx.DataDir != "" {
		p["data_dir"] = cx.DataDir
	}
	return p
}

// invokeRequestID 生成同步关联用的 request_id（优先复用 LLM tool-call id，便于排查）。
func invokeRequestID(cx pluginsdk.CallContext) string {
	if id := strings.TrimSpace(cx.ToolCallID); id != "" {
		return "mcp-invoke:" + id
	}
	return fmt.Sprintf("mcp-invoke-%d", time.Now().UnixNano())
}

// invokeOutcome 把 gateway 的同步结果转成 mcp_invoke 的返回：
//   - 目标 isError → 外层也返回错误（不伪装成功）；
//   - 有 structuredContent（pending/awaiting 等）→ 透出 status + result；
//   - 否则返回目标的文本结果。
func invokeOutcome(name string, raw any) (any, error) {
	res, ok := raw.(map[string]any)
	if !ok {
		if text := strings.TrimSpace(asString(raw)); text != "" {
			return map[string]any{"ok": true, "tool": name, "status": "complete", "result": truncateInvokeResult(text)}, nil
		}
		return map[string]any{"ok": true, "tool": name, "status": "complete", "result": raw}, nil
	}
	// 防御：桥/宿主若仍按命令类返回受理回执（没有真实结果），不得当成功结果。
	if accepted, _ := res["accepted"].(bool); accepted && res["content"] == nil && res["structuredContent"] == nil {
		return nil, fmt.Errorf("宿主未返回同步结果（仅受理回执）：目标 %s 可能仍在后台执行", name)
	}
	text := resultText(res)
	if isErr, _ := res["isError"].(bool); isErr {
		if text == "" {
			text = fmt.Sprintf("目标工具 %s 返回失败（无错误文本）", name)
		}
		return nil, errors.New(truncateInvokeResult(text))
	}
	out := map[string]any{"ok": true, "tool": name, "status": "complete"}
	if sc, ok := res["structuredContent"].(map[string]any); ok {
		if b, merr := json.Marshal(sc); merr == nil && len(b) > invokeResultMaxBytes {
			out["result"] = truncateInvokeResult(string(b))
			out["truncated"] = true
			out["total_bytes"] = len(b)
		} else {
			out["result"] = sc
		}
		if status := asString(sc["status"]); status != "" {
			out["status"] = status
		}
	} else if text != "" {
		if len(text) > invokeResultMaxBytes {
			out["result"] = truncateInvokeResult(text)
			out["truncated"] = true
			out["total_bytes"] = len(text)
		} else {
			out["result"] = text
		}
	} else {
		out["result"] = res
	}
	return out, nil
}

// truncateInvokeResult 截断到 1MiB（UTF-8 边界）并附恢复提示；字段 truncated/total_bytes
// 由调用方补齐，避免模型把截断内容当完整结果。
func truncateInvokeResult(s string) string {
	if len(s) <= invokeResultMaxBytes {
		return s
	}
	cut := invokeResultMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n…（结果超过 %d 字节，已截断；完整内容请直接调用目标工具或缩小范围）", invokeResultMaxBytes)
}

// resultText 拼接 gateway 结果 content[] 中的文本块（非文本块忽略）。
func resultText(res map[string]any) string {
	list, _ := res["content"].([]any)
	parts := make([]string, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if asString(m["type"]) != "text" {
			continue
		}
		if text := asString(m["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
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
