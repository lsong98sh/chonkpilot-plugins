// engine.go — desktop 插件的 Win32 引擎实现。
//
// 与 browser 的差异（见 docs/tool-plane §3.10）：
//   - 承载 = Win32（user32/gdi32），不是 WebView2；
//   - 句柄 = HWND（系统全局有效，可跨进程传），但输入注入受交互会话限制；
//   - 无 profile、无 console；对应物 = 截图 + 窗口树 dump；
//   - 会话回收只释放会话，不动目标窗口。
package desktopplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/lsong98sh/chonkpilot-plugins/desktop/engine/internal/desktop"
	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginhost"
)

// engine 是一个桌面会话。
type engine struct {
	opts pluginhost.ContextOptions
	hwnd syscall.Handle // start 时可选绑定；0 = 未绑定
}

// New 构造桌面会话（实现 pluginhost.Options.NewEngine）。
// start 时可经 meta 传 title / class / hwnd 预先绑定目标窗口；绑定失败立即报错，
// 避免等到 operation 才暴露「窗口不存在」。
func New(opts pluginhost.ContextOptions) (pluginhost.Engine, error) {
	e := &engine{opts: opts}
	if len(opts.Meta) > 0 {
		h, err := findWindowFrom(opts.Meta)
		if err != nil {
			return nil, fmt.Errorf("bind target window: %w", err)
		}
		e.hwnd = h
	}
	return e, nil
}

// Do 执行一个原子动作。
func (e *engine) Do(action string, args map[string]any) (map[string]any, error) {
	// SendInput 需要稳定的消息队列：把本次调用绑到一个 OS 线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	a := pluginhost.NormalizeAction(action)
	switch a {
	case "find_window":
		h, err := findWindowFrom(args)
		if err != nil {
			return nil, err
		}
		return windowInfo(h), nil

	case "list_windows":
		hs := desktop.EnumWindowsList()
		out := make([]map[string]any, 0, len(hs))
		for _, h := range hs {
			out = append(out, windowInfo(h))
		}
		return map[string]any{"windows": out, "count": len(out)}, nil

	case "window_info":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		return windowInfo(h), nil

	case "focus":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		if !focusWindow(uintptr(h)) {
			return nil, fmt.Errorf("SetForegroundWindow failed (non-interactive session?)")
		}
		return map[string]any{"hwnd": int64(h), "focused": true}, nil

	case "click":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		x := int32(argInt(args, "x", 0))
		y := int32(argInt(args, "y", 0))
		button := strings.ToLower(argString(args, "button"))
		rect := desktop.ClientScreenRect(h)
		sx, sy := rect.Left+x, rect.Top+y
		clickAt(sx, sy, button)
		return map[string]any{"hwnd": int64(h), "screen_x": sx, "screen_y": sy, "button": button}, nil

	case "input", "type":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		text := argString(args, "text")
		if text == "" {
			return nil, fmt.Errorf("text is required")
		}
		if !focusWindow(uintptr(h)) {
			return nil, fmt.Errorf("focus target window failed")
		}
		for _, r := range text {
			desktop.SendKeyUnicode(uint16(r), false)
			desktop.SendKeyUnicode(uint16(r), true)
		}
		return map[string]any{"hwnd": int64(h), "chars": len([]rune(text))}, nil

	case "key":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		spec := argString(args, "keys")
		if spec == "" {
			return nil, fmt.Errorf("keys is required (e.g. \"ctrl+s\")")
		}
		if !focusWindow(uintptr(h)) {
			return nil, fmt.Errorf("focus target window failed")
		}
		if err := desktop.PressKeySpec(spec); err != nil {
			return nil, err
		}
		return map[string]any{"hwnd": int64(h), "keys": spec}, nil

	case "screenshot", "shot":
		path := argString(args, "path")
		if path == "" {
			return nil, fmt.Errorf("path is required")
		}
		var h uintptr
		if hasTargetKey(args) {
			h32, err := e.target(args)
			if err != nil {
				return nil, err
			}
			h = uintptr(h32)
		}
		png, err := desktop.CaptureWindowPNG(h)
		if err != nil {
			return nil, fmt.Errorf("capture: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create dir: %w", err)
		}
		if err := os.WriteFile(path, png, 0o644); err != nil {
			return nil, fmt.Errorf("write screenshot: %w", err)
		}
		return map[string]any{"path": path, "bytes": len(png), "format": "png"}, nil

	case "move":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		if !moveWindow(uintptr(h), int32(argInt(args, "x", 0)), int32(argInt(args, "y", 0))) {
			return nil, fmt.Errorf("SetWindowPos(move) failed")
		}
		return map[string]any{"hwnd": int64(h), "moved": true}, nil

	case "size", "resize":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		if !resizeWindow(uintptr(h), int32(argInt(args, "w", 0)), int32(argInt(args, "h", 0))) {
			return nil, fmt.Errorf("SetWindowPos(size) failed")
		}
		return map[string]any{"hwnd": int64(h), "resized": true}, nil

	case "minimize":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		showWindow(uintptr(h), 6)
		return map[string]any{"hwnd": int64(h), "state": "minimized"}, nil

	case "maximize":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		showWindow(uintptr(h), 3)
		return map[string]any{"hwnd": int64(h), "state": "maximized"}, nil

	case "restore":
		h, err := e.target(args)
		if err != nil {
			return nil, err
		}
		showWindow(uintptr(h), 9)
		return map[string]any{"hwnd": int64(h), "state": "restored"}, nil

	default:
		return nil, fmt.Errorf("unsupported action: %s", action)
	}
}

