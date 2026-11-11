package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// TestNormalizeKind 收敛 mcp_find 的 type：skill 是 prompt 的别名，未知值明确报错。
func TestNormalizeKind(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "tool", false},
		{"tool", "tool", false},
		{"skill", "skill", false},
		{"SKILL", "skill", false},
		{" prompt ", "prompt", false},
		{"resource", "resource", false},
		{"all", "all", false},
		{"agent", "", true},
		{"kb", "", true},
	}
	for _, c := range cases {
		got, err := normalizeKind(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizeKind(%q) 期望报错，实际 %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeKind(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeKind(%q) = %q，want %q", c.in, got, c.want)
		}
	}
}

// TestListGroups 验证 type → 宿主列表组选择（skill/prompt 都查 prompt 列表）。
func TestListGroups(t *testing.T) {
	cases := []struct {
		kind string
		want []string
	}{
		{"tool", []string{"tool"}},
		{"skill", []string{"prompt"}},
		{"prompt", []string{"prompt"}},
		{"resource", []string{"resource"}},
		{"all", []string{"tool", "prompt", "resource"}},
	}
	for _, c := range cases {
		if got := listGroups(c.kind); !reflect.DeepEqual(got, c.want) {
			t.Errorf("listGroups(%q) = %v，want %v", c.kind, got, c.want)
		}
	}
}

// TestEntryFromTool 验证 schema / hot / server 归一与非 hot 过滤。
func TestEntryFromTool(t *testing.T) {
	schema := map[string]any{"type": "object"}
	raw := map[string]any{
		"name":        "filesys_run",
		"description": "文件操作",
		"inputSchema": schema,
		"_meta": map[string]any{
			"hot":    true,
			"server": map[string]any{"alias": "filesys", "node": "dir-node"},
		},
	}
	e, ok := entryFromTool(raw, false)
	if !ok {
		t.Fatal("hot 工具应被收录")
	}
	if e.Name != "filesys_run" || e.Kind != "tool" || e.Type != "tool" {
		t.Fatalf("entry = %+v", e)
	}
	if !e.Hot || e.Server != "filesys" {
		t.Fatalf("meta 归一失败: %+v", e)
	}
	if !reflect.DeepEqual(e.Schema, schema) {
		t.Fatalf("schema = %v，want %v", e.Schema, schema)
	}

	nonHot := map[string]any{"name": "cold_tool", "_meta": map[string]any{"hot": false}}
	if _, ok := entryFromTool(nonHot, false); ok {
		t.Fatal("非 hot 工具在 includeNonHot=false 时应被丢弃")
	}
	if e, ok := entryFromTool(nonHot, true); !ok || e.Name != "cold_tool" {
		t.Fatalf("includeNonHot=true 应收录非 hot 工具: ok=%v e=%+v", ok, e)
	}
}

// TestEntryFromPrompt skill 条目 kind/type 必须为 skill；宿主缺 type 时用请求类型兜底。
func TestEntryFromPrompt(t *testing.T) {
	skill := entryFromPrompt(map[string]any{
		"name": "ux-audit", "description": "审计", "type": "skill",
	}, "prompt")
	if skill.Kind != "skill" || skill.Type != "skill" {
		t.Fatalf("宿主 type=skill 应归为 skill: %+v", skill)
	}

	prompt := entryFromPrompt(map[string]any{"name": "plain", "type": "prompt"}, "skill")
	if prompt.Kind != "prompt" || prompt.Type != "prompt" {
		t.Fatalf("宿主 type=prompt 应归为 prompt: %+v", prompt)
	}

	fallbackSkill := entryFromPrompt(map[string]any{"name": "no-type"}, "skill")
	if fallbackSkill.Kind != "skill" || fallbackSkill.Type != "skill" {
		t.Fatalf("缺 type + 请求 skill 应兜底 skill: %+v", fallbackSkill)
	}

	fallbackAll := entryFromPrompt(map[string]any{"name": "no-type"}, "all")
	if fallbackAll.Kind != "prompt" || fallbackAll.Type != "prompt" {
		t.Fatalf("缺 type + 请求 all 应兜底 prompt: %+v", fallbackAll)
	}

	unknown := entryFromPrompt(map[string]any{"name": "weird", "type": "agent"}, "prompt")
	if unknown.Kind != "prompt" {
		t.Fatalf("未知宿主 type 应按请求类型兜底 prompt: %+v", unknown)
	}
}

