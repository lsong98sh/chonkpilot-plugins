//go:build windows
// +build windows

package webview2

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/internal/w32"
	"github.com/jchv/go-webview2/pkg/edge"

	"golang.org/x/sys/windows"
)

var (
	windowContext     = map[uintptr]interface{}{}
	windowContextSync sync.RWMutex
)

func getWindowContext(wnd uintptr) interface{} {
	windowContextSync.RLock()
	defer windowContextSync.RUnlock()
	return windowContext[wnd]
}

func setWindowContext(wnd uintptr, data interface{}) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	windowContext[wnd] = data
}

// delWindowContext 从 windowContext 移除条目（D-28）：窗口销毁（WM_DESTROY）时必须清理，
// 否则多窗口反复开关会永久泄漏 *webview（含 dispatchq）直至进程退出。
func delWindowContext(wnd uintptr) {
	windowContextSync.Lock()
	defer windowContextSync.Unlock()
	delete(windowContext, wnd)
}

type browser interface {
	Embed(hwnd uintptr) bool
	Resize()
	Navigate(url string)
	NavigateToString(htmlContent string)
	Init(script string)
	Eval(script string) error
	NotifyParentWindowPositionChanged() error
	Focus()
}

type webview struct {
	hwnd       uintptr
	mainthread uintptr
	browser    browser
	autofocus  bool
	hidden     bool
	frameless  bool
	// owner 是 CreateWindowExW 的 hwndParent（PRODSIM-1）：非 0 时本窗口成为该窗口的
	// owned window —— 随 owner 最小化/销毁、始终位于 owner 之上。
	owner uintptr
	// hideOnClose 为 true 时 WM_CLOSE 只隐藏窗口（不 DestroyWindow）—— 日志窗口用
	// （关闭 = 收回托盘，进程继续运行）。
	hideOnClose bool
	maxsz       w32.Point
	minsz       w32.Point
	m           sync.Mutex
	dispatchq   []func()
}

type WindowOptions struct {
	Title  string
	Width  uint
	Height uint
	IconId uint
	Center bool
}

type WebViewOptions struct {
	Window unsafe.Pointer
	Debug  bool

	// DevToolsDisabled blocks the *user* from opening the DevTools window via
	// keyboard shortcut (F12 / Ctrl+Shift+I) or the default context menu.
	// Independent of Debug: the host can still open DevTools programmatically
	// through the DevTools interface (OpenDevToolsWindow) when this is set.
	DevToolsDisabled bool

	// DataPath specifies the datapath for the WebView2 runtime to use for the
	// browser instance.
	DataPath string

	// AutoFocus will try to keep the WebView2 widget focused when the window
	// is focused.
	AutoFocus bool

	// Hidden keeps the window hidden after creation. Call Show() once the
	// first navigation has completed to avoid a white flash before the
	// webview content is ready.
	Hidden bool

	// Frameless removes the native title bar and borders, so the HTML page
	// can render its own title bar (e.g. via -webkit-app-region: drag).
	Frameless bool

	// Owner 是窗口的 owner（hwndParent）。非 0 时本窗口随 owner 最小化/销毁，
	// 并始终位于 owner 之上（主窗口/小窗口层级，PRODSIM-1）。
	Owner uintptr

	// HideOnClose 为 true 时，用户点关闭只隐藏窗口而不销毁（日志窗口：关闭 = 收回托盘）。
	HideOnClose bool

	// WindowOptions customizes the window that is created to embed the
	// WebView2 widget.
	WindowOptions WindowOptions
}

// New creates a new webview in a new window.
func New(debug bool) WebView { return NewWithOptions(WebViewOptions{Debug: debug}) }

// NewWindow creates a new webview using an existing window.
//
// Deprecated: Use NewWithOptions.
func NewWindow(debug bool, window unsafe.Pointer) WebView {
	return NewWithOptions(WebViewOptions{Debug: debug, Window: window})
}