// Close 结束会话（不关闭目标窗口）。
func (e *engine) Close() error { return nil }

// target 解析操作目标窗口：显式 hwnd/title/class 优先，否则用 start 时绑定的窗口。
func (e *engine) target(args map[string]any) (syscall.Handle, error) {
	if hasTargetKey(args) {
		return findWindowFrom(args)
	}
	if e.hwnd != 0 {
		return e.hwnd, nil
	}
	return 0, fmt.Errorf("no target window: pass hwnd/title/class, or bind one at start (meta)")
}

// hasTargetKey 判断参数里是否显式指定了窗口。
func hasTargetKey(args map[string]any) bool {
	if args == nil {
		return false
	}
	for _, k := range []string{"hwnd", "title", "class"} {
		if v, ok := args[k]; ok && v != nil {
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				continue
			}
			return true
		}
	}
	return false
}

// findWindowFrom 按 hwnd / class / title 查找窗口。
func findWindowFrom(m map[string]any) (syscall.Handle, error) {
	if v, ok := m["hwnd"]; ok {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				return syscall.Handle(uintptr(n)), nil
			}
		case int:
			if n > 0 {
				return syscall.Handle(uintptr(n)), nil
			}
		case int64:
			if n > 0 {
				return syscall.Handle(uintptr(n)), nil
			}
		case string:
			if parsed, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64); err == nil && parsed > 0 {
				return syscall.Handle(uintptr(parsed)), nil
			}
		}
	}
	if cls := strings.TrimSpace(argString(m, "class")); cls != "" {
		return desktop.FindWindowByClass(cls)
	}
	if title := strings.TrimSpace(argString(m, "title")); title != "" {
		if h, err := desktop.FindWindowByTitle(title); err == nil && h != 0 {
			return h, nil
		}
		// 精确标题未命中 → 退化为子串匹配
		if h, err := desktop.EnumWindowsFind(func(h syscall.Handle) bool {
			return desktop.WindowTitleMatches(h, title)
		}); err == nil && h != 0 {
			return h, nil
		}
		return 0, fmt.Errorf("window not found by title substring: %s", title)
	}
	return 0, fmt.Errorf("specify hwnd, class, or title")
}

// windowInfo 返回窗口的基础信息。
func windowInfo(h syscall.Handle) map[string]any {
	r := desktop.ClientScreenRect(h)
	return map[string]any{
		"hwnd":  int64(h),
		"title": desktop.GetWindowTitle(h),
		"client_screen_rect": map[string]any{
			"left":   r.Left,
			"top":    r.Top,
			"right":  r.Right,
			"bottom": r.Bottom,
			"width":  r.Right - r.Left,
			"height": r.Bottom - r.Top,
		},
	}
}

// argString 取字符串参数。
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

// argInt 取整数参数。
func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}