// TestEntryFromResource 验证 name/uri 回落与 uri 保留（resources.read 需要 uri）。
func TestEntryFromResource(t *testing.T) {
	e := entryFromResource(map[string]any{
		"uri": "kb://doc/1", "description": "文档", "mimeType": "text/markdown",
	})
	if e.Name != "kb://doc/1" || e.URI != "kb://doc/1" {
		t.Fatalf("name 应回落 uri: %+v", e)
	}
	if e.Kind != "resource" || e.Type != "resource" || e.MimeType != "text/markdown" {
		t.Fatalf("resource 归一无误: %+v", e)
	}

	named := entryFromResource(map[string]any{"uri": "kb://doc/2", "name": "doc-two"})
	if named.Name != "doc-two" || named.URI != "kb://doc/2" {
		t.Fatalf("name 与 uri 应分别保留: %+v", named)
	}
}

// TestLoadRequest 验证 load 分支选择：skill/prompt → prompts.get；resource → resources.read（uri 兜底）；
// 且 instance 作用域条目必须携带 instance_id。
func TestLoadRequest(t *testing.T) {
	if method, _ := loadRequest(entry{Kind: "tool", Name: "t"}, "win-1"); method != "" {
		t.Fatalf("tool 不应二次取详情，got method %q", method)
	}

	method, payload := loadRequest(entry{Kind: "skill", Name: "ux-audit"}, "win-1")
	if method != "plugin/v1/prompts.get" || payload["name"] != "ux-audit" || payload["instance_id"] != "win-1" {
		t.Fatalf("skill loadRequest = %q %v", method, payload)
	}
	method, payload = loadRequest(entry{Kind: "prompt", Name: "plain"}, "win-1")
	if method != "plugin/v1/prompts.get" || payload["name"] != "plain" || payload["instance_id"] != "win-1" {
		t.Fatalf("prompt loadRequest = %q %v", method, payload)
	}

	method, payload = loadRequest(entry{Kind: "resource", Name: "doc", URI: "kb://doc/1"}, "win-1")
	if method != "plugin/v1/resources.read" || payload["uri"] != "kb://doc/1" || payload["instance_id"] != "win-1" {
		t.Fatalf("resource loadRequest 应用 uri + instance_id: %q %v", method, payload)
	}
	method, payload = loadRequest(entry{Kind: "resource", Name: "kb://doc/2"}, "")
	if method != "plugin/v1/resources.read" || payload["uri"] != "kb://doc/2" || payload["instance_id"] != "" {
		t.Fatalf("resource loadRequest 应回落 name 并保留空 instance_id: %q %v", method, payload)
	}
}

// TestMergeLoad 验证三个分支的返回字段。
func TestMergeLoad(t *testing.T) {
	tool := mergeLoad(entry{
		Name: "filesys_run", Kind: "tool", Type: "tool",
		Description: "文件操作", Schema: map[string]any{"type": "object"},
	}, "", "")
	if tool["kind"] != "tool" || tool["input_schema"] == nil {
		t.Fatalf("tool 应返回 input_schema: %v", tool)
	}
	if _, ok := tool["content"]; ok {
		t.Fatalf("tool 不应返回 content: %v", tool)
	}

	skill := mergeLoad(entry{Name: "ux-audit", Kind: "skill", Type: "skill", Description: "审计"}, "正文", "")
	if skill["kind"] != "skill" || skill["content"] != "正文" || skill["text"] != "正文" {
		t.Fatalf("skill 应返回 content/text: %v", skill)
	}

	res := mergeLoad(entry{Name: "doc", Kind: "resource", Type: "resource", URI: "kb://doc/1"}, "内容", "text/markdown")
	if res["kind"] != "resource" || res["content"] != "内容" || res["mimeType"] != "text/markdown" {
		t.Fatalf("resource 应返回 content/mimeType: %v", res)
	}
	if res["uri"] != "kb://doc/1" {
		t.Fatalf("resource 应保留 uri: %v", res)
	}
}

