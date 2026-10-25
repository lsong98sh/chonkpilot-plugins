package w32

import (
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32               = windows.NewLazySystemDLL("ole32")
	Ole32CoInitializeEx = ole32.NewProc("CoInitializeEx")

	kernel32                   = windows.NewLazySystemDLL("kernel32")
	Kernel32GetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")

	shlwapi                  = windows.NewLazySystemDLL("shlwapi")
	shlwapiSHCreateMemStream = shlwapi.NewProc("SHCreateMemStream")

	user32                   = windows.NewLazySystemDLL("user32")
	User32LoadImageW         = user32.NewProc("LoadImageW")
	User32GetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	User32RegisterClassExW   = user32.NewProc("RegisterClassExW")
	User32CreateWindowExW    = user32.NewProc("CreateWindowExW")
	User32DestroyWindow      = user32.NewProc("DestroyWindow")
	User32ShowWindow         = user32.NewProc("ShowWindow")
	User32UpdateWindow       = user32.NewProc("UpdateWindow")
	User32SetFocus           = user32.NewProc("SetFocus")
	User32GetMessageW        = user32.NewProc("GetMessageW")
	User32TranslateMessage   = user32.NewProc("TranslateMessage")
	User32DispatchMessageW   = user32.NewProc("DispatchMessageW")
	User32DefWindowProcW     = user32.NewProc("DefWindowProcW")
	User32GetClientRect      = user32.NewProc("GetClientRect")
	User32PostQuitMessage    = user32.NewProc("PostQuitMessage")
	User32PostMessageW       = user32.NewProc("PostMessageW")
	User32SetWindowTextW     = user32.NewProc("SetWindowTextW")
	User32PostThreadMessageW = user32.NewProc("PostThreadMessageW")
	User32GetWindowLongW     = user32.NewProc("GetWindowLongW")
	User32GetWindowLongPtrW  = user32.NewProc("GetWindowLongPtrW")
	User32SetWindowLongW     = user32.NewProc("SetWindowLongW")
	User32SetWindowLongPtrW  = user32.NewProc("SetWindowLongPtrW")
	User32AdjustWindowRect   = user32.NewProc("AdjustWindowRect")
	User32SetWindowPos       = user32.NewProc("SetWindowPos")
	User32IsDialogMessage    = user32.NewProc("IsDialogMessage")
	User32GetAncestor        = user32.NewProc("GetAncestor")
	User32GetWindowRect      = user32.NewProc("GetWindowRect")
	User32IsZoomed           = user32.NewProc("IsZoomed")
	User32CallWindowProcW    = user32.NewProc("CallWindowProcW")
	User32GetWindow          = user32.NewProc("GetWindow")
	User32MonitorFromRect    = user32.NewProc("MonitorFromRect")
	User32GetMonitorInfoW    = user32.NewProc("GetMonitorInfoW")
	User32ReleaseCapture     = user32.NewProc("ReleaseCapture")
	User32SendMessageW       = user32.NewProc("SendMessageW")
)

const (
	SM_CXSCREEN       = 0
	SM_CYSCREEN       = 1
	SM_CXSIZEFRAME    = 32
	SM_CYSIZEFRAME    = 33
	SM_CXPADDEDBORDER = 92
)

const (
	CW_USEDEFAULT = 0x80000000
)

const (
	LR_DEFAULTCOLOR     = 0x0000
	LR_MONOCHROME       = 0x0001
	LR_LOADFROMFILE     = 0x0010
	LR_LOADTRANSPARENT  = 0x0020
	LR_DEFAULTSIZE      = 0x0040
	LR_VGACOLOR         = 0x0080
	LR_LOADMAP3DCOLORS  = 0x1000
	LR_CREATEDIBSECTION = 0x2000
	LR_SHARED           = 0x8000
)

const (
	SystemMetricsCxIcon = 11
	SystemMetricsCyIcon = 12
)

const (
	SWShow = 5
	SWHide = 0
)

const (
	SWPNoZOrder       = 0x0004
	SWPNoActivate     = 0x0010
	SWPNoMove         = 0x0002
	SWPFrameChanged   = 0x0020
	SWPNoOwnerZOrder  = 0x0200
	SWPNoSendChanging = 0x0400
)

