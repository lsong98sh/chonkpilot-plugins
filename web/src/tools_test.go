package main

// tools_test.go —— 嵌入的 `*.tool.md` 契约（T2）：名称 / schema / 策略 / _meta.source。

import (
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
)

func TestEmbeddedToolContracts(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("契约数 = %d，期望 3", len(tools))
	}
	byName := make(map[string]contract.Tool, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"web_search", "web_fetch", "web_download"} {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("缺少契约 %s", name)
		}
		if tool.Title == "" || tool.Description == "" {
			t.Errorf("%s: title/description 为空", name)
		}
		if !tool.Hot || tool.Category != "web" {
			t.Errorf("%s: hot/category = %v/%q", name, tool.Hot, tool.Category)
		}
		if tool.Async != "auto" || tool.OnTimeout != "background" || tool.Timeout != 180 {
			t.Errorf("%s: 策略 = async:%s timeout:%d on_timeout:%s", name, tool.Async, tool.Timeout, tool.OnTimeout)
		}
		meta := contract.MetaFor(tool)
		if meta["source"] != "md" {
			t.Errorf("%s: _meta.source = %v，期望 md", name, meta["source"])
		}
		if meta["hot"] != true || meta["category"] != "web" {
			t.Errorf("%s: _meta hot/category = %v/%v", name, meta["hot"], meta["category"])
		}
		if tool.Schema == nil {
			t.Errorf("%s: schema 为空", name)
		}
	}

	// schema 以 md 为准：count / need_content 的 default 必须存在。
	search := byName["web_search"].Schema
	props := search["properties"].(map[string]any)
	count := props["count"].(map[string]any)
	if count["default"] != float64(10) || count["minimum"] != float64(1) || count["maximum"] != float64(50) {
		t.Errorf("web_search.count schema = %#v", count)
	}
	need := props["need_content"].(map[string]any)
	if need["default"] != false {
		t.Errorf("web_search.need_content.default = %v", need["default"])
	}

	download := byName["web_download"].Schema
	dprops := download["properties"].(map[string]any)
	maxBytes := dprops["maxBytes"].(map[string]any)
	if maxBytes["maximum"] != float64(contentHardMaxBytes) {
		t.Errorf("web_download.maxBytes.maximum = %v，期望 %d", maxBytes["maximum"], contentHardMaxBytes)
	}
}
