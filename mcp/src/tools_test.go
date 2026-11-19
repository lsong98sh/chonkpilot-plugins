package main

import (
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
)

// TestContractsCoverAllTools 校验 embed 的 `.tool.md` 契约：
// 3 个元工具齐全、均为 hot/meta、策略字段来自 md、schema 与实现对齐。
func TestContractsCoverAllTools(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("应有 3 个契约，实际 %d: %+v", len(tools), tools)
	}
	byName := make(map[string]contract.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"mcp_find", "mcp_load", "mcp_invoke"} {
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
		// 打点声明：mcp_find 只读 → false；mcp_load/mcp_invoke 可加载/调用任意外部工具 → true（保守）。
		wantTouch := name != "mcp_find"
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
		if v, ok := meta["touch_files"].(bool); !ok || v != (name != "mcp_find") {
			t.Errorf("%s _meta.touch_files = %v（存在=%v）", name, meta["touch_files"], ok)
		}
	}

	// mcp_find 新增 match 参数；limit 有上限/默认说明。
	findProps := schemaProps(t, byName["mcp_find"])
	if _, ok := findProps["match"]; !ok {
		t.Error("mcp_find schema 缺少 match 参数")
	}
	if _, ok := findProps["query"]; !ok {
		t.Error("mcp_find schema 缺少 query 参数")
	}
	if limit, _ := findProps["limit"].(map[string]any); limit["maximum"] != float64(200) && limit["maximum"] != 200 {
		t.Errorf("mcp_find limit 上限未在 schema 声明: %+v", findProps["limit"])
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
