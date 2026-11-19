package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// TestBrowserContract md 是唯一契约源：字段/描述/schema 必须与实现一致。
func TestBrowserContract(t *testing.T) {
	tool, err := loadBrowserContract()
	if err != nil {
		t.Fatalf("loadBrowserContract: %v", err)
	}
	if tool.Name != "browser_run" {
		t.Fatalf("name=%q", tool.Name)
	}
	if !tool.Hot {
		t.Fatal("hot 应为 true")
	}
	if tool.Category != "browser" {
		t.Fatalf("category=%q", tool.Category)
	}
	if tool.Async != "auto" || tool.Timeout != 60 {
		t.Fatalf("policy = %s/%d", tool.Async, tool.Timeout)
	}
	if tool.AsyncSet != true || tool.TimeoutSet != true {
		t.Fatalf("async/timeout must be explicit: %+v", tool)
	}
	// 打点声明：browser 缺省不落盘 → touch_files: false（显式声明）。
	if !tool.TouchFilesSet || tool.TouchFiles {
		t.Fatalf("touch_files = %v(set=%v)，期望显式 false", tool.TouchFiles, tool.TouchFilesSet)
	}
	if tool.Description == "" || tool.Description == tool.Name {
		t.Fatalf("摘要未解析: %q", tool.Description)
	}
	for _, want := range []string{"eval", "script", "screenshot", "timeoutMs", "context_id"} {
		if !strings.Contains(tool.Description, want) {
			t.Fatalf("摘要缺少 %q: %s", want, tool.Description)
		}
	}

	props, _ := tool.Schema["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	enum, _ := action["enum"].([]any)
	if len(enum) == 0 {
		t.Fatalf("action 缺少 enum: %v", action)
	}
	args, _ := props["args"].(map[string]any)
	argProps, _ := args["properties"].(map[string]any)
	for _, key := range []string{"script", "eval", "expr", "timeoutMs", "path", "url", "selector"} {
		if _, ok := argProps[key]; !ok {
			t.Fatalf("args 缺少 %q", key)
		}
	}
	timeout, _ := argProps["timeoutMs"].(map[string]any)
	if timeout["maximum"] != float64(maxWaitTimeoutMs) && timeout["maximum"] != maxWaitTimeoutMs {
		t.Fatalf("timeoutMs.maximum=%v, want %d", timeout["maximum"], maxWaitTimeoutMs)
	}
}

// TestEvalAliases eval/script 动作与参数键别名统一。
func TestEvalAliases(t *testing.T) {
	cx := pluginsdk.CallContext{}
	cases := []struct {
		action string
		args   map[string]any
	}{
		{"eval", map[string]any{"expr": "1+1"}},
		{"script", map[string]any{"eval": "1+1"}},
		{"script", map[string]any{"js": "1+1"}},
		{"js", map[string]any{"expr": "1+1"}},
		{"eval", map[string]any{"script": "1+1"}},
		{"eval", map[string]any{"code": "1+1"}},
	}
	for _, c := range cases {
		action, got, err := prepareOperation(c.action, c.args, cx, settings{})
		if err != nil {
			t.Fatalf("%s %v: %v", c.action, c.args, err)
		}
		if action != "eval" {
			t.Fatalf("action=%q, want eval", action)
		}
		if got["script"] != "1+1" {
			t.Fatalf("%s %v → script=%v", c.action, c.args, got["script"])
		}
		if _, _, err := prepareOperation(c.action, c.args, cx, settings{}); err != nil {
			t.Fatalf("重复调用不应修改入参: %v", err)
		}
	}
}

// TestWaitTimeoutBound wait.timeoutMs 上限与非法值。
func TestWaitTimeoutBound(t *testing.T) {
	cx := pluginsdk.CallContext{}
	if _, _, err := prepareOperation("wait", map[string]any{"selector": "#x", "timeoutMs": float64(maxWaitTimeoutMs)}, cx, settings{}); err != nil {
		t.Fatalf("上限值应通过: %v", err)
	}
	for _, v := range []any{float64(maxWaitTimeoutMs + 1), 0, -1} {
		if _, _, err := prepareOperation("wait", map[string]any{"selector": "#x", "timeoutMs": v}, cx, settings{}); err == nil {
			t.Fatalf("timeoutMs=%v 应拒绝", v)
		}
	}
	// 缺省不校验（引擎默认 10s）。
	if _, _, err := prepareOperation("wait", map[string]any{"selector": "#x"}, cx, settings{}); err != nil {
		t.Fatalf("缺省 timeoutMs 应通过: %v", err)
	}
}

// TestScreenshotDefaultDirMissing 未给 path 且未配置默认目录 → 提前报错。
func TestScreenshotDefaultDirMissing(t *testing.T) {
	_, _, err := prepareOperation("screenshot", map[string]any{}, pluginsdk.CallContext{}, settings{})
	if err == nil || !strings.Contains(err.Error(), "截图默认目录") {
		t.Fatalf("want 默认目录错误, got %v", err)
	}
}

// TestScreenshotSandbox 沙箱：白名单内通过（含子目录），越界拒绝且不落盘；nil/false 不限制。
func TestScreenshotSandbox(t *testing.T) {
	allowed := t.TempDir()
	denied := t.TempDir()

	if err := checkScreenshotSandbox(pluginsdk.CallContext{}, filepath.Join(denied, "x.png")); err != nil {
		t.Fatalf("未启用沙箱不应限制: %v", err)
	}
	if err := checkScreenshotSandbox(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: false},
	}, filepath.Join(denied, "x.png")); err != nil {
		t.Fatalf("enabled=false 不应限制: %v", err)
	}

	// enabled=true 但没下发可写目录 → fail-closed。
	if err := checkScreenshotSandbox(pluginsdk.CallContext{
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true},
	}, filepath.Join(denied, "x.png")); err == nil {
		t.Fatal("enabled=true 且无白名单时应拒绝")
	}

	cx := pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{
		Enabled: true, WritableDirs: []string{"  ", allowed},
	}}
	if err := checkScreenshotSandbox(cx, filepath.Join(allowed, "x.png")); err != nil {
		t.Fatalf("白名单内应通过: %v", err)
	}
	if err := checkScreenshotSandbox(cx, filepath.Join(allowed, "sub", "x.png")); err != nil {
		t.Fatalf("白名单子目录应通过: %v", err)
	}
	err := checkScreenshotSandbox(cx, filepath.Join(denied, "x.png"))
	if err == nil || !strings.Contains(err.Error(), "sandbox:") || !strings.Contains(err.Error(), "未落盘") {
		t.Fatalf("白名单外应明确失败: %v", err)
	}

	// shot 是 screenshot 的别名，必须走同一套沙箱校验（不能借别名绕过）。
	if _, _, err := prepareOperation("shot", map[string]any{"path": filepath.Join(denied, "x.png")}, cx, settings{}); err == nil {
		t.Fatal("shot 别名越界应拒绝")
	}
}

