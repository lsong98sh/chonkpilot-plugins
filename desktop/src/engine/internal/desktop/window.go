package desktop

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// FindWindowByTitle finds a window by its title.
func FindWindowByTitle(title string) (syscall.Handle, error) {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	ret, _, _ := FindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	if ret == 0 {
		return EnumWindowsFind(func(hwnd syscall.Handle) bool {
			return WindowTitleMatches(hwnd, title)
		})
	}
	return syscall.Handle(ret), nil
}

// FindWindowByClass finds a top-level window by its class name.
func FindWindowByClass(class string) (syscall.Handle, error) {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	ret, _, _ := FindWindowW.Call(uintptr(unsafe.Pointer(classPtr)), 0)
	if ret == 0 {
		return EnumWindowsFind(func(hwnd syscall.Handle) bool {
			classBuf := make([]uint16, 256)
			GetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&classBuf[0])), 256)
			return syscall.UTF16ToString(classBuf) == class
		})
	}
	return syscall.Handle(ret), nil
}

// ClientScreenRect 返回窗口客户区在屏幕坐标系下的 rect（GetClientRect + ClientToScreen）。
func ClientScreenRect(hwnd syscall.Handle) Rect {
	var cr Rect
	GetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cr)))
	var pt struct{ X, Y int32 }
	ClientToScreen.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pt)))
	return Rect{Left: pt.X, Top: pt.Y, Right: pt.X + cr.Right, Bottom: pt.Y + cr.Bottom}
}

// WindowTitleMatches checks if window title contains substr.
func WindowTitleMatches(hwnd syscall.Handle, substr string) bool {
	length, _, _ := GetWindowTextLengthW.Call(uintptr(hwnd))
	if length == 0 {
		return false
	}
	buf := make([]uint16, length+1)
	GetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(length+1))
	return strings.Contains(syscall.UTF16ToString(buf), substr)
}

// GetWindowTitle returns the title of a window.
func GetWindowTitle(hwnd syscall.Handle) string {
	length, _, _ := GetWindowTextLengthW.Call(uintptr(hwnd))
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	GetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(length+1))
	return syscall.UTF16ToString(buf)
}

// EnumWindowsFind finds a window by matching function.
func EnumWindowsFind(match func(syscall.Handle) bool) (syscall.Handle, error) {
	var found syscall.Handle
	cb := syscall.NewCallback(func(hwnd syscall.Handle, lparam uintptr) uintptr {
		if match(hwnd) {
			found = hwnd
			return 0
		}
		return 1
	})
	EnumWindows.Call(cb, 0)
	if found == 0 {
		return 0, fmt.Errorf("no matching window found")
	}
	return found, nil
}

// EnumWindowsList returns a list of all top-level window handles.
func EnumWindowsList() []syscall.Handle {
	var result []syscall.Handle
	cb := syscall.NewCallback(func(hwnd syscall.Handle, lparam uintptr) uintptr {
		result = append(result, hwnd)
		return 1
	})
	EnumWindows.Call(cb, 0)
	return result
}

// WindowInfo holds information about a window.
type WindowInfo struct {
	Title   string  `json:"title"`
	Class   string  `json:"class"`
	Hwnd    uintptr `json:"hwnd,omitempty"`
	Left    int32   `json:"left"`
	Top     int32   `json:"top"`
	Right   int32   `json:"right"`
	Bottom  int32   `json:"bottom"`
	Width   int32   `json:"width"`
	Height  int32   `json:"height"`
	Visible bool    `json:"visible"`
}

// GetWindowInfo gets information about a window.
func GetWindowInfo(hwnd syscall.Handle) *WindowInfo {
	title := GetWindowTitle(hwnd)
	var r Rect
	GetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))

	classBuf := make([]uint16, 256)
	GetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&classBuf[0])), 256)
	className := syscall.UTF16ToString(classBuf)

	isVisible := uint32(0)
	proc := User32.NewProc("IsWindowVisible")
	ret, _, _ := proc.Call(uintptr(hwnd))
	isVisible = uint32(ret)

	return &WindowInfo{
		Title:   title,
		Class:   className,
		Hwnd:    uintptr(hwnd),
		Left:    r.Left,
		Top:     r.Top,
		Right:   r.Right,
		Bottom:  r.Bottom,
		Width:   r.Right - r.Left,
		Height:  r.Bottom - r.Top,
		Visible: isVisible != 0,
	}
}
