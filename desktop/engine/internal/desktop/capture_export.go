package desktop

import (
	"bytes"
	"fmt"
	"image/png"
	"syscall"
)

// CaptureWindowPNG 截取指定窗口（hwnd=0 表示整个桌面）并编码为 PNG。
//
// 供 desktop 插件复用，避免重复实现 GDI 截图链路（GetDC/BitBlt → PrintWindow → CreateDCW 三级回退）。
func CaptureWindowPNG(hwnd uintptr) ([]byte, error) {
	var rect *Rect
	if hwnd != 0 {
		r := ClientScreenRect(syscall.Handle(hwnd))
		rect = &r
	}
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
