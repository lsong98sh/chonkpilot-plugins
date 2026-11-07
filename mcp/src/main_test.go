package main

import (
	"reflect"
	"testing"
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

// TestLoadRequest 验证 load 分支选择：skill/prompt → prompts.get；resource → resources.read（uri 兜底）。
func TestLoadRequest(t *testing.T) {
	if method, _ := loadRequest(entry{Kind: "tool", Name: "t"}); method != "" {
		t.Fatalf("tool 不应二次取详情，got method %q", method)
	}

	method, payload := loadRequest(entry{Kind: "skill", Name: "ux-audit"})
	if method != "plugin/v1/prompts.get" || payload["name"] != "ux-audit" {
		t.Fatalf("skill loadRequest = %q %v", method, payload)
	}
	method, payload = loadRequest(entry{Kind: "prompt", Name: "plain"})
	if method != "plugin/v1/prompts.get" || payload["name"] != "plain" {
		t.Fatalf("prompt loadRequest = %q %v", method, payload)
	}

	method, payload = loadRequest(entry{Kind: "resource", Name: "doc", URI: "kb://doc/1"})
	if method != "plugin/v1/resources.read" || payload["uri"] != "kb://doc/1" {
		t.Fatalf("resource loadRequest 应用 uri: %q %v", method, payload)
	}
	method, payload = loadRequest(entry{Kind: "resource", Name: "kb://doc/2"})
	if method != "plugin/v1/resources.read" || payload["uri"] != "kb://doc/2" {
		t.Fatalf("resource loadRequest 应回落 name: %q %v", method, payload)
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