// NewWithOptions creates a new webview using the provided options.
func NewWithOptions(options WebViewOptions) WebView {
	w := &webview{}
	w.autofocus = options.AutoFocus
	w.hidden = options.Hidden
	w.frameless = options.Frameless
	w.owner = options.Owner
	w.hideOnClose = options.HideOnClose

	chromium := edge.NewChromium()
	chromium.DataPath = options.DataPath
	chromium.SetPermission(edge.CoreWebView2PermissionKindClipboardRead, edge.CoreWebView2PermissionStateAllow)

	w.browser = chromium
	w.mainthread, _, _ = w32.Kernel32GetCurrentThreadID.Call()
	if !w.CreateWithOptions(options.WindowOptions) {
		return nil
	}

	// Settings 获取/设置失败：**不 os.Exit**（单窗口失败不得波及其它窗口、不得跳过收尾），
	// 改为记日志并走既有失败返回路径（返回 nil，由宿主按单窗口失败收敛，与 CreateWithOptions
	// 失败口径一致；D-36）。调用方（gui 侧）已将 nil 视为建窗失败并回滚。
	settings, err := chromium.GetSettings()
	if err != nil {
		log.Printf("failed to get webview2 settings: %v", err)
		return nil
	}
	// disable context menu
	err = settings.PutAreDefaultContextMenusEnabled(options.Debug)
	if err != nil {
		log.Printf("failed to set context menus enabled: %v", err)
		return nil
	}
	// disable developer tools
	err = settings.PutAreDevToolsEnabled(options.Debug && !options.DevToolsDisabled)
	if err != nil {
		log.Printf("failed to set devtools enabled: %v", err)
		return nil
	}
	// Enable `-webkit-app-region: drag` for frameless windows so the HTML
	// toolbar can act as a native draggable title bar (Settings9). The
	// property takes effect on the next navigation.
	//
	// 必须 QueryInterface 取 ICoreWebView2Settings9 再调用（见 GetSettings9 注释）：
	// 经基接口 vtable 直接调扩展槽位会「返回成功但不生效」→ app-region 拖不动窗口。
	if options.Frameless {
		if s9, err := settings.GetSettings9(); err != nil {
			log.Printf("failed to query settings9 (non-client region): %v", err)
		} else if err := s9.PutIsNonClientRegionSupportEnabled(true); err != nil {
			log.Printf("failed to enable non-client region support: %v", err)
		}
	}

	return w
}

