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

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk/enginehost"
	"github.com/lsong98sh/chonkpilot-plugins/desktop/engine/internal/desktop"
)

// engine 是一个桌面会话。
type engine struct {
	opts enginehost.ContextOptions
	hwnd syscall.Handle // start 时可选绑定；0 = 未绑定
}

// New 构造桌面会话（实现 enginehost.Options.NewEngine）。
// start 时可经 meta 传 title / class / hwnd 预先绑定目标窗口；绑定失败立即报错，
// 避免等到 operation 才暴露「窗口不存在」。
func New(opts enginehost.ContextOptions) (enginehost.Engine, error) {
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
//
// 兜底 recover：单个动作里的 Win32/GDI panic 只转换为本次调用的错误，
// 不允许打死整个常驻引擎（P0：screenshot 无目标窗口曾导致进程退出）。
func (e *engine) Do(action string, args map[string]any) (out map[string]any, err error) {
	defer recoverAsError(action, &out, &err)
	// SendInput 需要稳定的消息队列：把本次调用绑到一个 OS 线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	a := enginehost.NormalizeAction(action)
	switch a {
	case "find_window":
		h, err := findWindowFrom(args)
		if err != nil {
			return nil, err
		}
		return windowInfo(h), nil

	case "list_windows":
		return listWindows(args), nil

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
		// 无显式目标时：start 绑定过窗口 → 截绑定窗口；否则（或 full_screen=true）→ 主屏整屏。
		// h=0 时 CaptureWindowPNG 走主屏，不再把 nil rect 传给 captureImage。
		h, err := e.screenshotTarget(args)
		if err != nil {
			return nil, err
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
		w, hh := argInt(args, "w", 0), argInt(args, "h", 0)
		if w <= 0 || hh <= 0 {
			return nil, fmt.Errorf("w and h must be positive")
		}
		if !resizeWindow(uintptr(h), int32(w), int32(hh)) {
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

// screenshotTarget 解析截图目标：显式 hwnd/title/class 优先；否则用 start 绑定窗口
// （full_screen=true 时忽略绑定）；都没有 → 0（截主屏整屏）。
func (e *engine) screenshotTarget(args map[string]any) (uintptr, error) {
	if hasTargetKey(args) {
		h, err := e.target(args)
		if err != nil {
			return 0, err
		}
		return uintptr(h), nil
	}
	if e.hwnd != 0 && !argBool(args, "full_screen", false) {
		return uintptr(e.hwnd), nil
	}
	return 0, nil
}

// hasTargetKey 判断参数里是否显式指定了窗口（hwnd=0 / 空串视为未指定）。
func hasTargetKey(args map[string]any) bool {
	if args == nil {
		return false
	}
	if v, ok := args["hwnd"]; ok {
		if h, valid := hwndFrom(v); valid && h != 0 {
			return true
		}
	}
	for _, k := range []string{"title", "class"} {
		if s := strings.TrimSpace(argString(args, k)); s != "" {
			return true
		}
	}
	return false
}

// hwndFrom 把 hwnd 参数（数字/数字字符串）转成句柄；0 / 非法返回 false。
func hwndFrom(v any) (syscall.Handle, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return syscall.Handle(uintptr(n)), true
		}
	case int:
		if n > 0 {
			return syscall.Handle(uintptr(n)), true
		}
	case int64:
		if n > 0 {
			return syscall.Handle(uintptr(n)), true
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64); err == nil && parsed > 0 {
			return syscall.Handle(uintptr(parsed)), true
		}
	}
	return 0, false
}

// findWindowFrom 按 hwnd / class / title 查找窗口。
func findWindowFrom(m map[string]any) (syscall.Handle, error) {
	if v, ok := m["hwnd"]; ok {
		if h, valid := hwndFrom(v); valid {
			return h, nil
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

// listWindows 枚举顶层窗口：补充 class / visible，并标注同名标题与（可选）过滤隐藏窗口。
func listWindows(args map[string]any) map[string]any {
	hs := desktop.EnumWindowsList()
	infos := make([]*desktop.WindowInfo, 0, len(hs))
	titleCount := make(map[string]int, len(hs))
	for _, h := range hs {
		info := desktop.GetWindowInfo(h)
		infos = append(infos, info)
		if info.Title != "" {
			titleCount[info.Title]++
		}
	}
	visibleOnly := argBool(args, "visible_only", false)
	limit := argInt(args, "limit", 0)
	out := make([]map[string]any, 0, len(infos))
	for _, info := range infos {
		if visibleOnly && !info.Visible {
			continue
		}
		if limit > 0 && len(out) >= limit {
			break
		}
		m := windowInfoMap(info)
		if info.Title != "" && titleCount[info.Title] > 1 {
			m["duplicate_title"] = true
		}
		out = append(out, m)
	}
	return map[string]any{"windows": out, "count": len(out)}
}

// windowInfo 返回窗口的基础信息。
func windowInfo(h syscall.Handle) map[string]any {
	return windowInfoMap(desktop.GetWindowInfo(h))
}

// windowInfoMap 统一窗口信息形状（list_windows / find_window / window_info 共用）。
func windowInfoMap(info *desktop.WindowInfo) map[string]any {
	r := desktop.ClientScreenRect(syscall.Handle(info.Hwnd))
	return map[string]any{
		"hwnd":    int64(info.Hwnd),
		"title":   info.Title,
		"class":   info.Class,
		"visible": info.Visible,
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

// argBool 取布尔参数。
func argBool(args map[string]any, key string, def bool) bool {
	if args == nil {
		return def
	}
	if v, ok := args[key].(bool); ok {
		return v
	}
	return def
}

// recoverAsError 把 Win32/GDI panic 转换为本次调用的错误（不打死常驻引擎）。
func recoverAsError(action string, out *map[string]any, err *error) {
	if r := recover(); r != nil {
		*out, *err = nil, fmt.Errorf("operation %q panic: %v", action, r)
	}
}
