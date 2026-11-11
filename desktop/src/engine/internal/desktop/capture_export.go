package desktop

import (
	"bytes"
	"fmt"
	"image/png"
	"syscall"
)

// CaptureWindowPNG 截取指定窗口客户区（hwnd=0 表示整个主屏）并编码为 PNG。
//
// 供 desktop 插件复用，避免重复实现 GDI 截图链路（GetDC/BitBlt → PrintWindow → CreateDCW 三级回退）。
// hwnd=0 时使用主屏矩形（GetFullScreenRect），不再把 nil rect 交给 captureImage（P0 崩溃根因）。
func CaptureWindowPNG(hwnd uintptr) ([]byte, error) {
	rect := captureRect(hwnd)
	img, err := captureImage(rect, hwnd)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// captureRect 返回截图区域：hwnd≠0 → 客户区屏幕矩形；hwnd=0 → 主屏整屏矩形。
func captureRect(hwnd uintptr) *Rect {
	if hwnd != 0 {
		r := ClientScreenRect(syscall.Handle(hwnd))
		return &r
	}
	return GetFullScreenRect()
}

// PressKeySpec 解析并发送组合键（如 "ctrl+s"、"alt+f4"、"enter"）。
func PressKeySpec(spec string) error {
	mods, key, err := parseKeySpec(spec)
	if err != nil {
		return err
	}
	if key == 0 {
		return fmt.Errorf("empty key spec: %s", spec)
	}
	release := DesktopPressMods(mods)
	defer release()
	if !SendKey(key, false) {
		return fmt.Errorf("send key down failed")
	}
	SendKey(key, true)
	return nil
}