// TestFindLimit 验证 mcp_find 的 limit 默认值与硬顶。
func TestFindLimit(t *testing.T) {
	cases := []struct{ in, cfg, want int }{
		{0, 20, 20},  // 未传 → 配置默认
		{-5, 20, 20}, // 非法 → 配置默认
		{0, 0, 20},   // 配置也非法 → 兜底 20
		{1, 20, 1},
		{200, 20, 200},
		{201, 20, 200}, // 硬顶
		{5000, 50, 200},
	}
	for _, c := range cases {
		if got := findLimit(c.in, c.cfg); got != c.want {
			t.Errorf("findLimit(%d,%d) = %d，want %d", c.in, c.cfg, got, c.want)
		}
	}
}

// TestFilterAndRank 验证 query 硬过滤 + match 加权排序（名称优先于描述、同分保持原序）。
func TestFilterAndRank(t *testing.T) {
	list := []entry{
		{Name: "alpha", Description: "无关"},
		{Name: "beta", Description: "代码审查"},
		{Name: "gamma", Description: "代码生成"},
		{Name: "file_read", Description: "读取文件"},
	}
	if got := filterEntries(list, "read"); len(got) != 1 || got[0].Name != "file_read" {
		t.Fatalf("query 过滤失败: %+v", got)
	}
	if got := filterEntries(list, "READ"); len(got) != 1 || got[0].Name != "file_read" {
		t.Fatalf("query 应忽略大小写: %+v", got)
	}
	if got := filterEntries(list, ""); len(got) != len(list) {
		t.Fatalf("空 query 不应过滤: %+v", got)
	}

	ranked := append([]entry(nil), list...)
	rankEntries(ranked, splitTerms("代码"))
	if ranked[0].Name != "beta" || ranked[1].Name != "gamma" {
		t.Fatalf("match 应按命中排序: %+v", ranked)
	}
	// 名称命中权重高于描述。
	ranked = []entry{
		{Name: "x", Description: "代码"},
		{Name: "代码工具", Description: "无"},
	}
	rankEntries(ranked, splitTerms("代码"))
	if ranked[0].Name != "代码工具" {
		t.Fatalf("名称命中应优先: %+v", ranked)
	}
}

// TestInterleave 验证 type=all 时各组轮转取用，避免工具先占满 limit。
func TestInterleave(t *testing.T) {
	groups := map[string][]entry{
		"tool":     {{Name: "t1"}, {Name: "t2"}, {Name: "t3"}},
		"prompt":   {{Name: "p1"}, {Name: "p2"}},
		"resource": {{Name: "r1"}},
	}
	got := interleave([]string{"tool", "prompt", "resource"}, groups, 4)
	want := []string{"t1", "p1", "r1", "t2"}
	if len(got) != len(want) {
		t.Fatalf("interleave = %+v，want %v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i] {
			t.Fatalf("interleave[%d] = %q，want %q（全部：%+v）", i, got[i].Name, want[i], got)
		}
	}

	all := interleave([]string{"tool", "prompt", "resource"}, groups, 10)
	if len(all) != 6 {
		t.Fatalf("limit 大于总数时应取全: %+v", all)
	}
}

// TestInvokeArguments 验证 arguments 类型：缺省 {}；非 object（含 null）报错。
func TestInvokeArguments(t *testing.T) {
	if got, err := invokeArguments(map[string]any{}); err != nil || len(got) != 0 {
		t.Fatalf("缺省 arguments 应为空对象: %v %v", got, err)
	}
	if got, err := invokeArguments(map[string]any{"arguments": map[string]any{"path": "a"}}); err != nil || got["path"] != "a" {
		t.Fatalf("object arguments 应原样返回: %v %v", got, err)
	}
	for _, bad := range []any{"str", []any{1}, nil} {
		if _, err := invokeArguments(map[string]any{"arguments": bad}); err == nil {
			t.Errorf("arguments=%v 应报参数错误", bad)
		}
	}
}

