package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// ─── 内嵌契约 ────────────────────────────────────────────────────

func TestEmbeddedContract(t *testing.T) {
	tool, err := loadDesktopContract()
	if err != nil {
		t.Fatalf("loadDesktopContract: %v", err)
	}
	if tool.Name != "desktop_run" {
		t.Fatalf("name = %q", tool.Name)
	}
	if tool.Title != "桌面操作" {
		t.Errorf("title = %q", tool.Title)
	}
	if !tool.Hot || !tool.HotSet {
		t.Errorf("hot = %v(set=%v)，期望 true", tool.Hot, tool.HotSet)
	}
	if tool.Async != "auto" || !tool.AsyncSet {
		t.Errorf("async = %q(set=%v)，期望 auto", tool.Async, tool.AsyncSet)
	}
	if tool.Timeout != 60 || !tool.TimeoutSet {
		t.Errorf("timeout = %d(set=%v)，期望 60", tool.Timeout, tool.TimeoutSet)
	}
	if tool.OnTimeout != "background" || !tool.OnTimeoutSet {
		t.Errorf("on_timeout = %q(set=%v)，期望 background", tool.OnTimeout, tool.OnTimeoutSet)
	}
	if tool.Category != "desktop" {
		t.Errorf("category = %q", tool.Category)
	}
	meta := contract.MetaFor(tool)
	if meta["source"] != "md" {
		t.Errorf("_meta.source = %v，期望 md", meta["source"])
	}
	if meta["hot"] != true || meta["async"] != "auto" || meta["timeout"] != 60 || meta["on_timeout"] != "background" {
		t.Errorf("_meta 行为字段不符：%v", meta)
	}
	if meta["category"] != "desktop" {
		t.Errorf("_meta.category = %v", meta["category"])
	}
}

func TestEmbeddedContractSchema(t *testing.T) {
	tool, err := loadDesktopContract()
	if err != nil {
		t.Fatalf("loadDesktopContract: %v", err)
	}
	props, ok := tool.Schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties 缺失：%v", tool.Schema)
	}
	required, _ := tool.Schema["required"].([]any)
	if len(required) != 1 || required[0] != "action" {
		t.Errorf("required = %v，期望 [action]", required)
	}

	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]any)
	got := make(map[string]bool, len(enum))
	for _, v := range enum {
		if s, ok := v.(string); ok {
			got[s] = true
		}
	}
	for _, want := range []string{
		"start", "stop", "list_windows", "find_window", "window_info", "focus",
		"click", "input", "type", "key", "screenshot", "shot", "move", "size",
		"resize", "minimize", "maximize", "restore", "ping", "status",
	} {
		if !got[want] {
			t.Errorf("action enum 缺少 %q", want)
		}
	}

	args, _ := props["args"].(map[string]any)
	argProps, _ := args["properties"].(map[string]any)
	for _, want := range []string{
		"hwnd", "title", "class", "x", "y", "w", "h", "button", "text", "keys",
		"path", "full_screen", "visible_only", "limit",
	} {
		if _, ok := argProps[want]; !ok {
			t.Errorf("args.properties 缺少目标/动作参数 %q", want)
		}
	}
	// 目标字段必须出现在 schema 中，否则模型无法按描述调用。
	for _, target := range []string{"hwnd", "title", "class"} {
		if desc, _ := argProps[target].(map[string]any)["description"].(string); strings.TrimSpace(desc) == "" {
			t.Errorf("args.%s 缺少 description", target)
		}
	}
}

// ─── 目标窗口 / start meta ───────────────────────────────────────

func TestRequiresTarget(t *testing.T) {
	for _, action := range []string{
		"window_info", "focus", "click", "input", "type", "key",
		"move", "size", "resize", "minimize", "maximize", "restore",
	} {
		if !requiresTarget(action) {
			t.Errorf("requiresTarget(%q) = false，期望 true", action)
		}
	}
	for _, action := range []string{
		"start", "stop", "list_windows", "find_window", "screenshot", "shot", "ping", "status",
	} {
		if requiresTarget(action) {
			t.Errorf("requiresTarget(%q) = true，期望 false", action)
		}
	}
}

func TestTargetMeta(t *testing.T) {
	got := targetMeta(
		map[string]any{"title": " 记事本 ", "hwnd": float64(0)},
		map[string]any{"class": "Notepad", "hwnd": "12345"},
	)
	if len(got) != 3 {
		t.Fatalf("targetMeta = %v，期望 hwnd/title/class", got)
	}
	if got["title"] != "记事本" {
		t.Errorf("title = %v", got["title"])
	}
	if got["class"] != "Notepad" {
		t.Errorf("class = %v", got["class"])
	}
	if got["hwnd"] != uint64(12345) {
		t.Errorf("hwnd = %v", got["hwnd"])
	}

	// 无有效目标 → nil（不把 hwnd=0/空串传给引擎导致绑定失败）。
	if got := targetMeta(map[string]any{"hwnd": float64(0), "title": "  "}); got != nil {
		t.Errorf("空目标应为 nil，实际 %v", got)
	}
}

