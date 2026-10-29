// win32.go — desktop 插件自用的窗口/鼠标原语（避免依赖 desktop 包的私有常量）。
package desktopplugin

import (
	"golang.org/x/sys/windows"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procSetCursorPos        = user32.NewProc("SetCursorPos")
	procMouseEvent          = user32.NewProc("mouse_event")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procShowWindow          = user32.NewProc("ShowWindow")
)

// 鼠标事件标志。
const (
	mouseEventLeftDown   = 0x0002
	mouseEventLeftUp     = 0x0004
	mouseEventRightDown  = 0x0008
	mouseEventRightUp    = 0x0010
	mouseEventMiddleDown = 0x0020
	mouseEventMiddleUp   = 0x0040
)

// 窗口位置标志。
const (
	swpNoZOrder  = 0x0004
	swpNoActivate = 0x0010
	swShow       = 5
)

// setCursorPos 移动光标到屏幕绝对坐标。
func setCursorPos(x, y int32) bool {
	r, _, _ := procSetCursorPos.Call(uintptr(x), uintptr(y))
	return r != 0
}

// clickAt 在屏幕坐标处点击一次。
func clickAt(x, y int32, button string) {
	if !setCursorPos(x, y) {
		return
	}
	var down, up uintptr
	switch button {
	case "right":
		down, up = mouseEventRightDown, mouseEventRightUp
	case "middle":
		down, up = mouseEventMiddleDown, mouseEventMiddleUp
	default:
		down, up = mouseEventLeftDown, mouseEventLeftUp
	}
	procMouseEvent.Call(down, 0, 0, 0, 0)
	procMouseEvent.Call(up, 0, 0, 0, 0)
}

// focusWindow 把窗口置前。
func focusWindow(hwnd uintptr) bool {
	r, _, _ := procSetForegroundWindow.Call(hwnd)
	return r != 0
}

// moveWindow 移动窗口到屏幕坐标。
func moveWindow(hwnd uintptr, x, y int32) bool {
	r, _, _ := procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0, swpNoZOrder|swpNoActivate)
	return r != 0
}

// resizeWindow 调整窗口尺寸。
func resizeWindow(hwnd uintptr, w, h int32) bool {
	r, _, _ := procSetWindowPos.Call(hwnd, 0, 0, 0, uintptr(w), uintptr(h), swpNoZOrder|swpNoActivate)
	return r != 0
}

// showWindow 显示/最小化/最大化/还原。
func showWindow(hwnd uintptr, cmd int) {
	procShowWindow.Call(hwnd, uintptr(cmd))
}
