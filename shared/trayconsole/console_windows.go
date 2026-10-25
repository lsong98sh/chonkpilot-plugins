//go:build windows

package trayconsole

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetFocus            = user32.NewProc("SetFocus")
	procMoveWindow          = user32.NewProc("MoveWindow")
	procGetClientRect       = user32.NewProc("GetClientRect")
	procSendMessageW        = user32.NewProc("SendMessageW")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procCreateFontW         = gdi32.NewProc("CreateFontW")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procCreateSolidBrush    = gdi32.NewProc("CreateSolidBrush")
	procSetTextColor        = gdi32.NewProc("SetTextColor")
	procSetBkColor          = gdi32.NewProc("SetBkColor")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
)

const (
	nimAdd    = 0x00000000
	nimDelete = 0x00000002
	nifMsg    = 0x00000001
	nifIcon   = 0x00000002
	nifTip    = 0x00000004

	wmApp       = 0x8000
	wmTray      = wmApp + 1 // 托盘回调
	wmAppend    = wmApp + 2 // 追加输出
	wmCloseApp  = wmApp + 3 // 真正退出（移除托盘 + 销毁窗口）
	wmNull      = 0x0000
	wmClose     = 0x0010
	wmDestroy   = 0x0002
	wmSize      = 0x0005
	wmSetText   = 0x000C
	wmSetFont   = 0x0030
	wmCtlColor  = 0x0133 // WM_CTLCOLOREDIT
	wmCtlColorS = 0x0138 // WM_CTLCOLORSTATIC（只读 EDIT 走这条）
	wmLButtonUp = 0x0202
	wmLButtonD = 0x0203
	wmRButtonUp = 0x0205

	emSetSel        = 0x00B1
	emScrollCaret   = 0x00B7
	emLineIndex     = 0x00BB
	emGetLineCount  = 0x00BA
	emReplaceSel    = 0x00C2
	emSetMargins    = 0x00D3
	ecLeftRightMarg = 0x0003

	wsOverlappedWindow = 0x00CF0000
	wsFixedWindow      = 0x00CA0000 // WS_CAPTION|WS_SYSMENU|WS_MINIMIZEBOX（无 thick frame / 无最大化）
	wsClipChildren     = 0x02000000
	wsChild            = 0x40000000
	wsVisible          = 0x10000000
	wsVScroll          = 0x00200000
	esMultiline        = 0x0004
	esAutoVScroll      = 0x0040
	esNoHideSel        = 0x0100
	esReadOnly         = 0x0800
	wsExClientEdge     = 0x00000200

	swHide       = 0
	swShowNormal = 1

	csDblClks      = 0x0008
	csGlobalClass  = 0x4000
	idiApplication = 32512

	// SPI_GETWORKAREA：取桌面工作区（扣除任务栏），用于窗口居中。
	spiGetWorkArea = 0x0030

	// ERROR_CLASS_ALREADY_EXISTS：同名全局窗口类已注册（= 已有实例）。
	errorClassAlreadyExists = 1410

	// 固定窗口尺寸（用户 2026-10-19 定稿：固定大小、弹出始终居中）。
	windowWidth  = 960
	windowHeight = 600

	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmRightButton = 0x00000002
	tpmReturnCmd   = 0x00000100

	menuOpen   = 1
	menuClear  = 2
	menuHide   = 3
	maxTipLen  = 127
	maxBatchLn = 200 // 单次 UI 追加行数上限（避免超长消息）
)

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct{ X, Y int32 }

type rect struct{ Left, Top, Right, Bottom int32 }

type msgStruct struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type notifyIconDataW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// platformState 是 Windows 下的窗口 / 控件 / GDI / 托盘句柄。
type platformState struct {
	hwnd   uintptr
	hEdit  uintptr
	hFont  uintptr
	hBrush uintptr
	hIcon  uintptr
	nid    notifyIconDataW
}

var (
	consolesMu sync.Mutex
	consoles   = map[uintptr]*Console{}
	wndProcCB  = syscall.NewCallback(consoleProc)
)

// isErrno 判定 syscall 返回的错误码（LazyProc.Call 的 err 为 syscall.Errno）。
func isErrno(err error, code int) bool {
	e, ok := err.(syscall.Errno)
	return ok && int(e) == code
}

