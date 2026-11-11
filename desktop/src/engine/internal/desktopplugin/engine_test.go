package desktopplugin

import (
	"strings"
	"syscall"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugins/desktop/engine/internal/desktop"
)

// TestRecoverAsErrorConvertsPanic：单次动作 panic 必须变成错误，而不是退出引擎。
func TestRecoverAsErrorConvertsPanic(t *testing.T) {
	var out map[string]any
	var err error
	func() {
		defer recoverAsError("screenshot", &out, &err)
		panic("nil rect")
	}()
	if err == nil || !strings.Contains(err.Error(), "panic") || !strings.Contains(err.Error(), "screenshot") {
		t.Fatalf("panic 未转换为错误：%v", err)
	}
	if out != nil {
		t.Fatalf("panic 后 out 应为 nil：%v", out)
	}
}

func TestHWNDFrom(t *testing.T) {
	cases := []struct {
		in   any
		want syscall.Handle
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
		got, ok := hwndFrom(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("hwndFrom(%v) = %v,%v，期望 %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestHasTargetKey(t *testing.T) {
	cases := []struct {
		args map[string]any
		want bool
	}{
		{map[string]any{"hwnd": float64(0)}, false},
		{map[string]any{"hwnd": "0"}, false},
		{map[string]any{"title": "  "}, false},
		{map[string]any{"title": "记事本"}, true},
		{map[string]any{"class": "Notepad"}, true},
		{map[string]any{"hwnd": "123"}, true},
		{nil, false},
	}
	for _, c := range cases {
		if got := hasTargetKey(c.args); got != c.want {
			t.Errorf("hasTargetKey(%v) = %v，期望 %v", c.args, got, c.want)
		}
	}
}

func TestTargetUsesBoundWindow(t *testing.T) {
	e := &engine{hwnd: syscall.Handle(42)}
	h, err := e.target(nil)
	if err != nil || h != 42 {
		t.Fatalf("绑定窗口未生效：%v,%v", h, err)
	}
	// 显式 hwnd 优先于绑定。
	h, err = e.target(map[string]any{"hwnd": "99"})
	if err != nil || h != 99 {
		t.Fatalf("显式 hwnd 未优先：%v,%v", h, err)
	}
}

func TestTargetMissing(t *testing.T) {
	e := &engine{}
	_, err := e.target(nil)
	if err == nil || !strings.Contains(err.Error(), "no target window") {
		t.Fatalf("无目标应报错，实际 %v", err)
	}
}

// TestScreenshotTarget：显式目标 > 绑定窗口 > 主屏（0）；full_screen 可强制整屏。
func TestScreenshotTarget(t *testing.T) {
	e := &engine{hwnd: syscall.Handle(42)}

	if h, err := e.screenshotTarget(nil); err != nil || h != 42 {
		t.Errorf("绑定窗口截图目标 = %v,%v，期望 42", h, err)
	}
	if h, err := e.screenshotTarget(map[string]any{"full_screen": true}); err != nil || h != 0 {
		t.Errorf("full_screen 应截整屏 = %v,%v", h, err)
	}
	if h, err := e.screenshotTarget(map[string]any{"hwnd": "99"}); err != nil || h != 99 {
		t.Errorf("显式 hwnd 应优先 = %v,%v", h, err)
	}

	plain := &engine{}
	if h, err := plain.screenshotTarget(nil); err != nil || h != 0 {
		t.Errorf("无绑定应截整屏 = %v,%v", h, err)
	}
}

func TestWindowInfoMapShape(t *testing.T) {
	info := &desktop.WindowInfo{Hwnd: 0, Title: "t", Class: "c", Visible: true}
	m := windowInfoMap(info)
	for _, k := range []string{"hwnd", "title", "class", "visible", "client_screen_rect"} {
		if _, ok := m[k]; !ok {
			t.Errorf("windowInfoMap 缺少 %q：%v", k, m)
		}
	}
	if m["class"] != "c" || m["visible"] != true {
		t.Errorf("class/visible = %v/%v", m["class"], m["visible"])
	}
}

// TestListWindowsFields：真实枚举顶层窗口，确认 class/visible 与 count 自洽。
func TestListWindowsFields(t *testing.T) {
	res := listWindows(nil)
	wins, ok := res["windows"].([]map[string]any)
	if !ok {
		t.Fatalf("windows 类型错误：%T", res["windows"])
	}
	if count, _ := res["count"].(int); count != len(wins) {
		t.Errorf("count = %v，len = %d", res["count"], len(wins))
	}
	for i, w := range wins {
		if _, ok := w["class"].(string); !ok {
			t.Errorf("窗口 %d 缺少 class：%v", i, w)
		}
		if _, ok := w["visible"].(bool); !ok {
			t.Errorf("窗口 %d 缺少 visible：%v", i, w)
		}
	}
}

func TestListWindowsVisibleOnly(t *testing.T) {
	res := listWindows(map[string]any{"visible_only": true})
	wins, _ := res["windows"].([]map[string]any)
	for i, w := range wins {
		if w["visible"] != true {
			t.Errorf("visible_only 返回了隐藏窗口 %d：%v", i, w)
		}
	}
}

func TestListWindowsLimit(t *testing.T) {
	res := listWindows(map[string]any{"limit": 1})
	wins, _ := res["windows"].([]map[string]any)
	if len(wins) > 1 {
		t.Fatalf("limit=1 返回 %d 条", len(wins))
	}
}