func wndproc(hwnd, msg, wp, lp uintptr) uintptr {
	// WM_NCCREATE 是 CreateWindowExW 期间收到的第一个消息，此时 windowContext
	// 尚未注册。CreateWindowExW 的 lpParam 指向 *webview，这里把它登记进
	// windowContext，保证后续消息（包括紧随其后的首个 WM_NCCALCSIZE）都能
	// 读取 frameless 标志 —— 否则首个 NCCALCSIZE 会走 DefWindowProc 画出
	// 原生标题栏，直到下一次触发 NCCALCSIZE（resize/最大化）才消失。
	if msg == w32.WMNCCreate {
		if cs := (*w32.CreateStructW)(unsafe.Pointer(lp)); cs.LpCreateParams != 0 {
			setWindowContext(hwnd, (*webview)(unsafe.Pointer(cs.LpCreateParams)))
		}
	}
	if w, ok := getWindowContext(hwnd).(*webview); ok {
		switch msg {
		case w32.WMMove, w32.WMMoving:
			_ = w.browser.NotifyParentWindowPositionChanged()
		case w32.WMNCLButtonDown:
			_, _, _ = w32.User32SetFocus.Call(w.hwnd)
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		case w32.WMSize:
			w.browser.Resize()
		case w32.WMNCCalcSize:
			// Frameless：无论 wparam 取值（0=移动/普通缩放，1=创建/最大化/
			// 样式变化）都让客户区占满整个窗口以隐藏标准边框（标题栏+边缘）。
			// 只有这里返回 0 才能保证任意路径（含应用层普通 SetWindowPos）
			// 都不会重新画出原生标题栏。最大化时把客户区钳制到监视器工作区，
			// 避免内容盖住任务栏。
			if w.frameless {
				if zoomed, _, _ := w32.User32IsZoomed.Call(hwnd); zoomed != 0 {
					rgrc := (*w32.Rect)(unsafe.Pointer(lp))
					if mon, _, _ := w32.User32MonitorFromRect.Call(uintptr(unsafe.Pointer(rgrc)), w32.MonitorDefaultToNull); mon != 0 {
						var mi w32.MonitorInfo
						mi.CbSize = uint32(unsafe.Sizeof(mi))
						if ok, _, _ := w32.User32GetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
							*rgrc = mi.RcWork
						}
					}
				}
				return 0
			}
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		case w32.WMActivate:
			if wp == w32.WAInactive {
				break
			}
			if w.autofocus {
				w.browser.Focus()
			}
		case w32.WMClose:
			// HideOnClose（PRODSIM-1）：日志窗口关闭 = 收回托盘，**不销毁**（进程继续）。
			if w.hideOnClose {
				_, _, _ = w32.User32ShowWindow.Call(hwnd, w32.SWHide)
				return 0
			}
			_, _, _ = w32.User32DestroyWindow.Call(hwnd)
		case w32.WMDestroy:
			// 窗口销毁：移除 windowContext 条目（D-28，避免多窗口反复开关时 *webview 永久泄漏），
			// 再终止消息循环。
			delWindowContext(hwnd)
			w.Terminate()
		case w32.WMGetMinMaxInfo:
			lpmmi := (*w32.MinMaxInfo)(unsafe.Pointer(lp))
			if w.maxsz.X > 0 && w.maxsz.Y > 0 {
				lpmmi.PtMaxSize = w.maxsz
				lpmmi.PtMaxTrackSize = w.maxsz
			}
			if w.minsz.X > 0 && w.minsz.Y > 0 {
				lpmmi.PtMinTrackSize = w.minsz
			}
		default:
			r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
			return r
		}
		return 0
	}
	r, _, _ := w32.User32DefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// WindowHit 是 frameless 窗口的非客户区命中方位（取值与前端 `gui.window.resize.hit` 一致）。
//
// 该映射与「进入系统 move/resize 模态循环」同属本包封装：宿主只传方位名，
// 无需知道 Win32 WM_NCHITTEST 命中码。
type WindowHit = string

const (
	HitLeft        WindowHit = "left"
	HitRight       WindowHit = "right"
	HitTop         WindowHit = "top"
	HitBottom      WindowHit = "bottom"
	HitTopLeft     WindowHit = "topleft"
	HitTopRight    WindowHit = "topright"
	HitBottomLeft  WindowHit = "bottomleft"
	HitBottomRight WindowHit = "bottomright"
	// HitCaption 进入系统**移动**模态循环（frameless 自绘顶栏拖拽；
	// WebView2 app-region 在 WM_NCCALCSIZE=0 的窗口上实测不生效，故显式走 HTCAPTION）。
	HitCaption WindowHit = "caption"
)

// Win32 WM_NCHITTEST 命中码（HT*）。
const (
	htCaption     uintptr = 2
	htLeft        uintptr = 10
	htRight       uintptr = 11
	htTop         uintptr = 12
	htTopLeft     uintptr = 13
	htTopRight    uintptr = 14
	htBottom      uintptr = 15
	htBottomLeft  uintptr = 16
	htBottomRight uintptr = 17
)

// HitCode 把命中方位映射为 Win32 HT* 命中码；未知方位返回 (0, false)。
func HitCode(hit string) (uintptr, bool) {
	switch hit {
	case HitLeft:
		return htLeft, true
	case HitRight:
		return htRight, true
	case HitTop:
		return htTop, true
	case HitBottom:
		return htBottom, true
	case HitTopLeft:
		return htTopLeft, true
	case HitTopRight:
		return htTopRight, true
	case HitBottomLeft:
		return htBottomLeft, true
	case HitBottomRight:
		return htBottomRight, true
	case HitCaption:
		return htCaption, true
	}
	return 0, false
}