func utf16Ptr(s string) *uint16 {
	s = strings.ReplaceAll(s, "\x00", "")
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

// findWindow 查找当前桌面上该类名的顶层窗口（0 = 不存在）。
func findWindow(className *uint16) uintptr {
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(className)), 0)
	return h
}

func utf16Into(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(strings.ReplaceAll(s, "\x00", ""))
	n := copy(dst[:len(dst)-1], u)
	dst[n] = 0
}

// Start 创建托盘图标与（隐藏的）输出窗口，返回可写入的 Console。
// 创建失败（注册窗口类 / 建窗 / Shell_NotifyIcon）→ 返回错误（调用方回落为仅 stderr）。
func Start(opts Options) (*Console, error) {
	c := &Console{
		opts:    normalizeOptions(opts),
		closeCh: make(chan struct{}),
	}
	ready := make(chan error, 1)
	go func() {
		// Win32 窗口 + 消息循环必须同线程（LockOSThread）。
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := c.setup(); err != nil {
			ready <- err
			return
		}
		ready <- nil
		c.loop()
		c.cleanup()
		close(c.closeCh)
	}()
	if err := <-ready; err != nil {
		return nil, err
	}
	for _, line := range c.opts.Header {
		_, _ = c.Write([]byte(line + "\n"))
	}
	return c, nil
}

// setup 在锁定线程内建窗（窗口默认不可见 = 启动 hide）。
//
// SingleInstance=true → **窗口类 + 同名窗口**双判据（2026-10-19 实测：现代 Windows 的
// CS_GLOBALCLASS 注册**不跨进程冲突**，仅靠 RegisterClassExW 返回 1410 无法判定）：
//   1. 建窗前 `FindWindowW(<类名>)` 命中 → 已有实例 → ErrAlreadyRunning；
//   2. 建窗后复查：命中窗口不是自己 → 竞争失败，销毁自己的窗口并返回 ErrAlreadyRunning。
// 极小概率的「同时启动」竞争由第 2 步收敛为恰好一个存活；剩余损耗由 pluginhost 重启退避兜底。
func (c *Console) setup() error {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr(c.opts.ClassName)
	style := uintptr(csDblClks)
	wcInst := hInst
	if c.opts.SingleInstance {
		style |= csGlobalClass
		wcInst = 0 // 全局窗口类要求 hInstance=NULL
	}
	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		Style:         uint32(style),
		LpfnWndProc:   wndProcCB,
		HInstance:     wcInst,
		LpszClassName: className,
	}
	if ret, _, regErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		if c.opts.SingleInstance && isErrno(regErr, errorClassAlreadyExists) {
			return ErrAlreadyRunning
		}
		return fmt.Errorf("RegisterClassExW: %v", regErr)
	}
	if c.opts.SingleInstance {
		if h := findWindow(className); h != 0 {
			return ErrAlreadyRunning
		}
	}
	// 主窗口：不建 WS_VISIBLE → 启动即隐藏（仅托盘可见）；固定尺寸（不可缩放）。
	hwnd, _, werr := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16Ptr(c.opts.Title))),
		uintptr(wsFixedWindow|wsClipChildren),
		0x80000000 /*CW_USEDEFAULT*/, 0x80000000, windowWidth, windowHeight,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowExW(main): %v", werr)
	}
	if c.opts.SingleInstance {
		if h := findWindow(className); h != 0 && h != hwnd {
			procDestroyWindow.Call(hwnd)
			return ErrAlreadyRunning
		}
	}

	// 只读多行 EDIT（控制台式输出）。
	hEdit, _, eerr := procCreateWindowExW.Call(
		uintptr(wsExClientEdge), uintptr(unsafe.Pointer(utf16Ptr("EDIT"))), 0,
		uintptr(wsChild|wsVisible|wsVScroll|esMultiline|esReadOnly|esAutoVScroll|esNoHideSel),
		0, 0, windowWidth, windowHeight, hwnd, 0, hInst, 0,
	)
	if hEdit == 0 {
		procDestroyWindow.Call(hwnd)
		return fmt.Errorf("CreateWindowExW(edit): %v", eerr)
	}

	fontHeight := int32(-13)
	hFont, _, _ := procCreateFontW.Call(
		uintptr(fontHeight), 0, 0, 0, 400, 0, 0, 0,
		1 /*DEFAULT_CHARSET*/, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(utf16Ptr("Consolas"))),
	)
	if hFont != 0 {
		procSendMessageW.Call(hEdit, wmSetFont, hFont, 1)
	}
	procSendMessageW.Call(hEdit, emSetMargins, ecLeftRightMarg, uintptr(0x00080008))

	hBrush, _, _ := procCreateSolidBrush.Call(0x001E1E1E) // RGB(30,30,30)

	c.st.hwnd = hwnd
	c.st.hEdit = hEdit
	c.st.hFont = hFont
	c.st.hBrush = hBrush
	c.st.hIcon, _, _ = procLoadIconW.Call(0, idiApplication)

	consolesMu.Lock()
	consoles[hwnd] = c
	consolesMu.Unlock()

	// 首次布局（隐藏状态下也要摆好子控件，显示时立即正确）。
	c.layout()

	// 托盘图标（点击 → 显示窗口）。
	c.st.nid = notifyIconDataW{
		CbSize:           uint32(unsafe.Sizeof(notifyIconDataW{})),
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifMsg | nifIcon | nifTip,
		UCallbackMessage: wmTray,
		HIcon:            c.st.hIcon,
	}
	tip := c.opts.Tooltip
	if len([]rune(tip)) > maxTipLen {
		tip = string([]rune(tip)[:maxTipLen])
	}
	utf16Into(c.st.nid.SzTip[:], tip)
	if ret, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&c.st.nid))); ret == 0 {
		procDestroyWindow.Call(hwnd)
		return fmt.Errorf("Shell_NotifyIconW(NIM_ADD): %v", err)
	}
	return nil
}