func TestHwndValue(t *testing.T) {
	cases := []struct {
		in   any
		want uint64
		ok   bool
	}{
		{float64(42), 42, true},
		{int(7), 7, true},
		{int64(9), 9, true},
		{" 123 ", 123, true},
		{"0", 0, false},
		{float64(0), 0, false},
		{"abc", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := hwndValue(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("hwndValue(%v) = %d,%v，期望 %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ─── screenshot 沙箱 ────────────────────────────────────────────

func TestResolveSandboxPathDisabled(t *testing.T) {
	work := t.TempDir()
	got, err := resolveSandboxPath(pluginsdk.CallContext{WorkDir: work}, "shots/a.png")
	if err != nil {
		t.Fatalf("resolveSandboxPath: %v", err)
	}
	want := filepath.Join(work, "shots", "a.png")
	if got != want {
		t.Errorf("path = %q，期望 %q", got, want)
	}
	// 未启用沙箱不做白名单限制。
	outside := filepath.Join(t.TempDir(), "x.png")
	if got, err := resolveSandboxPath(pluginsdk.CallContext{}, outside); err != nil || got != outside {
		t.Errorf("未启用沙箱应放行：%q,%v", got, err)
	}
}

func TestResolveSandboxPathInside(t *testing.T) {
	work := t.TempDir()
	target := filepath.Join(work, "sub", "a.png")
	cx := pluginsdk.CallContext{
		WorkDir: work,
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{work}},
	}
	got, err := resolveSandboxPath(cx, target)
	if err != nil {
		t.Fatalf("白名单内应放行：%v", err)
	}
	if got != filepath.Clean(target) {
		t.Errorf("path = %q", got)
	}
	// 相对路径按 work_dir 解析后仍在白名单内。
	got, err = resolveSandboxPath(cx, "sub/b.png")
	if err != nil {
		t.Fatalf("相对路径应解析到 work_dir：%v", err)
	}
	if got != filepath.Join(work, "sub", "b.png") {
		t.Errorf("相对路径 = %q", got)
	}
}

func TestResolveSandboxPathOutsideDenied(t *testing.T) {
	work := t.TempDir()
	other := t.TempDir()
	cx := pluginsdk.CallContext{
		WorkDir: work,
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{work}},
	}
	_, err := resolveSandboxPath(cx, filepath.Join(other, "a.png"))
	if err == nil || !strings.Contains(err.Error(), "sandbox_denied") {
		t.Fatalf("白名单外应拒绝，实际 %v", err)
	}
}

func TestResolveSandboxPathDotDotDenied(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	cx := pluginsdk.CallContext{
		WorkDir: base,
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{allowed}},
	}
	escape := filepath.Join(allowed, "..", "outside", "a.png")
	_, err := resolveSandboxPath(cx, escape)
	if err == nil || !strings.Contains(err.Error(), "sandbox_denied") {
		t.Fatalf(".. 逃逸应拒绝，实际 %v", err)
	}
}

func TestResolveSandboxPathNoWritableDirs(t *testing.T) {
	work := t.TempDir()
	cx := pluginsdk.CallContext{
		WorkDir: work,
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true},
	}
	_, err := resolveSandboxPath(cx, filepath.Join(work, "a.png"))
	if err == nil || !strings.Contains(err.Error(), "sandbox_denied") {
		t.Fatalf("enabled 且无 writable_dirs 应拒绝，实际 %v", err)
	}
}

func TestResolveSandboxPathRelativeNoBaseDenied(t *testing.T) {
	cx := pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{t.TempDir()}}}
	_, err := resolveSandboxPath(cx, "a.png")
	if err == nil || !strings.Contains(err.Error(), "sandbox_denied") {
		t.Fatalf("无法解析的相对路径应拒绝，实际 %v", err)
	}
}

func TestResolveSandboxPathEmpty(t *testing.T) {
	got, err := resolveSandboxPath(pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{Enabled: true}}, "  ")
	if err != nil || got != "" {
		t.Fatalf("空路径应原样返回：%q,%v", got, err)
	}
}

func TestPathWithin(t *testing.T) {
	base := t.TempDir()
	if !pathWithin(filepath.Join(base, "a", "b.png"), base) {
		t.Errorf("子路径应在目录内")
	}
	if pathWithin(base+"-other", base) {
		t.Errorf("同名前缀目录不应视为目录内")
	}
}