// TestInvokeRejectsSelf：mcp_invoke 自调用必须直接拒绝（无需真实桥）。
func TestInvokeRejectsSelf(t *testing.T) {
	m := &mcpPlugin{}
	if _, err := m.invoke(context.Background(), map[string]any{"name": " mcp_invoke "}, pluginsdk.CallContext{}); err == nil {
		t.Fatal("mcp_invoke 自调用应被拒绝")
	}
	if _, err := m.invoke(context.Background(), map[string]any{"name": ""}, pluginsdk.CallContext{}); err == nil {
		t.Fatal("空 name 应报错")
	}
}

// TestInvokeRequest 验证同步 request_id 与调用上下文透传。
func TestInvokeRequest(t *testing.T) {
	cx := pluginsdk.CallContext{
		InstanceID: "win-1", WorkDir: "C:/proj", DataDir: "C:/data",
		Session: "s1", Turn: "turn-2", ToolCallID: "tc-9", TopSession: "top",
		Parent: "p-1", DisplayName: "读取配置",
	}
	p := invokeRequest("file_read", map[string]any{"path": "a.txt"}, cx)
	if p["name"] != "file_read" || p["work_dir"] != "C:/proj" {
		t.Fatalf("基础字段错误: %+v", p)
	}
	if p["request_id"] != "mcp-invoke:tc-9" {
		t.Fatalf("request_id 必须由 tool_call_id 派生（触发同步分支）: %+v", p)
	}
	for _, k := range []string{"instance_id", "session", "turn", "tool_call_id", "display_name", "top_session", "parent", "data_dir"} {
		if p[k] == nil || p[k] == "" {
			t.Errorf("上下文字段 %s 未透传: %+v", k, p)
		}
	}
	if args, _ := p["arguments"].(map[string]any); args["path"] != "a.txt" {
		t.Fatalf("arguments 未透传: %+v", p)
	}
	// 无 tool_call_id 时仍须有非空 request_id。
	p = invokeRequest("x", map[string]any{}, pluginsdk.CallContext{})
	if strings.TrimSpace(pluginsdk.Str(p["request_id"])) == "" {
		t.Fatalf("无 tool_call_id 时也要生成 request_id: %+v", p)
	}
}

// TestInvokeOutcome 验证目标结果/失败/异步状态的转换。
func TestInvokeOutcome(t *testing.T) {
	// 同步成功（文本）。
	ok := map[string]any{
		"resultType": "complete",
		"content":    []any{map[string]any{"type": "text", "text": "hello"}},
	}
	got, err := invokeOutcome("file_read", ok)
	if err != nil {
		t.Fatalf("成功结果不应报错: %v", err)
	}
	out, _ := got.(map[string]any)
	if out["ok"] != true || out["tool"] != "file_read" || out["status"] != "complete" || out["result"] != "hello" {
		t.Fatalf("成功结果转换错误: %+v", out)
	}

	// 目标失败 → 外层必须 isError（error 非 nil），且带目标错误文本。
	failed := map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": "permission denied"}},
	}
	if _, err := invokeOutcome("file_write", failed); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("目标 isError 应透传为外层错误: %v", err)
	}

	// 异步目标 → status=pending + task_id。
	pending := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": "任务已转后台执行"}},
		"structuredContent": map[string]any{"status": "pending", "task_id": "ex-1"},
	}
	got, err = invokeOutcome("task_run", pending)
	if err != nil {
		t.Fatalf("pending 不应报错: %v", err)
	}
	out, _ = got.(map[string]any)
	if out["status"] != "pending" {
		t.Fatalf("异步目标应返回 pending: %+v", out)
	}
	if sc, _ := out["result"].(map[string]any); sc["task_id"] != "ex-1" {
		t.Fatalf("task_id 应透出: %+v", out)
	}

	// 非对象结果 → 文本。
	got, err = invokeOutcome("x", "raw")
	if err != nil {
		t.Fatalf("非对象结果不应报错: %v", err)
	}
	out, _ = got.(map[string]any)
	if out["result"] != "raw" {
		t.Fatalf("非对象结果应按文本返回: %+v", out)
	}

	// 受理回执（无真实结果）不得当成功。
	ack := map[string]any{"accepted": true, "topic": "mcp-tools-call"}
	if _, err := invokeOutcome("file_read", ack); err == nil {
		t.Fatal("{accepted:true} 受理回执必须报错，不能当作结果")
	}
}