// loop 跑消息循环（仅 WM_QUIT 返回）。
func (c *Console) loop() {
	var m msgStruct
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) == 0 || int32(ret) == -1 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// cleanup 回收 GDI 资源（托盘图标与窗口已在消息循环内销毁）。
func (c *Console) cleanup() {
	if c.st.hFont != 0 {
		procDeleteObject.Call(c.st.hFont)
	}
	if c.st.hBrush != 0 {
		procDeleteObject.Call(c.st.hBrush)
	}
	consolesMu.Lock()
	delete(consoles, c.st.hwnd)
	consolesMu.Unlock()
}

// layout 调整 EDIT 铺满客户区。
func (c *Console) layout() {
	if c.st.hwnd == 0 || c.st.hEdit == 0 {
		return
	}
	var rc rect
	procGetClientRect.Call(c.st.hwnd, uintptr(unsafe.Pointer(&rc)))
	procMoveWindow.Call(c.st.hEdit, 0, 0, uintptr(rc.Right-rc.Left), uintptr(rc.Bottom-rc.Top), 1)
}

// show 显示并置前输出窗口（每次弹出都回到屏幕中央；固定尺寸）。
func (c *Console) show() {
	if c.st.hwnd == 0 {
		return
	}
	c.centerWindow()
	procShowWindow.Call(c.st.hwnd, swShowNormal)
	c.layout()
	procSetForegroundWindow.Call(c.st.hwnd)
	procSetFocus.Call(c.st.hEdit)
}

// centerWindow 把固定尺寸窗口摆到桌面工作区正中（扣除任务栏）。
func (c *Console) centerWindow() {
	var rc rect
	ok, _, _ := procSystemParametersInfoW.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&rc)), 0)
	if ok == 0 {
		sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
		sh, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
		rc = rect{0, 0, int32(sw), int32(sh)}
	}
	areaW := rc.Right - rc.Left
	areaH := rc.Bottom - rc.Top
	x := rc.Left + (areaW-windowWidth)/2
	y := rc.Top + (areaH-windowHeight)/2
	procMoveWindow.Call(c.st.hwnd, uintptr(x), uintptr(y), windowWidth, windowHeight, 1)
}

// hide 收起回托盘。
func (c *Console) hide() {
	if c.st.hwnd != 0 {
		procShowWindow.Call(c.st.hwnd, swHide)
	}
}

// clear 清空输出。
func (c *Console) clear() {
	if c.st.hEdit != 0 {
		procSendMessageW.Call(c.st.hEdit, wmSetText, 0, uintptr(unsafe.Pointer(utf16Ptr(""))))
	}
}

