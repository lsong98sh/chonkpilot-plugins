//go:build windows
// +build windows

package edge

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/jchv/go-webview2/internal/w32"
	"golang.org/x/sys/windows"
)

type Chromium struct {
	hwnd                  uintptr
	focusOnInit           bool
	controller            *ICoreWebView2Controller
	webview               *ICoreWebView2
	inited                uintptr
	envCompleted          *iCoreWebView2CreateCoreWebView2EnvironmentCompletedHandler
	controllerCompleted   *iCoreWebView2CreateCoreWebView2ControllerCompletedHandler
	permissionRequested   *iCoreWebView2PermissionRequestedEventHandler
	webResourceRequested  *iCoreWebView2WebResourceRequestedEventHandler
	acceleratorKeyPressed *ICoreWebView2AcceleratorKeyPressedEventHandler
	navigationCompleted   *ICoreWebView2NavigationCompletedEventHandler

	environment *ICoreWebView2Environment

	// Settings
	DataPath string

	// initMu / initErr / initFailed 承载「环境 + 控制器创建」阶段的失败：
	// EnvironmentCompleted / CreateCoreWebView2ControllerCompleted 是 COM 回调、只能返回
	// uintptr，无法直接回传 Go error，故把失败记录到结构体字段（首个错误为准）并置
	// initFailed 位，供发起创建的 Embed 消息泵感知后向上返回 false（单窗口失败不再
	// os.Exit 拖垮整进程）。
	initMu     sync.Mutex
	initErr    error
	initFailed uintptr

	// scriptHandlers roots ExecuteScriptWithResult completion handlers until
	// their async callback fires (native WebView2 holds only a raw pointer).
	scriptHandlers sync.Map

	// captureHandlers roots CapturePreview completion handlers likewise.
	captureHandlers sync.Map

	// permissions
	permissions      map[CoreWebView2PermissionKind]CoreWebView2PermissionState
	globalPermission *CoreWebView2PermissionState

	// Callbacks
	WebResourceRequestedCallback func(request *ICoreWebView2WebResourceRequest, args *ICoreWebView2WebResourceRequestedEventArgs)
	NavigationCompletedCallback  func(sender *ICoreWebView2, args *ICoreWebView2NavigationCompletedEventArgs)
	AcceleratorKeyCallback       func(uint) bool
}

func NewChromium() *Chromium {
	e := &Chromium{}
	/*
	 All these handlers are passed to native code through syscalls with 'uintptr(unsafe.Pointer(handler))' and we know
	 that a pointer to those will be kept in the native code. Furthermore these handlers als contain pointer to other Go
	 structs like the vtable.
	 This violates the unsafe.Pointer rule '(4) Conversion of a Pointer to a uintptr when calling syscall.Syscall.' because
	 theres no guarantee that Go doesn't move these objects.
	 AFAIK currently the Go runtime doesn't move HEAP objects, so we should be safe with these handlers. But they don't
	 guarantee it, because in the future Go might use a compacting GC.
	 There's a proposal to add a runtime.Pin function, to prevent moving pinned objects, which would allow to easily fix
	 this issue by just pinning the handlers. The https://go-review.googlesource.com/c/go/+/367296/ should land in Go 1.19.
	*/
	e.envCompleted = newICoreWebView2CreateCoreWebView2EnvironmentCompletedHandler(e)
	e.controllerCompleted = newICoreWebView2CreateCoreWebView2ControllerCompletedHandler(e)
	e.permissionRequested = newICoreWebView2PermissionRequestedEventHandler(e)
	e.webResourceRequested = newICoreWebView2WebResourceRequestedEventHandler(e)
	e.acceleratorKeyPressed = newICoreWebView2AcceleratorKeyPressedEventHandler(e)
	e.navigationCompleted = newICoreWebView2NavigationCompletedEventHandler(e)
	e.permissions = make(map[CoreWebView2PermissionKind]CoreWebView2PermissionState)

	return e
}

// setInitError 记录初始化阶段（环境 / 控制器创建）的首个错误，并置「已结束」标记，
// 供 Embed 的消息泵退出（COM 回调无法直接返回 error，见字段注释）。
func (e *Chromium) setInitError(err error) {
	if err == nil {
		return
	}
	e.initMu.Lock()
	if e.initErr == nil {
		e.initErr = err
	}
	e.initMu.Unlock()
	atomic.StoreUintptr(&e.initFailed, 1)
}