const (
	WMDestroy       = 0x0002
	WMMove          = 0x0003
	WMSize          = 0x0005
	WMActivate      = 0x0006
	WMClose         = 0x0010
	WMQuit          = 0x0012
	WMGetMinMaxInfo = 0x0024
	WMNCCreate      = 0x0081
	WMNCCalcSize    = 0x0083
	WMNCHitTest     = 0x0084
	WMNCLButtonDown = 0x00A1
	WMMoving        = 0x0216
	WMApp           = 0x8000
)

// WM_NCHITTEST return values
const (
	HTClient      = 1
	HTCaption     = 2
	HTLeft        = 10
	HTRight       = 11
	HTTop         = 12
	HTTopLeft     = 13
	HTTopRight    = 14
	HTBottom      = 15
	HTBottomLeft  = 16
	HTBottomRight = 17
	HTBorder      = 18
	// HTTransparent (-1): 光标所在窗口对本次命中测试透明，
	// 系统向父窗口重新发送 WM_NCHITTEST（负值以 uintptr 位模式表达）。
	HTTransparent = ^uintptr(0)
)

const (
	GAParent    = 1
	GARoot      = 2
	GARootOwner = 3
)

const (
	MonitorDefaultToNull    = 0x00000000
	MonitorDefaultToPrimary = 0x00000001
	MonitorDefaultToNearest = 0x00000002
)

const (
	GWLStyle    = -16
	GWLPWndProc = ^uintptr(3) // -4，负索引以 uintptr 位模式表达
)

const (
	GWHWndNext = 2
	GWChild    = 5
)

const (
	WSOverlapped       = 0x00000000
	WSMaximizeBox      = 0x00010000
	WSThickFrame       = 0x00040000
	WSCaption          = 0x00C00000
	WSSysMenu          = 0x00080000
	WSMinimizeBox      = 0x00020000
	WSOverlappedWindow = (WSOverlapped | WSCaption | WSSysMenu | WSThickFrame | WSMinimizeBox | WSMaximizeBox)
)

const (
	WAInactive    = 0
	WAActive      = 1
	WAActiveClick = 2
)

type WndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CnClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type Rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type MonitorInfo struct {
	CbSize    uint32
	RcMonitor Rect
	RcWork    Rect
	DwFlags   uint32
}

// CreateStructW is the CREATESTRUCTW layout delivered with WM_NCCREATE.
// Only the fields we use (lpCreateParams) are strictly needed, but the
// full layout keeps the pointer offsets correct.
type CreateStructW struct {
	LpCreateParams uintptr
	HInstance      windows.Handle
	HMenu          windows.Handle
	HWndParent     windows.Handle
	Cy             int32
	Cx             int32
	Y              int32
	X              int32
	Style          int32
	LpszName       *uint16
	LpszClass      *uint16
	DwExStyle      uint32
}

type MinMaxInfo struct {
	PtReserved     Point
	PtMaxSize      Point
	PtMaxPosition  Point
	PtMinTrackSize Point
	PtMaxTrackSize Point
}

type Point struct {
	X, Y int32
}

type Msg struct {
	Hwnd     syscall.Handle
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       Point
	LPrivate uint32
}

func Utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	// Find NUL terminator.
	end := unsafe.Pointer(p)
	n := 0
	for *(*uint16)(end) != 0 {
		end = unsafe.Pointer(uintptr(end) + unsafe.Sizeof(*p))
		n++
	}
	s := (*[(1 << 30) - 1]uint16)(unsafe.Pointer(p))[:n:n]
	return string(utf16.Decode(s))
}

func SHCreateMemStream(data []byte) (uintptr, error) {
	// SHCreateMemStream(NULL, 0) creates an empty stream (e.g. for
	// CapturePreview to write into); a nil data slice must map to NULL.
	var p uintptr
	if len(data) > 0 {
		p = uintptr(unsafe.Pointer(&data[0]))
	}
	ret, _, err := shlwapiSHCreateMemStream.Call(
		p,
		uintptr(len(data)),
	)
	if ret == 0 {
		return 0, err
	}

	return ret, nil
}