// WindowResizer exposes the ability to start a native window resize drag.
// The frontend performs edge hit-testing (hover cursor + mousedown on a
// resize handle) and calls StartResize with the matching HT* code; the
// system then runs its native modal resize loop until the mouse is released.
type WindowResizer interface {
	StartResize(hitCode uintptr)
}

// StartResize starts a native resize drag for the given WM_NCHITTEST hit
// code (HTLEFT/HTRIGHT/HTTOP/HTBOTTOM/HTTOPLEFT/...). It must run on the
// UI thread: ReleaseCapture + WM_NCLBUTTONDOWN make DefWindowProc enter the
// modal resize loop that tracks the mouse and sizes the window natively.
func (w *webview) StartResize(hitCode uintptr) {
	w.Dispatch(func() {
		_, _, _ = w32.User32ReleaseCapture.Call()
		_, _, _ = w32.User32SendMessageW.Call(w.hwnd, w32.WMNCLButtonDown, hitCode, 0)
	})
}

func (w *webview) Create(debug bool, window unsafe.Pointer) bool {
	// This function signature stopped making sense a long time ago.
	// It is but legacy cruft at this point.
	return w.CreateWithOptions(WindowOptions{})
}

func (w *webview) CreateWithOptions(opts WindowOptions) bool {
	var hinstance windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &hinstance)

	var icon uintptr
	if opts.IconId == 0 {
		// load default icon
		icow, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCxIcon)
		icoh, _, _ := w32.User32GetSystemMetrics.Call(w32.SystemMetricsCyIcon)
		icon, _, _ = w32.User32LoadImageW.Call(uintptr(hinstance), 32512, icow, icoh, 0)
	} else {
		// load icon from resource
		icon, _, _ = w32.User32LoadImageW.Call(uintptr(hinstance), uintptr(opts.IconId), 1, 0, 0, w32.LR_DEFAULTSIZE|w32.LR_SHARED)
	}

	className, _ := windows.UTF16PtrFromString("webview")
	wc := w32.WndClassExW{
		CbSize:        uint32(unsafe.Sizeof(w32.WndClassExW{})),
		HInstance:     hinstance,
		LpszClassName: className,
		HIcon:         windows.Handle(icon),
		HIconSm:       windows.Handle(icon),
		LpfnWndProc:   windows.NewCallback(wndproc),
	}
	_, _, _ = w32.User32RegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	windowName, _ := windows.UTF16PtrFromString(opts.Title)

	windowWidth := opts.Width
	if windowWidth == 0 {
		windowWidth = 640
	}
	windowHeight := opts.Height
	if windowHeight == 0 {
		windowHeight = 480
	}

	var posX, posY uint
	if opts.Center {
		// get screen size
		screenWidth, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CXSCREEN)
		screenHeight, _, _ := w32.User32GetSystemMetrics.Call(w32.SM_CYSCREEN)
		// calculate window position
		posX = (uint(screenWidth) - windowWidth) / 2
		posY = (uint(screenHeight) - windowHeight) / 2
	} else {
		// use default position
		posX = w32.CW_USEDEFAULT
		posY = w32.CW_USEDEFAULT
	}

	// 普通窗口与 frameless 窗口都用 WS_OVERLAPPEDWINDOW（wails 的做法）。
	// frameless 的原生标题栏/边框在运行时通过 WM_NCCALCSIZE 返回 0 隐藏；
	// 保留 WS_THICKFRAME 使系统 resize 循环、最大化/贴靠等行为保持原生。
	style := uintptr(0xCF0000) // WS_OVERLAPPEDWINDOW
	// lpParam 传 *webview：wndproc 在 WM_NCCREATE（首个消息）里把它登记进
	// windowContext，从而首个 WM_NCCALCSIZE 就能按 frameless 处理 —— 启动
	// 即隐藏原生标题栏，无需事后 SWP_FRAMECHANGED 补救。
	w.hwnd, _, _ = w32.User32CreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		style,
		uintptr(posX),
		uintptr(posY),
		uintptr(windowWidth),
		uintptr(windowHeight),
		w.owner, // hwndParent：owner 窗口（PRODSIM-1 主窗口/小窗口层级）
		0,
		uintptr(hinstance),
		uintptr(unsafe.Pointer(w)),
	)
	setWindowContext(w.hwnd, w)

	// If Hidden is requested, keep the window invisible until Show() is called
	// (e.g. after the first navigation completes) to avoid a white flash.
	showCmd := w32.SWShow
	if w.hidden {
		showCmd = w32.SWHide
	}
	_, _, _ = w32.User32ShowWindow.Call(w.hwnd, uintptr(showCmd))
	_, _, _ = w32.User32UpdateWindow.Call(w.hwnd)
	_, _, _ = w32.User32SetFocus.Call(w.hwnd)

	if !w.browser.Embed(w.hwnd) {
		return false
	}
	w.browser.Resize()
	return true
}

