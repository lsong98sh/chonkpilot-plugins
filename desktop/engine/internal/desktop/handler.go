package desktop

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ─── Win32 API types and constants ─────────────────────────

var (
	User32 = windows.NewLazySystemDLL("user32.dll")
	Gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	// user32
	SendInput             = User32.NewProc("SendInput")
	PeekMessageW          = User32.NewProc("PeekMessageW")
	MapVirtualKey         = User32.NewProc("MapVirtualKeyW")
	FindWindowW           = User32.NewProc("FindWindowW")
	EnumWindows           = User32.NewProc("EnumWindows")
	GetWindowTextW        = User32.NewProc("GetWindowTextW")
	GetWindowTextLengthW  = User32.NewProc("GetWindowTextLengthW")
	GetWindowRect         = User32.NewProc("GetWindowRect")
	SetWindowPos          = User32.NewProc("SetWindowPos")
	ShowWindow            = User32.NewProc("ShowWindow")
	GetDesktopWindow      = User32.NewProc("GetDesktopWindow")
	GetDC                 = User32.NewProc("GetDC")
	ReleaseDC             = User32.NewProc("ReleaseDC")
	GetForegroundWindow   = User32.NewProc("GetForegroundWindow")
	SetForegroundWindow   = User32.NewProc("SetForegroundWindow")
	GetClassNameW         = User32.NewProc("GetClassNameW")
	GetSystemMetricsProc  = User32.NewProc("GetSystemMetrics")
	PrintWindow           = User32.NewProc("PrintWindow")
	GetClientRect         = User32.NewProc("GetClientRect")
	ClientToScreen        = User32.NewProc("ClientToScreen")

	// gdi32
	CreateDCW          = Gdi32.NewProc("CreateDCW")
	CreateCompatibleDC = Gdi32.NewProc("CreateCompatibleDC")
	CreateDIBSection   = Gdi32.NewProc("CreateDIBSection")
	SelectObject       = Gdi32.NewProc("SelectObject")
	BitBlt             = Gdi32.NewProc("BitBlt")
	DeleteDC           = Gdi32.NewProc("DeleteDC")
	DeleteObject       = Gdi32.NewProc("DeleteObject")
)

const (
	// SendInput types
	InputMouse    = 0
	InputKeyboard = 1

	// Mouse event flags
	MouseEventMove       = 0x0001
	MouseEventLeftDown   = 0x0002
	MouseEventLeftUp     = 0x0004
	MouseEventRightDown  = 0x0008
	MouseEventRightUp    = 0x0010
	MouseEventMiddleDown = 0x0020
	MouseEventMiddleUp   = 0x0040
	MouseEventWheel      = 0x0800
	MouseEventHWheel     = 0x1000
	MouseEventAbsolute   = 0x8000

	// Keyboard event flags
	KeyEventKeyDown  = 0x0000
	KeyEventKeyUp    = 0x0002
	KeyEventExtended = 0x0001
	KeyEventScanCode = 0x0008
	KeyEventUnicode  = 0x0004

	// Window flags
	SwpNoZOrder = 0x0004
	SwMaximize  = 3
	SwMinimize  = 6
	SwRestore   = 9

	// GDI
	SrcCopy      = 0x00CC0020
	DibRgbColors = 0

	// PrintWindow flags
	PwRenderFullContent = 2
)

// Rect is a window rectangle.
type Rect struct {
	Left, Top, Right, Bottom int32
}