// InitError 返回初始化阶段的失败原因；nil = 成功或仍在进行中。
func (e *Chromium) InitError() error {
	e.initMu.Lock()
	defer e.initMu.Unlock()
	return e.initErr
}

func (e *Chromium) Embed(hwnd uintptr) bool {
	e.hwnd = hwnd

	dataPath := e.DataPath
	if dataPath == "" {
		currentExePath := make([]uint16, windows.MAX_PATH)
		_, err := windows.GetModuleFileName(windows.Handle(0), &currentExePath[0], windows.MAX_PATH)
		if err != nil {
			// What to do here?
			return false
		}
		currentExeName := filepath.Base(windows.UTF16ToString(currentExePath))
		dataPath = filepath.Join(os.Getenv("AppData"), currentExeName)
	}

	res, err := createCoreWebView2EnvironmentWithOptions(nil, windows.StringToUTF16Ptr(dataPath), 0, e.envCompleted)
	if err != nil {
		log.Printf("Error calling Webview2Loader: %v", err)
		return false
	} else if res != 0 {
		log.Printf("Result: %08x", res)
		return false
	}
	var msg w32.Msg
	for {
		if atomic.LoadUintptr(&e.inited) != 0 || atomic.LoadUintptr(&e.initFailed) != 0 {
			break
		}
		r, _, _ := w32.User32GetMessageW.Call(
			uintptr(unsafe.Pointer(&msg)),
			0,
			0,
			0,
		)
		if r == 0 {
			break
		}
		_, _, _ = w32.User32TranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = w32.User32DispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
	// 回调内记录失败时，消息泵据 initFailed 退出并向调用方回传「该窗口失败」（不再 log.Fatal 退出进程）。
	if err := e.InitError(); err != nil {
		log.Printf("WebView2 initialization failed: %v", err)
		return false
	}
	return true
}

func (e *Chromium) Navigate(url string) {
	_, _, _ = e.webview.vtbl.Navigate.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(url))),
	)
}

func (e *Chromium) NavigateToString(htmlContent string) {
	_, _, _ = e.webview.vtbl.NavigateToString.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(htmlContent))),
	)
}

func (e *Chromium) Init(script string) {
	_, _, _ = e.webview.vtbl.AddScriptToExecuteOnDocumentCreated.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(script))),
		0,
	)
}

// Eval 在页面执行 JS；仅脚本含 NUL 等致 UTF-16 转换失败时返回 error（原 log.Fatal 会
// 直接退出整进程，单次求值失败不应如此）。
func (e *Chromium) Eval(script string) error {
	_script, err := windows.UTF16PtrFromString(script)
	if err != nil {
		log.Printf("WebView2 Eval: %v", err)
		return err
	}

	_, _, _ = e.webview.vtbl.ExecuteScript.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(_script)),
		0,
	)
	return nil
}

// ExecuteScriptWithResult runs JavaScript and delivers the JSON-encoded result
// string via cb when the script completes (on the WebView2 UI thread).
func (e *Chromium) ExecuteScriptWithResult(script string, cb func(res uintptr, result string)) {
	var handler *ICoreWebView2ExecuteScriptCompletedHandler
	handler = NewExecuteScriptCompletedHandler(func(res uintptr, result string) {
		cb(res, result)
		// The native side holds a raw pointer to handler without rooting it;
		// keep it reachable from Go until the async callback has fired.
		e.scriptHandlers.Delete(handler)
	})
	e.scriptHandlers.Store(handler, struct{}{})
	if err := e.webview.ExecuteScriptWithResult(script, handler); err != nil {
		e.scriptHandlers.Delete(handler)
		log.Printf("ExecuteScriptWithResult failed: %v", err)
	}
}