// TestToolsFromResultFiltering 验证 include-non-hot 只影响检索；加载走全量。
func TestToolsFromResultFiltering(t *testing.T) {
	root := map[string]any{"tools": []any{
		map[string]any{"name": "hot_tool", "_meta": map[string]any{"hot": true}},
		map[string]any{"name": "cold_tool", "_meta": map[string]any{"hot": false}},
		map[string]any{"name": ""},
	}}
	filtered := toolsFromResult(root, false)
	if len(filtered) != 1 || filtered[0].Name != "hot_tool" {
		t.Fatalf("检索过滤应只留 hot 工具: %+v", filtered)
	}
	all := toolsFromResult(root, true)
	if len(all) != 2 || all[0].Name != "cold_tool" || all[1].Name != "hot_tool" {
		t.Fatalf("加载全量应含非 hot 且按名排序: %+v", all)
	}
	if got := toolsFromResult(nil, true); len(got) != 0 {
		t.Fatalf("空结果应为空: %+v", got)
	}
}

// TestItemsFromResult 验证 prompts/resources 列表键名兼容。
func TestItemsFromResult(t *testing.T) {
	for _, key := range []string{"items", "prompts", "resources", "entries"} {
		root := map[string]any{key: []any{map[string]any{"name": "x"}, "bad"}}
		got := itemsFromResult(root)
		if len(got) != 1 || got[0]["name"] != "x" {
			t.Errorf("键 %s 提取错误: %+v", key, got)
		}
	}
	if got := itemsFromResult(nil); len(got) != 0 {
		t.Fatalf("空结果应为空: %+v", got)
	}
}

// TestInvokeOutcomeTruncation 验证 1MiB 上限：超限截断并标记，未超限不动。
func TestInvokeOutcomeTruncation(t *testing.T) {
	big := strings.Repeat("a", invokeResultMaxBytes+10)
	got, err := invokeOutcome("x", map[string]any{
		"content": []any{map[string]any{"type": "text", "text": big}},
	})
	if err != nil {
		t.Fatalf("超限结果不应报错: %v", err)
	}
	out, _ := got.(map[string]any)
	if out["truncated"] != true || out["total_bytes"] != len(big) {
		t.Fatalf("超限结果应标记 truncated/total_bytes: %+v", out)
	}
	text, _ := out["result"].(string)
	if !strings.Contains(text, "已截断") || len(text) > invokeResultMaxBytes+200 {
		t.Fatalf("截断文本异常: len=%d", len(text))
	}

	// 多字节边界：截断点必须是完整 UTF-8。
	zh := strings.Repeat("中", invokeResultMaxBytes/3+10)
	got, err = invokeOutcome("x", map[string]any{
		"content": []any{map[string]any{"type": "text", "text": zh}},
	})
	if err != nil {
		t.Fatalf("多字节超限不应报错: %v", err)
	}
	out, _ = got.(map[string]any)
	if text, _ := out["result"].(string); !utf8.ValidString(text) {
		t.Fatal("截断结果必须是合法 UTF-8")
	}

	// 结构化结果超限 → JSON 文本截断。
	got, err = invokeOutcome("x", map[string]any{
		"structuredContent": map[string]any{"blob": big, "status": "pending"},
	})
	if err != nil {
		t.Fatalf("结构化超限不应报错: %v", err)
	}
	out, _ = got.(map[string]any)
	if out["truncated"] != true || out["status"] != "pending" {
		t.Fatalf("结构化超限应保留 status 并标记截断: %+v", out)
	}

	// 未超限：不产生 truncated 字段。
	got, _ = invokeOutcome("x", map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "small"}},
	})
	out, _ = got.(map[string]any)
	if _, ok := out["truncated"]; ok {
		t.Fatalf("未超限不应有 truncated: %+v", out)
	}
}
