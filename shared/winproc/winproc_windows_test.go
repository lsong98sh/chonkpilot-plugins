//go:build windows

package winproc

import "testing"

// TestSysProcAttr 断言 Windows 下 spawn 属性同时抑制窗口显示与创建控制台。
func TestSysProcAttr(t *testing.T) {
	attr := SysProcAttr()
	if attr == nil {
		t.Fatal("SysProcAttr() = nil, want non-nil on windows")
	}
	if !attr.HideWindow {
		t.Errorf("HideWindow = false, want true")
	}
	if attr.CreationFlags != 0x08000000 {
		t.Errorf("CreationFlags = 0x%08X, want 0x08000000 (CREATE_NO_WINDOW)", attr.CreationFlags)
	}
}