func (w *webview) Destroy() {
	_, _, _ = w32.User32PostMessageW.Call(w.hwnd, w32.WMClose, 0, 0)
}

// DestroyForced 销毁窗口，**无视 HideOnClose**（程序收尾用：日志窗口的 HideOnClose 只拦截
// 用户点关闭，不阻止进程退出）。DestroyWindow 必须在本窗口线程执行 → 经 Dispatch 投递。
func (w *webview) DestroyForced() {
	w.Dispatch(func() {
		_, _, _ = w32.User32DestroyWindow.Call(w.hwnd)
	})
}

func (w *webview) Run() {
	var msg w32.Msg
	for {
		r, _, err := w32.User32GetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)
		// GetMessageW 返回 int：>0 = 取到消息；0 = WM_QUIT（退出循环）；-1 = 出错。
		// 原实现忽略返回值 → -1 时 msg 未填充会忙循环；此处显式处理。
		if int32(r) == -1 {
			log.Printf("GetMessageW error: %v", err)
			return
		}
		if r == 0 {
			return
		}
		if msg.Message == w32.WMApp {
			w.m.Lock()
			q := append([]func(){}, w.dispatchq...)
			w.dispatchq = []func(){}
			w.m.Unlock()
			for _, v := range q {
				v()
			}
		} else if msg.Message == w32.WMQuit {
			return
		}
		r, _, _ = w32.User32GetAncestor.Call(uintptr(msg.Hwnd), w32.GARoot)
		r, _, _ = w32.User32IsDialogMessage.Call(r, uintptr(unsafe.Pointer(&msg)))
		if r != 0 {
			continue
		}
		_, _, _ = w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (w *webview) Terminate() {
	_, _, _ = w32.User32PostQuitMessage.Call(0)
}

func (w *webview) Window() unsafe.Pointer {
	return unsafe.Pointer(w.hwnd)
}

func (w *webview) Navigate(url string) {
	w.browser.Navigate(url)
}

func (w *webview) SetHtml(html string) {
	w.browser.NavigateToString(html)
}

func (w *webview) SetTitle(title string) {
	_title, err := windows.UTF16FromString(title)
	if err != nil {
		_title, _ = windows.UTF16FromString("")
	}
	_, _, _ = w32.User32SetWindowTextW.Call(w.hwnd, uintptr(unsafe.Pointer(&_title[0])))
}

func (w *webview) SetSize(width int, height int, hints Hint) {
	index := w32.GWLStyle
	style := w32.GetWindowLong(w.hwnd, index)
	if hints == HintFixed {
		style &^= (w32.WSThickFrame | w32.WSMaximizeBox)
	} else {
		style |= (w32.WSThickFrame | w32.WSMaximizeBox)
	}
	w32.SetWindowLong(w.hwnd, index, style)

	if hints == HintMax {
		w.maxsz.X = int32(width)
		w.maxsz.Y = int32(height)
	} else if hints == HintMin {
		w.minsz.X = int32(width)
		w.minsz.Y = int32(height)
	} else {
		r := w32.Rect{}
		r.Left = 0
		r.Top = 0
		r.Right = int32(width)
		r.Bottom = int32(height)
		if !w.frameless {
			_, _, _ = w32.User32AdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), w32.WSOverlappedWindow, 0)
		}
		// frameless 的 WM_NCCALCSIZE 返回 0（客户区=整个窗口），
		// 因此窗口尺寸直接等于内容尺寸，无需 AdjustWindowRect。
		_, _, _ = w32.User32SetWindowPos.Call(
			w.hwnd, 0, uintptr(r.Left), uintptr(r.Top), uintptr(r.Right-r.Left), uintptr(r.Bottom-r.Top),
			w32.SWPNoZOrder|w32.SWPNoActivate|w32.SWPNoMove|w32.SWPFrameChanged)
		w.browser.Resize()
	}
}