// CapturePreview captures the current page into an in-memory PNG stream and
// delivers the raw PNG bytes via cb when done. Must be called on the WebView2
// UI thread.
func (e *Chromium) CapturePreview(cb func(res uintptr, data []byte)) {
	streamPtr, err := w32.SHCreateMemStream(nil)
	if err != nil || streamPtr == 0 {
		cb(uintptr(0x80004005), nil) // E_FAIL
		return
	}
	stream := (*IStream)(unsafe.Pointer(streamPtr))

	var handler *ICoreWebView2CapturePreviewCompletedHandler
	handler = newICoreWebView2CapturePreviewCompletedHandler(&CapturePreviewCompletedHandler{cb: func(res uintptr) {
		defer e.captureHandlers.Delete(handler)
		defer stream.Release()
		if res != 0 {
			cb(res, nil)
			return
		}
		var size uint64
		if err := stream.Seek(0, StreamSeekEnd, &size); err != nil {
			cb(uintptr(0x80004005), nil)
			return
		}
		_ = stream.Seek(0, StreamSeekSet, nil)
		data, err := stream.ReadAll()
		if err != nil {
			cb(uintptr(0x80004005), nil)
			return
		}
		cb(res, data)
	}})
	e.captureHandlers.Store(handler, struct{}{})
	if err := e.webview.CapturePreview(CapturePreviewImageFormatPng, stream, handler); err != nil {
		e.captureHandlers.Delete(handler)
		cb(uintptr(0x80004005), nil)
	}
}

func (e *Chromium) Show() error {
	return e.controller.PutIsVisible(true)
}

func (e *Chromium) Hide() error {
	return e.controller.PutIsVisible(false)
}

func (e *Chromium) QueryInterface(_, _ uintptr) uintptr {
	return 0
}

func (e *Chromium) AddRef() uintptr {
	return 1
}

func (e *Chromium) Release() uintptr {
	return 1
}

func (e *Chromium) EnvironmentCompleted(res uintptr, env *ICoreWebView2Environment) uintptr {
	if int64(res) < 0 {
		// 不再 log.Fatal：记录失败供 Embed 感知（该窗口失败，不拖垮整进程）。
		e.setInitError(fmt.Errorf("creating environment failed with %08x", res))
		return 0
	}
	_, _, _ = env.vtbl.AddRef.Call(uintptr(unsafe.Pointer(env)))
	e.environment = env

	_, _, _ = env.vtbl.CreateCoreWebView2Controller.Call(
		uintptr(unsafe.Pointer(env)),
		e.hwnd,
		uintptr(unsafe.Pointer(e.controllerCompleted)),
	)
	return 0
}

func (e *Chromium) CreateCoreWebView2ControllerCompleted(res uintptr, controller *ICoreWebView2Controller) uintptr {
	if int64(res) < 0 {
		// 不再 log.Fatal：记录失败供 Embed 感知（该窗口失败，不拖垮整进程）。
		e.setInitError(fmt.Errorf("creating controller failed with %08x", res))
		return 0
	}
	_, _, _ = controller.vtbl.AddRef.Call(uintptr(unsafe.Pointer(controller)))
	e.controller = controller

	var token _EventRegistrationToken
	_, _, _ = controller.vtbl.GetCoreWebView2.Call(
		uintptr(unsafe.Pointer(controller)),
		uintptr(unsafe.Pointer(&e.webview)),
	)
	_, _, _ = e.webview.vtbl.AddRef.Call(
		uintptr(unsafe.Pointer(e.webview)),
	)
	_, _, _ = e.webview.vtbl.AddPermissionRequested.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(e.permissionRequested)),
		uintptr(unsafe.Pointer(&token)),
	)
	_, _, _ = e.webview.vtbl.AddWebResourceRequested.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(e.webResourceRequested)),
		uintptr(unsafe.Pointer(&token)),
	)
	_, _, _ = e.webview.vtbl.AddNavigationCompleted.Call(
		uintptr(unsafe.Pointer(e.webview)),
		uintptr(unsafe.Pointer(e.navigationCompleted)),
		uintptr(unsafe.Pointer(&token)),
	)

	_ = e.controller.AddAcceleratorKeyPressed(e.acceleratorKeyPressed, &token)

	atomic.StoreUintptr(&e.inited, 1)

	// 确保 WebView2 控件可见（默认值依赖运行时版本，显式设置更稳妥）
	_ = e.Show()

	if e.focusOnInit {
		e.Focus()
	}

	return 0
}

func (e *Chromium) SetPermission(kind CoreWebView2PermissionKind, state CoreWebView2PermissionState) {
	e.permissions[kind] = state
}

func (e *Chromium) SetGlobalPermission(state CoreWebView2PermissionState) {
	e.globalPermission = &state
}

