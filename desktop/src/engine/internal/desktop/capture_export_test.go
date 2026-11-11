package desktop

import "testing"

// TestCaptureRectNoTargetUsesFullScreen：hwnd=0 必须拿主屏矩形（不是 nil）。
func TestCaptureRectNoTargetUsesFullScreen(t *testing.T) {
	r := captureRect(0)
	if r == nil {
		t.Fatal("captureRect(0) = nil（会再次触发 nil 解引用）")
	}
	if r.Right <= r.Left || r.Bottom <= r.Top {
		t.Fatalf("主屏矩形非法：%+v", r)
	}
}

// TestCaptureWindowPNGNoTargetNoPanic：无目标截图不得 panic；无交互桌面时可跳过。
func TestCaptureWindowPNGNoTargetNoPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CaptureWindowPNG(0) panic: %v", r)
		}
	}()
	png, err := CaptureWindowPNG(0)
	if err != nil {
		t.Skipf("当前会话无法截屏（跳过内容断言）：%v", err)
	}
	if len(png) == 0 {
		t.Fatal("截图结果为空")
	}
}
