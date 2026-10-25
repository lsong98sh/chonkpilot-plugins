package desktop

import (
	"fmt"
	"image"
	"syscall"
	"unsafe"
)

// BITMAPINFOHEADER is the standard 40-byte bitmap info header.
type BITMAPINFOHEADER struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

// captureRectToImage captures pixels from srcDC into *image.RGBA (no encoding).
func captureRectToImage(srcDC uintptr, r *Rect) (*image.RGBA, error) {
	width := r.Right - r.Left
	height := r.Bottom - r.Top
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid capture rect: %dx%d", width, height)
	}

	memDC, _, _ := CreateCompatibleDC.Call(srcDC)
	if memDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer DeleteDC.Call(memDC)

	bmiSize := 40 + 1024
	bmi := make([]byte, bmiSize)
	header := (*BITMAPINFOHEADER)(unsafe.Pointer(&bmi[0]))
	header.BiSize = 40
	header.BiWidth = int32(width)
	header.BiHeight = -int32(height) // top-down
	header.BiPlanes = 1
	header.BiBitCount = 32
	header.BiCompression = 0 // BI_RGB

	var bitsPtr unsafe.Pointer
	hbitmap, _, _ := CreateDIBSection.Call(
		memDC,
		uintptr(unsafe.Pointer(&bmi[0])),
		DibRgbColors,
		uintptr(unsafe.Pointer(&bitsPtr)),
		0, 0)
	if hbitmap == 0 {
		return nil, fmt.Errorf("CreateDIBSection failed")
	}
	// 选入 memDC（保存旧位图）。收尾须**先选回旧位图再 DeleteObject** —— 位图被选入 DC 时
	// DeleteObject 会失败、不回收（GDI 规定）；defer LIFO 保证本闭包先于上面的 DeleteDC(memDC) 执行，
	// 否则每次截图泄漏 1 个 DIB（R8-D-1）。
	oldBmp, _, _ := SelectObject.Call(memDC, hbitmap)
	defer func() {
		if oldBmp != 0 {
			SelectObject.Call(memDC, oldBmp)
		}
		DeleteObject.Call(hbitmap)
	}()

	ret, _, _ := BitBlt.Call(memDC, 0, 0, uintptr(width), uintptr(height),
		srcDC, uintptr(r.Left), uintptr(r.Top), SrcCopy)
	if ret == 0 {
		return nil, fmt.Errorf("BitBlt failed")
	}

	// 直接读 DIB 内存（bitsPtr 由 GDI 填充；用 unsafe.Pointer 承载避免 uintptr→Pointer 转换的 vet 告警）
	pixels := make([]byte, int(width)*int(height)*4)
	copy(pixels, unsafe.Slice((*byte)(bitsPtr), int(width)*int(height)*4))
	return bgraToRGBA(pixels, int(width), int(height)), nil
}

// bgraToRGBA 把 BGRA 像素缓冲（每像素 4 字节）转换为 RGBA 图像。
func bgraToRGBA(pixels []byte, width, height int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := (y*width + x) * 4
			img.Pix[idx] = pixels[idx+2]   // R
			img.Pix[idx+1] = pixels[idx+1] // G
			img.Pix[idx+2] = pixels[idx]   // B
			img.Pix[idx+3] = 255           // A
		}
	}
	return img
}

// GetFullScreenRect returns the bounding rect of the primary monitor.
func GetFullScreenRect() *Rect {
	w := int32(GetSystemMetrics(0))
	h := int32(GetSystemMetrics(1))
	return &Rect{Left: 0, Top: 0, Right: w, Bottom: h}
}

// captureImage captures a rect to *image.RGBA with fallback logic.
func captureImage(r *Rect, hwnd uintptr) (*image.RGBA, error) {
	width := r.Right - r.Left
	height := r.Bottom - r.Top
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("invalid capture rect: %dx%d", width, height)
	}

	// Method 1 (primary): GetDC(GetDesktopWindow()) — most compatible
	hwndDesktop, _, _ := GetDesktopWindow.Call()
	dc, _, _ := GetDC.Call(hwndDesktop)
	if dc != 0 {
		img, err := captureRectToImage(dc, r)
		ReleaseDC.Call(hwndDesktop, dc)
		if err == nil {
			return img, nil
		}
		primaryErr := err.Error()

		// Method 2 (hwnd available): try PrintWindow for window-specific capture
		if hwnd != 0 {
			hwndDesktop2, _, _ := GetDesktopWindow.Call()
			dc2, _, _ := GetDC.Call(hwndDesktop2)
			if dc2 != 0 {
				memDC, _, _ := CreateCompatibleDC.Call(dc2)
				if memDC != 0 {
					bmiSize := 40 + 1024
					bmi := make([]byte, bmiSize)
					header := (*BITMAPINFOHEADER)(unsafe.Pointer(&bmi[0]))
					header.BiSize = 40
					header.BiWidth = int32(width)
					header.BiHeight = -int32(height)
					header.BiPlanes = 1
					header.BiBitCount = 32
					header.BiCompression = 0
					var bitsPtr unsafe.Pointer
					hbitmap, _, _ := CreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi[0])), DibRgbColors,
						uintptr(unsafe.Pointer(&bitsPtr)), 0, 0)
					if hbitmap != 0 {
						// 选入 memDC 并保存旧位图；删除前先选回旧位图（见 captureRectToImage 同款收尾，R8-D-1）。
						oldBmp, _, _ := SelectObject.Call(memDC, hbitmap)
						pwRet, _, _ := PrintWindow.Call(hwnd, memDC, PwRenderFullContent)
						if pwRet != 0 {
							pixels := make([]byte, int(width)*int(height)*4)
							copy(pixels, unsafe.Slice((*byte)(bitsPtr), int(width)*int(height)*4))
							if oldBmp != 0 {
								SelectObject.Call(memDC, oldBmp)
							}
							DeleteObject.Call(hbitmap)
							DeleteDC.Call(memDC)
							ReleaseDC.Call(hwndDesktop2, dc2)
							return bgraToRGBA(pixels, int(width), int(height)), nil
						}
						if oldBmp != 0 {
							SelectObject.Call(memDC, oldBmp)
						}
						DeleteObject.Call(hbitmap)
					}
					DeleteDC.Call(memDC)
				}
				ReleaseDC.Call(hwndDesktop2, dc2)
			}
		}

		// Method 3 (fallback): CreateDCW("DISPLAY") — works in some environments
		dc3, _, _ := CreateDCW.Call(uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("DISPLAY"))), 0, 0, 0)
		if dc3 != 0 {
			img3, err3 := captureRectToImage(dc3, r)
			DeleteDC.Call(dc3)
			if err3 == nil {
				return img3, nil
			}
			return nil, fmt.Errorf("all capture methods failed: GetDC: %s; CreateDCW: %s", primaryErr, err3.Error())
		}
		return nil, fmt.Errorf("capture failed: %s (CreateDCW also failed)", primaryErr)
	}

	// Fallback when GetDC fails entirely: try CreateDCW directly
	dc4, _, _ := CreateDCW.Call(uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("DISPLAY"))), 0, 0, 0)
	if dc4 == 0 {
		return nil, fmt.Errorf("GetDC and CreateDCW both failed")
	}
	img, err := captureRectToImage(dc4, r)
	DeleteDC.Call(dc4)
	if err != nil {
		return nil, fmt.Errorf("capture rect image: %w", err)
	}
	return img, nil
}
