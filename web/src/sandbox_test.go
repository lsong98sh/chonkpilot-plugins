package main

// sandbox_test.go —— web_download 的写白名单校验（C6，2026-11-03）。

import (
	"path/filepath"
	"strings"
	"testing"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func TestCheckSandboxWritable(t *testing.T) {
	allowed := t.TempDir()
	denied := t.TempDir()

	// 未启用沙箱（nil / enabled=false）→ 不校验。
	if err := checkSandboxWritable(pluginsdk.CallContext{}, filepath.Join(denied, "x.txt")); err != nil {
		t.Fatalf("未启用沙箱不应限制: %v", err)
	}
	if err := checkSandboxWritable(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: false},
	}, filepath.Join(denied, "x.txt")); err != nil {
		t.Fatalf("enabled=false 不应限制: %v", err)
	}

	cx := pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{
		Enabled: true, WritableDirs: []string{allowed},
	}}
	if err := checkSandboxWritable(cx, filepath.Join(allowed, "ok.txt")); err != nil {
		t.Fatalf("白名单内应通过: %v", err)
	}
	if err := checkSandboxWritable(cx, filepath.Join(allowed, "sub", "ok.txt")); err != nil {
		t.Fatalf("白名单子目录应通过: %v", err)
	}
	err := checkSandboxWritable(cx, filepath.Join(denied, "bad.txt"))
	if err == nil || !strings.Contains(err.Error(), "sandbox:") {
		t.Fatalf("白名单外应失败: %v", err)
	}
}