func (w *webview) Init(js string) {
	w.browser.Init(js)
}

func (w *webview) Eval(js string) {
	// browser.Eval 失败（如脚本含 NUL 致 UTF-16 转换失败）仅记日志，不再退出进程。
	if err := w.browser.Eval(js); err != nil {
		log.Printf("failed to eval script: %v", err)
	}
}

// WebResourceInterceptor exposes the WebView2 WebResourceRequested capability
// that the base WebView interface does not provide. Implemented by *webview.
type WebResourceInterceptor interface {
	// SetWebResourceRequestedCallback registers the handler invoked for every
	// request matching one of the added filters.
	SetWebResourceRequestedCallback(cb func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs))
	// AddWebResourceRequestedFilter registers a URI filter (e.g. "*" or
	// "https://app.localhost/*") to be intercepted.
	AddWebResourceRequestedFilter(filter string, ctx edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT)
	// Chromium returns the underlying Chromium instance, exposing the
	// WebView2 environment for building WebResourceResponses.
	Chromium() *edge.Chromium
}

// SetWebResourceRequestedCallback delegates to the underlying Chromium.
func (w *webview) SetWebResourceRequestedCallback(cb func(request *edge.ICoreWebView2WebResourceRequest, args *edge.ICoreWebView2WebResourceRequestedEventArgs)) {
	if c, ok := w.browser.(*edge.Chromium); ok {
		c.WebResourceRequestedCallback = cb
	}
}

// AddWebResourceRequestedFilter delegates to the underlying Chromium.
func (w *webview) AddWebResourceRequestedFilter(filter string, ctx edge.COREWEBVIEW2_WEB_RESOURCE_CONTEXT) {
	if c, ok := w.browser.(*edge.Chromium); ok {
		c.AddWebResourceRequestedFilter(filter, ctx)
	}
}

// Chromium returns the underlying Chromium instance.
func (w *webview) Chromium() *edge.Chromium {
	if c, ok := w.browser.(*edge.Chromium); ok {
		return c
	}
	return nil
}

// SetNavigationCompletedCallback registers a callback fired when a navigation
// completes (used to reveal a Hidden window once content is ready).
func (w *webview) SetNavigationCompletedCallback(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs)) {
	if c, ok := w.browser.(*edge.Chromium); ok {
		c.NavigationCompletedCallback = func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs) {
			if cb != nil {
				cb(sender, args)
			}
		}
	}
}

// Show makes the native window visible (after it was created with Hidden).
func (w *webview) Show() {
	_, _, _ = w32.User32ShowWindow.Call(w.hwnd, w32.SWShow)
	_, _, _ = w32.User32UpdateWindow.Call(w.hwnd)
	// 同步 WebView2 控件可见性，并重设 bounds 确保渲染区域正确
	if c, ok := w.browser.(*edge.Chromium); ok {
		_ = c.Show()
		c.Resize()
	}
}

// Hide hides the native window without destroying it (log window: close = back to tray).
func (w *webview) Hide() {
	_, _, _ = w32.User32ShowWindow.Call(w.hwnd, w32.SWHide)
}

