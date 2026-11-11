package main

// contract_meta_test.go —— crontab 5 个 `.tool.md` 的行为字段必须经 SDK 进入 `_meta`，
// 供 gateway 裁决同步/异步与超时动作（source=md）。

import (
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
)

func TestCrontabContractsMeta(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	want := map[string]bool{
		"crontab_add": true, "crontab_update": true, "crontab_list": true,
		"crontab_toggle": true, "crontab_delete": true,
	}
	if len(tools) != len(want) {
		t.Fatalf("契约数=%d，期望 %d", len(tools), len(want))
	}
	for _, tool := range tools {
		if !want[tool.Name] {
			t.Errorf("未声明工具: %s", tool.Name)
		}
		m := contract.MetaFor(tool)
		if m["source"] != "md" {
			t.Errorf("%s: source=%v，期望 md", tool.Name, m["source"])
		}
		if m["hot"] != true {
			t.Errorf("%s: hot=%v，期望 true", tool.Name, m["hot"])
		}
		if m["async"] != "sync" {
			t.Errorf("%s: async=%v，期望 sync（md async: never）", tool.Name, m["async"])
		}
		if !tool.AsyncSet || !tool.TimeoutSet {
			t.Errorf("%s: async/timeout 未在 md 显式声明（async_set=%v timeout_set=%v）",
				tool.Name, tool.AsyncSet, tool.TimeoutSet)
		}
		if tool.Timeout <= 0 {
			t.Errorf("%s: timeout=%d，同步工具不应为 0/负数", tool.Name, tool.Timeout)
		}
		if m["on_timeout"] != "background" {
			t.Errorf("%s: on_timeout=%v，期望 background", tool.Name, m["on_timeout"])
		}
	}
}
