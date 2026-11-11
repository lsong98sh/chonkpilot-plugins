package main

import (
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
)

// TestEmbeddedToolContracts：9 个 hot 工具的 md 必须齐全，且解析出策略面（source=md）。
func TestEmbeddedToolContracts(t *testing.T) {
	all, err := loadToolContracts()
	if err != nil {
		t.Fatalf("loadToolContracts: %v", err)
	}
	if len(all) != 9 {
		t.Fatalf("契约数量 = %d，want 9", len(all))
	}
	for _, name := range hotTools {
		name := name
		t.Run(name, func(t *testing.T) {
			ct, ok := all[name]
			if !ok {
				t.Fatalf("缺少契约 %s", name)
			}
			if ct.Name != name {
				t.Errorf("name = %q，want %q", ct.Name, name)
			}
			if !ct.Hot {
				t.Errorf("hot 应为 true")
			}
			if ct.Async != "auto" || ct.Timeout != 30 || ct.OnTimeout != "background" {
				t.Errorf("策略 = async:%s timeout:%d on_timeout:%s，want auto/30/background",
					ct.Async, ct.Timeout, ct.OnTimeout)
			}
			if ct.Category != "codegraph" {
				t.Errorf("category = %q，want codegraph", ct.Category)
			}
			if strings.TrimSpace(ct.Description) == "" {
				t.Errorf("摘要为空")
			}
			meta := contract.MetaFor(ct)
			if meta["source"] != "md" {
				t.Errorf("_meta.source = %v，want md", meta["source"])
			}
			if meta["hot"] != true {
				t.Errorf("_meta.hot = %v，want true", meta["hot"])
			}
			if meta["async"] != "auto" || meta["timeout"] != 30 || meta["on_timeout"] != "background" {
				t.Errorf("_meta 策略不完整: %v", meta)
			}
			props, _ := ct.Schema["properties"].(map[string]any)
			if _, ok := props["workdir"]; !ok {
				t.Errorf("schema 缺少可选 workdir 参数")
			}
			if req, ok := ct.Schema["required"].([]any); ok {
				for _, r := range req {
					if r == "workdir" {
						t.Errorf("workdir 不应在 required 中")
					}
				}
			}
		})
	}
}

// TestCodegraphContracts：8 个 codegraph_* 查询契约转引擎注册面（vfts_query 单独注册）。
func TestCodegraphContracts(t *testing.T) {
	all, err := loadToolContracts()
	if err != nil {
		t.Fatalf("loadToolContracts: %v", err)
	}
	got := codegraphContracts(all)
	if len(got) != 8 {
		t.Fatalf("codegraphContracts 数量 = %d，want 8", len(got))
	}
	for _, c := range got {
		if !strings.HasPrefix(c.Name, "codegraph_") {
			t.Errorf("非 codegraph_ 工具被纳入: %s", c.Name)
		}
		if c.Meta["source"] != "md" || c.Meta["hot"] != true {
			t.Errorf("%s 的 _meta 不完整: %v", c.Name, c.Meta)
		}
	}
}