// WebViewLifecycle exposes window visibility control to the caller.
type WebViewLifecycle interface {
	// SetNavigationCompletedCallback fires when a navigation completes.
	SetNavigationCompletedCallback(cb func(sender *edge.ICoreWebView2, args *edge.ICoreWebView2NavigationCompletedEventArgs))
	// Show makes the native window visible.
	Show()
	// Hide hides the native window without destroying it.
	Hide()
}

// OpenDevToolsWindow opens the DevTools window for the current page.
func (w *webview) OpenDevToolsWindow() {
	if c, ok := w.browser.(*edge.Chromium); ok {
		_ = c.OpenDevToolsWindow()
	}
}

// DevTools exposes the ability to programmatically open the DevTools window.
type DevTools interface {
	OpenDevToolsWindow()
}

// SetAcceleratorKeyCallback installs a callback invoked for accelerator key
// presses (e.g. F12). Return true to handle (swallow) the key, false to let
// WebView2 process it normally.
func (w *webview) SetAcceleratorKeyCallback(cb func(uint) bool) {
	if c, ok := w.browser.(*edge.Chromium); ok {
		c.AcceleratorKeyCallback = cb
	}
}

// Accelerator exposes accelerator key interception.
type Accelerator interface {
	SetAcceleratorKeyCallback(cb func(uint) bool)
}

// ScriptEval exposes synchronous script evaluation with a returned result.
type ScriptEval interface {
	// EvalWithResult runs JavaScript in the top-level frame and returns the
	// JSON-encoded result, blocking until the completion callback fires or
	// timeout elapses. Must not be called from the WebView2 UI thread (the
	// callback needs that thread to run, which would deadlock).
	EvalWithResult(script string, timeout time.Duration) (string, error)
}

// EvalWithResult runs JavaScript and returns the JSON-encoded result string.
// The ExecuteScript call itself is dispatched onto the WebView2 UI thread
// (STA marshaling requires it), while this method blocks the caller until the
// completion callback fires or timeout elapses.
func (w *webview) EvalWithResult(script string, timeout time.Duration) (string, error) {
	c, ok := w.browser.(*edge.Chromium)
	if !ok {
		return "", errors.New("browser does not support script result")
	}
	ch := make(chan struct {
		res    uintptr
		result string
	}, 1)
	w.Dispatch(func() {
		c.ExecuteScriptWithResult(script, func(res uintptr, result string) {
			ch <- struct {
				res    uintptr
				result string
			}{res, result}
		})
	})
	select {
	case r := <-ch:
		if r.res != 0 {
			return "", fmt.Errorf("ExecuteScript failed: HRESULT=%08x", r.res)
		}
		return r.result, nil
	case <-time.After(timeout):
		return "", errors.New("ExecuteScript timed out")
	}
}

// Screenshot captures the current page and returns raw PNG bytes.
type Screenshot interface {
	// Screenshot blocks until the capture completes or timeout elapses. Must
	// not be called from the WebView2 UI thread.
	Screenshot(timeout time.Duration) ([]byte, error)
}

// Screenshot captures the page into raw PNG bytes.
func (w *webview) Screenshot(timeout time.Duration) ([]byte, error) {
	c, ok := w.browser.(*edge.Chromium)
	if !ok {
		return nil, errors.New("browser does not support capture preview")
	}
	ch := make(chan struct {
		data []byte
		err  error
	}, 1)
	w.Dispatch(func() {
		c.CapturePreview(func(res uintptr, data []byte) {
			if res != 0 {
				ch <- struct {
					data []byte
					err  error
				}{nil, fmt.Errorf("CapturePreview failed: HRESULT=%08x", res)}
				return
			}
			ch <- struct {
				data []byte
				err  error
			}{data, nil}
		})
	})
	select {
	case r := <-ch:
		return r.data, r.err
	case <-time.After(timeout):
		return nil, errors.New("CapturePreview timed out")
	}
}

func (w *webview) Dispatch(f func()) {
	w.m.Lock()
	w.dispatchq = append(w.dispatchq, f)
	w.m.Unlock()
	_, _, _ = w32.User32PostThreadMessageW.Call(w.mainthread, w32.WMApp, 0, 0)
}