// TestScreenshotPathResolution 相对路径：沙箱开启时解析到工作目录；无工作目录则明确报错。
func TestScreenshotPathResolution(t *testing.T) {
	wd := t.TempDir()
	cx := pluginsdk.CallContext{
		WorkDir: wd,
		Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{wd}},
	}
	action, args, err := prepareOperation("screenshot", map[string]any{"path": "shots/a.png"}, cx, settings{})
	if err != nil {
		t.Fatalf("相对路径应解析到工作目录: %v", err)
	}
	if action != "screenshot" {
		t.Fatalf("action=%q", action)
	}
	if want := filepath.Join(wd, "shots", "a.png"); args["path"] != want {
		t.Fatalf("path=%v, want %s", args["path"], want)
	}

	noWD := pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{wd}}}
	if _, _, err := prepareOperation("screenshot", map[string]any{"path": "shots/a.png"}, noWD, settings{}); err == nil {
		t.Fatal("无工作目录的相对路径应拒绝")
	}

	// 沙箱未启用：保持相对路径（旧行为，由引擎 cwd 解析）。
	if _, args, err := prepareOperation("screenshot", map[string]any{"path": "shots/a.png"}, pluginsdk.CallContext{}, settings{}); err != nil {
		t.Fatalf("未启用沙箱不应限制: %v", err)
	} else if args["path"] != "shots/a.png" {
		t.Fatalf("path=%v", args["path"])
	}
}

// TestScreenshotDefaultDirSandbox 配置的默认目录不在白名单 → 拒绝（默认目录也不放行）。
func TestScreenshotDefaultDirSandbox(t *testing.T) {
	allowed := t.TempDir()
	denied := t.TempDir()
	cx := pluginsdk.CallContext{Sandbox: &pluginsdk.SandboxPolicy{Enabled: true, WritableDirs: []string{allowed}}}
	_, _, err := prepareOperation("screenshot", map[string]any{}, cx, settings{ScreenshotDir: denied})
	if err == nil || !strings.Contains(err.Error(), "sandbox:") {
		t.Fatalf("默认目录越界应拒绝: %v", err)
	}
}

// fakeEngine 记录调用并按 context_id 模拟引擎的 unknown_context。
type fakeEngine struct {
	calls    []string
	startSeq int
}

func (f *fakeEngine) Ensure(context.Context) error { return nil }
func (f *fakeEngine) Socket() string               { return "" }
func (f *fakeEngine) Call(_ context.Context, endpoint string, params any) (map[string]any, error) {
	f.calls = append(f.calls, endpoint)
	p, _ := params.(map[string]any)
	switch endpoint {
	case "start":
		f.startSeq++
		return map[string]any{"context_id": fmt.Sprintf("ctx-%d", f.startSeq)}, nil
	case "stop", "operation":
		id, _ := p["context_id"].(string)
		if id == "stale" {
			return nil, errors.New("unknown_context: unknown or expired context: " + id)
		}
		if endpoint == "stop" {
			return map[string]any{"closed": true}, nil
		}
		return map[string]any{"done": true}, nil
	}
	return map[string]any{}, nil
}