// drain 在 UI 线程把待输出行追加到 EDIT（自动裁剪到 MaxLines 并滚到底）。
func (c *Console) drain() {
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.posted = false
		c.mu.Unlock()
		return
	}
	n := len(c.pending)
	if n > maxBatchLn {
		n = maxBatchLn
	}
	lines := c.pending[:n]
	c.pending = c.pending[n:]
	more := len(c.pending) > 0
	if !more {
		c.posted = false
	}
	c.mu.Unlock()
	if c.st.hEdit == 0 {
		return
	}

	text := strings.Join(lines, "\r\n") + "\r\n"
	procSendMessageW.Call(c.st.hEdit, emSetSel, ^uintptr(0), ^uintptr(0))
	procSendMessageW.Call(c.st.hEdit, emReplaceSel, 0, uintptr(unsafe.Pointer(utf16Ptr(text))))

	// 超过上限 → 删除最旧行。
	cnt, _, _ := procSendMessageW.Call(c.st.hEdit, emGetLineCount, 0, 0)
	count := int32(cnt)
	if int(count) > c.opts.MaxLines {
		del := int(count) - c.opts.MaxLines
		idx, _, _ := procSendMessageW.Call(c.st.hEdit, emLineIndex, uintptr(del), 0)
		if idx != ^uintptr(0) {
			procSendMessageW.Call(c.st.hEdit, emSetSel, 0, idx)
			procSendMessageW.Call(c.st.hEdit, emReplaceSel, 0, uintptr(unsafe.Pointer(utf16Ptr(""))))
		}
	}

	// 滚到底 + 光标置尾（便于 Ctrl+C / Ctrl+A）。
	procSendMessageW.Call(c.st.hEdit, emSetSel, ^uintptr(0), ^uintptr(0))
	procSendMessageW.Call(c.st.hEdit, emScrollCaret, 0, 0)

	if more {
		procPostMessageW.Call(c.st.hwnd, wmAppend, 0, 0)
	}
}

// showMenu 弹出托盘右键菜单。
func (c *Console) showMenu() {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)
	procAppendMenuW.Call(hMenu, mfString, menuOpen, uintptr(unsafe.Pointer(utf16Ptr("打开输出窗口"))))
	procAppendMenuW.Call(hMenu, mfString, menuClear, uintptr(unsafe.Pointer(utf16Ptr("清空输出"))))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, menuHide, uintptr(unsafe.Pointer(utf16Ptr("隐藏窗口"))))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(c.st.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmReturnCmd, uintptr(pt.X), uintptr(pt.Y), 0, c.st.hwnd, 0)
	procPostMessageW.Call(c.st.hwnd, wmNull, 0, 0)
	switch cmd {
	case menuOpen:
		c.show()
	case menuClear:
		c.clear()
	case menuHide:
		c.hide()
	}
}

// consoleProc 是窗口过程（经 registry 按 hwnd 分发）。
func consoleProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	consolesMu.Lock()
	c := consoles[hwnd]
	consolesMu.Unlock()

	switch msg {
	case wmTray:
		if c == nil {
			return 0
		}
		switch uint32(lparam) & 0xFFFF {
		case wmLButtonUp, wmLButtonD:
			c.show()
		case wmRButtonUp:
			c.showMenu()
		}
		return 0
	case wmAppend:
		if c != nil {
			c.drain()
		}
		return 0
	case wmSize:
		if c != nil {
			c.layout()
		}
		return 0
	case wmCtlColor, wmCtlColorS:
		if c != nil && lparam == c.st.hEdit {
			procSetTextColor.Call(wparam, 0x00DCDCDC)
			procSetBkColor.Call(wparam, 0x001E1E1E)
			return c.st.hBrush
		}
	case wmClose:
		// 关闭 = 收起回托盘（进程继续常驻）。
		if c != nil {
			c.hide()
		}
		return 0
	case wmCloseApp:
		if c != nil {
			procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&c.st.nid)))
		}
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

// postAppend / postClose 是跨线程投递（Write / Close 在任意 goroutine）。
func postAppend(hwnd uintptr) { procPostMessageW.Call(hwnd, wmAppend, 0, 0) }
func postClose(hwnd uintptr)  { procPostMessageW.Call(hwnd, wmCloseApp, 0, 0) }