func (e *Chromium) PermissionRequested(_ *ICoreWebView2, args *iCoreWebView2PermissionRequestedEventArgs) uintptr {
	var kind CoreWebView2PermissionKind
	_, _, _ = args.vtbl.GetPermissionKind.Call(
		uintptr(unsafe.Pointer(args)),
		uintptr(unsafe.Pointer(&kind)),
	)
	var result CoreWebView2PermissionState
	if e.globalPermission != nil {
		result = *e.globalPermission
	} else {
		var ok bool
		result, ok = e.permissions[kind]
		if !ok {
			result = CoreWebView2PermissionStateDefault
		}
	}
	_, _, _ = args.vtbl.PutState.Call(
		uintptr(unsafe.Pointer(args)),
		uintptr(result),
	)
	return 0
}

func (e *Chromium) WebResourceRequested(sender *ICoreWebView2, args *ICoreWebView2WebResourceRequestedEventArgs) uintptr {
	req, err := args.GetRequest()
	if err != nil {
		// 单次请求失败 → 忽略该请求（交由 WebView2 走默认处理），不 log.Fatal 退出进程。
		log.Printf("WebView2 WebResourceRequested: GetRequest failed: %v", err)
		return 0
	}
	if e.WebResourceRequestedCallback != nil {
		e.WebResourceRequestedCallback(req, args)
	}
	return 0
}

func (e *Chromium) AddWebResourceRequestedFilter(filter string, ctx COREWEBVIEW2_WEB_RESOURCE_CONTEXT) {
	err := e.webview.AddWebResourceRequestedFilter(filter, ctx)
	if err != nil {
		// 过滤器注册失败仅记日志（原 log.Fatal 退出进程）；该过滤器不生效但其余不受影响。
		log.Printf("WebView2 AddWebResourceRequestedFilter(%q) failed: %v", filter, err)
	}
}

func (e *Chromium) Environment() *ICoreWebView2Environment {
	return e.environment
}

// CreateWebResourceResponse builds a response for WebResourceRequested handling.
func (e *Chromium) CreateWebResourceResponse(content []byte, statusCode int, reasonPhrase string, headers string) (*ICoreWebView2WebResourceResponse, error) {
	return e.environment.CreateWebResourceResponse(content, statusCode, reasonPhrase, headers)
}

// AcceleratorKeyPressed is called when an accelerator key is pressed.
// If the AcceleratorKeyCallback method has been set, it will defer handling of the keypress
// to the callback. That callback returns a bool indicating if the event was handled.
func (e *Chromium) AcceleratorKeyPressed(sender *ICoreWebView2Controller, args *ICoreWebView2AcceleratorKeyPressedEventArgs) uintptr {
	if e.AcceleratorKeyCallback == nil {
		return 0
	}
	eventKind, _ := args.GetKeyEventKind()
	if eventKind == COREWEBVIEW2_KEY_EVENT_KIND_KEY_DOWN ||
		eventKind == COREWEBVIEW2_KEY_EVENT_KIND_SYSTEM_KEY_DOWN {
		virtualKey, _ := args.GetVirtualKey()
		status, _ := args.GetPhysicalKeyStatus()
		if !status.WasKeyDown {
			_ = args.PutHandled(e.AcceleratorKeyCallback(virtualKey))
			return 0
		}
	}
	_ = args.PutHandled(false)
	return 0
}

func (e *Chromium) GetSettings() (*ICoreWebViewSettings, error) {
	return e.webview.GetSettings()
}

// OpenDevToolsWindow opens the DevTools window for the current page.
func (e *Chromium) OpenDevToolsWindow() error {
	return e.webview.OpenDevToolsWindow()
}

func (e *Chromium) GetController() *ICoreWebView2Controller {
	return e.controller
}

func boolToInt(input bool) int {
	if input {
		return 1
	}
	return 0
}

func (e *Chromium) NavigationCompleted(sender *ICoreWebView2, args *ICoreWebView2NavigationCompletedEventArgs) uintptr {
	if e.NavigationCompletedCallback != nil {
		e.NavigationCompletedCallback(sender, args)
	}
	return 0
}

func (e *Chromium) NotifyParentWindowPositionChanged() error {
	//It looks like the wndproc function is called before the controller initialization is complete.
	//Because of this the controller is nil
	if e.controller == nil {
		return nil
	}
	return e.controller.NotifyParentWindowPositionChanged()
}

func (e *Chromium) Focus() {
	if e.controller == nil {
		e.focusOnInit = true
		return
	}
	_ = e.controller.MoveFocus(COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
}