// MouseInput struct for SendInput.
type MouseInput struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// KeyboardInput struct for SendInput.
type KeyboardInput struct {
	WVk       uint16
	WScan     uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// VKMap maps key names to virtual key codes.
var VKMap = map[string]uint16{
	"backspace":   0x08,
	"tab":         0x09,
	"enter":       0x0D,
	"shift":       0x10,
	"ctrl":        0x11,
	"control":     0x11,
	"alt":         0x12,
	"pause":       0x13,
	"capslock":    0x14,
	"escape":      0x1B,
	"esc":         0x1B,
	"space":       0x20,
	"pageup":      0x21,
	"pagedown":    0x22,
	"end":         0x23,
	"home":        0x24,
	"left":        0x25,
	"up":          0x26,
	"right":       0x27,
	"down":        0x28,
	"delete":      0x2E,
	"del":         0x2E,
	"insert":      0x2D,
	"ins":         0x2D,
	"printscreen": 0x2C,
	"prtsc":       0x2C,
	"snapshot":    0x2C,
	"0":           0x30,
	"1":           0x31,
	"2":           0x32,
	"3":           0x33,
	"4":           0x34,
	"5":           0x35,
	"6":           0x36,
	"7":           0x37,
	"8":           0x38,
	"9":           0x39,
	"a":           0x41,
	"b":           0x42,
	"c":           0x43,
	"d":           0x44,
	"e":           0x45,
	"f":           0x46,
	"g":           0x47,
	"h":           0x48,
	"i":           0x49,
	"j":           0x4A,
	"k":           0x4B,
	"l":           0x4C,
	"m":           0x4D,
	"n":           0x4E,
	"o":           0x4F,
	"p":           0x50,
	"q":           0x51,
	"r":           0x52,
	"s":           0x53,
	"t":           0x54,
	"u":           0x55,
	"v":           0x56,
	"w":           0x57,
	"x":           0x58,
	"y":           0x59,
	"z":           0x5A,
	"lwin":        0x5B,
	"rwin":        0x5C,
	"numpad0":     0x60,
	"numpad1":     0x61,
	"numpad2":     0x62,
	"numpad3":     0x63,
	"numpad4":     0x64,
	"numpad5":     0x65,
	"numpad6":     0x66,
	"numpad7":     0x67,
	"numpad8":     0x68,
	"numpad9":     0x69,
	"multiply":    0x6A,
	"add":         0x6B,
	"subtract":    0x6D,
	"decimal":     0x6E,
	"divide":      0x6F,
	"f1":          0x70,
	"f2":          0x71,
	"f3":          0x72,
	"f4":          0x73,
	"f5":          0x74,
	"f6":          0x75,
	"f7":          0x76,
	"f8":          0x77,
	"f9":          0x78,
	"f10":         0x79,
	"f11":         0x7A,
	"f12":         0x7B,
	"numlock":     0x90,
	"scrolllock":  0x91,
	"lshift":      0xA0,
	"rshift":      0xA1,
	"lctrl":       0xA2,
	"rctrl":       0xA3,
	"lalt":        0xA4,
	"ralt":        0xA5,
	"semicolon":   0xBA,
	"plus":        0xBB,
	"comma":       0xBC,
	"minus":       0xBD,
	"period":      0xBE,
	"slash":       0xBF,
	"tilde":       0xC0,
	"lbracket":    0xDB,
	"backslash":   0xDC,
	"rbracket":    0xDD,
	"quote":       0xDE,
}

// SendKey sends a key event via SendInput.
func SendKey(vk uint16, up bool) bool {
	flags := uint32(KeyEventKeyDown)
	if up {
		flags = KeyEventKeyUp
	}

	scan, _, _ := MapVirtualKey.Call(uintptr(vk), 0)
	scanCode := uint16(scan & 0xFFFF)
	if (scan>>16)&0xFF == 0xE0 {
		flags |= KeyEventExtended
	}

	ki := KeyboardInput{WVk: vk, WScan: scanCode, Flags: flags}
	return SendInputRaw(InputKeyboard, unsafe.Pointer(&ki), int32(unsafe.Sizeof(ki)))
}

// SendInputRaw sends raw input via SendInput.
func SendInputRaw(inputType uint32, data unsafe.Pointer, size int32) bool {
	ensureMsgQueue()

	type inputUnion struct {
		_ [32]byte
	}
	type fullInput struct {
		typ   uint32
		_     [4]byte
		union inputUnion
	}

	var buf fullInput
	buf.typ = inputType
	if size > 32 {
		size = 32
	}
	dstPtr := unsafe.Pointer(&buf.union)
	for i := int32(0); i < size; i++ {
		src := *(*byte)(unsafe.Add(data, i))
		*(*byte)(unsafe.Add(dstPtr, i)) = src
	}

	ret, _, _ := SendInput.Call(1, uintptr(unsafe.Pointer(&buf)), unsafe.Sizeof(buf))
	return ret != 0
}

// SendMouseEvent sends a mouse event via SendInput.
func SendMouseEvent(flags uint32, dx, dy int32, mouseData uint32) {
	mi := MouseInput{
		Dx:        dx,
		Dy:        dy,
		MouseData: mouseData,
		Flags:     flags,
	}
	SendInputRaw(InputMouse, unsafe.Pointer(&mi), int32(unsafe.Sizeof(mi)))
}

// CharToVK converts an ASCII character to its Windows VK code.
func CharToVK(ch byte) (vk uint16, needsShift bool) {
	switch {
	case ch >= 'a' && ch <= 'z':
		return uint16(ch - 0x20), false
	case ch >= 'A' && ch <= 'Z':
		return uint16(ch), true
	case ch >= '0' && ch <= '9':
		return uint16(ch), false
	case ch == ' ':
		return 0x20, false
	case ch == '\t':
		return 0x09, false
	case ch == '\n' || ch == '\r':
		return 0x0D, false
	case ch == '.':
		return 0xBE, false
	case ch == ',':
		return 0xBC, false
	default:
		return uint16(ch), false
	}
}

// EnsureMsgQueue ensures the calling thread has a Windows message queue.
func ensureMsgQueue() {
	var msg [48]byte
	PeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
}

// DesktopPressMods presses modifier keys and returns a release function.
func DesktopPressMods(vks []uint16) func() {
	for _, vk := range vks {
		SendKey(vk, false)
	}
	return func() {
		for i := len(vks) - 1; i >= 0; i-- {
			SendKey(vks[i], true)
		}
	}
}

// SendKeyUnicode sends a Unicode character via SendInput.
func SendKeyUnicode(ch uint16, up bool) bool {
	flags := uint32(KeyEventUnicode)
	if up {
		flags |= KeyEventKeyUp
	}
	ki := KeyboardInput{WVk: 0, WScan: ch, Flags: flags}
	return SendInputRaw(InputKeyboard, unsafe.Pointer(&ki), int32(unsafe.Sizeof(ki)))
}

// GetSystemMetrics returns the specified system metric.
func GetSystemMetrics(index int) int {
	ret, _, _ := GetSystemMetricsProc.Call(uintptr(index))
	return int(ret)
}