// TestImplicitContextExpiredRecovers 验证隐式 context 失效后清理映射、重建并重试一次。
func TestImplicitContextExpiredRecovers(t *testing.T) {
	fe := &fakeEngine{}
	b := &browserPlugin{cfg: &configManager{}, eng: fe, ctxID: map[string]string{"inst-1": "stale"}}
	res, err := b.run(context.Background(), map[string]any{
		"action": "navigate",
		"args":   map[string]any{"url": "https://example.com"},
	}, pluginsdk.CallContext{InstanceID: "inst-1"})
	if err != nil {
		t.Fatalf("隐式失效应自动恢复: %v", err)
	}
	if res.(map[string]any)["context_id"] != "ctx-1" {
		t.Fatalf("应返回新 context: %+v", res)
	}
	if got := b.get("inst-1"); got != "ctx-1" {
		t.Fatalf("映射未更新: %q", got)
	}
	want := []string{"operation", "start", "operation"}
	if strings.Join(fe.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("调用顺序 = %v, want %v", fe.calls, want)
	}
}

// TestExplicitContextExpiredNoRetry 显式 context 失效返回可操作错误，不自动重建。
func TestExplicitContextExpiredNoRetry(t *testing.T) {
	fe := &fakeEngine{}
	b := &browserPlugin{cfg: &configManager{}, eng: fe, ctxID: map[string]string{}}
	_, err := b.run(context.Background(), map[string]any{
		"action":     "navigate",
		"context_id": "stale",
		"args":       map[string]any{"url": "https://example.com"},
	}, pluginsdk.CallContext{InstanceID: "inst-1"})
	if err == nil || !strings.Contains(err.Error(), "已失效") {
		t.Fatalf("显式失效应报可操作错误: %v", err)
	}
	if strings.Contains(strings.Join(fe.calls, ","), "start") {
		t.Fatalf("显式 context 不应自动 start: %v", fe.calls)
	}
}

// TestStopExpiredContextIsIdempotent 失效 context 的 stop 返回 stopped=false 并清理映射。
func TestStopExpiredContextIsIdempotent(t *testing.T) {
	fe := &fakeEngine{}
	b := &browserPlugin{cfg: &configManager{}, eng: fe, ctxID: map[string]string{"inst-1": "stale"}}
	res, err := b.run(context.Background(), map[string]any{"action": "stop"}, pluginsdk.CallContext{InstanceID: "inst-1"})
	if err != nil {
		t.Fatalf("失效 context 的 stop 不应报错: %v", err)
	}
	out := res.(map[string]any)
	if out["stopped"] != false {
		t.Fatalf("stopped 应为 false: %+v", out)
	}
	if b.get("inst-1") != "" {
		t.Fatal("stop 后映射应清空")
	}
}

// TestCapOutput 验证超大文本字段按 UTF-8 边界截断并标记。
func TestCapOutput(t *testing.T) {
	big := strings.Repeat("a", maxOutputBytes+10)
	res := map[string]any{"html": big, "text": "small"}
	capOutput(res)
	if res["truncated"] != true || res["total_bytes"] != len(big) {
		t.Fatalf("应标记截断: %+v", res)
	}
	if out, _ := res["html"].(string); len(out) > maxOutputBytes+200 || !strings.Contains(out, "已截断") {
		t.Fatalf("截断文本异常: len=%d", len(out))
	}
	if res["text"] != "small" {
		t.Fatal("未超限字段不应改动")
	}
	zh := map[string]any{"value": strings.Repeat("中", maxOutputBytes/3+10)}
	capOutput(zh)
	if !utf8.ValidString(zh["value"].(string)) {
		t.Fatal("截断点必须是合法 UTF-8")
	}
}

// TestStartRecyclesOldContext 重复 start 应回收同实例旧 context。
func TestStartRecyclesOldContext(t *testing.T) {
	fe := &fakeEngine{}
	b := &browserPlugin{cfg: &configManager{}, eng: fe, ctxID: map[string]string{"inst-1": "ctx-old"}}
	res, err := b.run(context.Background(), map[string]any{"action": "start"}, pluginsdk.CallContext{InstanceID: "inst-1"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if strings.Join(fe.calls, ",") != "stop,start" {
		t.Fatalf("应先 stop 旧 context: %v", fe.calls)
	}
	if res.(map[string]any)["context_id"] != "ctx-1" || b.get("inst-1") != "ctx-1" {
		t.Fatalf("新 context 未生效: %+v", res)
	}
}
