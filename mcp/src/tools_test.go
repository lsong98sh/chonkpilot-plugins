package main

import (
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
)

// TestContractsCoverAllTools 校验 embed 的 `.tool.md` 契约：
// 4 个元工具齐全（find/match/load/invoke）、均为 hot/meta、策略字段来自 md、schema 与实现对齐。
func TestContractsCoverAllTools(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	if len(tools) != 4 {
		t.Fatalf("应有 4 个契约，实际 %d: %+v", len(tools), tools)
	}
	byName := make(map[string]contract.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"mcp_find", "mcp_match", "mcp_load", "mcp_invoke"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("缺少契约 %s", name)
		}
		if !tool.Hot || !tool.HotSet {
			t.Errorf("%s 应显式 hot: true", name)
		}
		if tool.Category != "meta" {
			t.Errorf("%s category = %q，want meta", name, tool.Category)
		}
		if tool.Async != "sync" || !tool.AsyncSet {
			t.Errorf("%s async = %q（set=%v），want sync", name, tool.Async, tool.AsyncSet)
		}
		if name == "mcp_invoke" {
			// mcp_invoke 不设自身超时（0 = 无上限）：等待/超时由目标工具的契约决定。
			if tool.Timeout != 0 || !tool.TimeoutSet {
				t.Errorf("%s timeout = %d（set=%v），want 0（无自身超时）", name, tool.Timeout, tool.TimeoutSet)
			}
		} else {
			if tool.Timeout <= 0 || !tool.TimeoutSet {
				t.Errorf("%s timeout = %d（set=%v），want >0", name, tool.Timeout, tool.TimeoutSet)
			}
		}
		// 打点声明：mcp_find / mcp_match 只读 → false；mcp_load/mcp_invoke 可加载/调用任意外部工具 → true（保守）。
		wantTouch := name != "mcp_find" && name != "mcp_match"
		if !tool.TouchFilesSet || tool.TouchFiles != wantTouch {
			t.Errorf("%s touch_files = %v（set=%v），want %v", name, tool.TouchFiles, tool.TouchFilesSet, wantTouch)
		}
		if tool.Description == "" || tool.Description == tool.Title {
			t.Errorf("%s 摘要为空或回退为 title: %q", name, tool.Description)
		}
		if tool.Schema == nil {
			t.Errorf("%s 缺少参数 schema", name)
		}
		meta := contract.MetaFor(tool)
		if meta["source"] != "md" {
			t.Errorf("%s _meta.source = %v，want md", name, meta["source"])
		}
		if meta["hot"] != true {
			t.Errorf("%s _meta.hot = %v，want true", name, meta["hot"])
		}
		if name == "mcp_invoke" && meta["policy_lock"] != true {
			t.Errorf("%s 应 policy_lock=true（不允许界面/CLI/调用级覆盖超时）", name)
		}
		if v, ok := meta["touch_files"].(bool); !ok || v != wantTouch {
			t.Errorf("%s _meta.touch_files = %v（存在=%v）", name, meta["touch_files"], ok)
		}
	}

	// mcp_find：query / match / limit（语义检索参数面不变；limit 有上限说明）。
	findProps := schemaProps(t, byName["mcp_find"])
	for _, p := range []string{"query", "match", "limit"} {
		if _, ok := findProps[p]; !ok {
			t.Errorf("mcp_find schema 缺少 %s 参数", p)
		}
	}
	if limit, _ := findProps["limit"].(map[string]any); limit["maximum"] != float64(200) && limit["maximum"] != 200 {
		t.Errorf("mcp_find limit 上限未在 schema 声明: %+v", findProps["limit"])
	}

	// mcp_match：表达式检索参数面（query / type / limit；不引入 match）。
	matchProps := schemaProps(t, byName["mcp_match"])
	for _, p := range []string{"query", "type", "limit"} {
		if _, ok := matchProps[p]; !ok {
			t.Errorf("mcp_match schema 缺少 %s 参数", p)
		}
	}
	if _, ok := matchProps["match"]; ok {
		t.Error("mcp_match 不应有 match 参数（表达式检索单输入）")
	}
	if limit, _ := matchProps["limit"].(map[string]any); limit["maximum"] != float64(200) && limit["maximum"] != 200 {
		t.Errorf("mcp_match limit 上限未在 schema 声明: %+v", matchProps["limit"])
	}

	// mcp_load / mcp_invoke 的 required 必须含 name。
	for _, name := range []string{"mcp_load", "mcp_invoke"} {
		required, _ := byName[name].Schema["required"].([]any)
		if len(required) != 1 || required[0] != "name" {
			t.Errorf("%s schema.required = %v，want [name]", name, required)
		}
	}
	if props := schemaProps(t, byName["mcp_invoke"]); props["arguments"] == nil {
		t.Error("mcp_invoke schema 缺少 arguments 参数")
	}
	if props := schemaProps(t, byName["mcp_invoke"]); props["timeoutSec"] != nil {
		t.Error("mcp_invoke 不应再有 timeoutSec（超时由目标工具契约决定）")
	}
}

// schemaProps 取契约 schema 的 properties（缺失 → 空 map）。
func schemaProps(t *testing.T, tool contract.Tool) map[string]any {
	t.Helper()
	props, ok := tool.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s schema.properties 形状异常: %+v", tool.Name, tool.Schema)
	}
	return props
}
