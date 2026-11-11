package main

// sandbox_test.go —— task_run 进程级写隔离入参组装 + cwd 边界（C5，2026-11-03；P0 2026-11-08）。

import (
	"path/filepath"
	"strings"
	"testing"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func TestSandboxOptions(t *testing.T) {
	// 未启用沙箱（nil / enabled=false）→ 零值（不隔离）。
	if got := sandboxOptions(pluginsdk.CallContext{}); got.Enabled {
		t.Fatalf("nil sandbox 应不隔离: %+v", got)
	}
	if got := sandboxOptions(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: false},
	}); got.Enabled {
		t.Fatalf("enabled=false 应不隔离: %+v", got)
	}

	// 启用：相对目录被跳过（R-11 只接受绝对路径），不触发打标副作用。
	got := sandboxOptions(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{"relative-dir", "  "}},
	})
	if !got.Enabled {
		t.Fatalf("启用沙箱应保留 Enabled: %+v", got)
	}
	if len(got.Writable) != 0 {
		t.Fatalf("相对/空目录应被跳过: %+v", got.Writable)
	}
}

// TestCwdMustBeInsideWorkDir（P0）：cwd 只作为执行目录，必须是 work_dir 子树；
// 任意绝对 cwd 越界 / 缺少 work_dir 一律拒绝。
func TestCwdMustBeInsideWorkDir(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "sub")
	if _, err := resolveCwd("", base, ""); err != nil {
		t.Fatalf("未传 cwd 不应报错: %v", err)
	}
	got, err := resolveCwd(child, base, "")
	if err != nil {
		t.Fatalf("work_dir 子树内 cwd 应通过: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(got), filepath.Clean(child)) {
		t.Fatalf("cwd 归一异常: %q", got)
	}

	outside := t.TempDir()
	if _, err := resolveCwd(outside, base, ""); err == nil {
		t.Fatalf("越界 cwd 应拒绝: %s", outside)
	}
	if _, err := resolveCwd(base, "", ""); err == nil {
		t.Fatal("缺少 work_dir 时应 fail-closed 拒绝 cwd")
	}
}

// TestDepsDirStaysInsideWorkDir（P0）：requires 安装目录必须在项目 work_dir 子树内。
func TestDepsDirStaysInsideWorkDir(t *testing.T) {
	base := t.TempDir()
	got, err := depsDirFor(base)
	if err != nil {
		t.Fatalf("有 work_dir 时不应报错: %v", err)
	}
	if !withinDir(base, got) {
		t.Fatalf("依赖目录越出 work_dir: %q", got)
	}
	if !strings.Contains(got, ".chonkpilot") {
		t.Fatalf("依赖目录应为 work_dir/.chonkpilot/deps: %q", got)
	}
	if _, err := depsDirFor("  "); err == nil {
		t.Fatal("缺少 work_dir 时 requires 应 fail-closed")
	}
}

// TestWithinDir 边界判定（同目录、子目录、前缀混淆、父目录）。
func TestWithinDir(t *testing.T) {
	base := filepath.Join(t.TempDir(), "proj")
	cases := []struct {
		target string
		want   bool
	}{
		{base, true},
		{filepath.Join(base, "a"), true},
		{filepath.Join(base, "a", "b"), true},
		{base + "2", false},
		{filepath.Dir(base), false},
	}
	for _, c := range cases {
		if got := withinDir(base, c.target); got != c.want {
			t.Fatalf("withinDir(%q,%q)=%v want %v", base, c.target, got, c.want)
		}
	}
}
