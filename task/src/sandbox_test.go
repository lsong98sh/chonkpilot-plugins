package main

// sandbox_test.go —— task_run 进程级写隔离入参组装（C5，2026-11-03）。

import (
	"testing"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func TestSandboxOptions(t *testing.T) {
	// 未启用沙箱（nil / enabled=false）→ 零值（不隔离）。
	if got := sandboxOptions(pluginsdk.CallContext{}, ""); got.Enabled {
		t.Fatalf("nil sandbox 应不隔离: %+v", got)
	}
	if got := sandboxOptions(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: false},
	}, ""); got.Enabled {
		t.Fatalf("enabled=false 应不隔离: %+v", got)
	}

	// 启用：相对目录被跳过（R-11 只接受绝对路径），不触发打标副作用。
	got := sandboxOptions(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{"relative-dir", "  "}},
	}, "")
	if !got.Enabled {
		t.Fatalf("启用沙箱应保留 Enabled: %+v", got)
	}
	if len(got.Writable) != 0 {
		t.Fatalf("相对/空目录应被跳过: %+v", got.Writable)
	}
}
