package edge

import (
	"github.com/jchv/go-webview2/internal/w32"
)

type _ICoreWebView2ExecuteScriptCompletedHandlerVtbl struct {
	_IUnknownVtbl
	Invoke ComProc
}

type ICoreWebView2ExecuteScriptCompletedHandler struct {
	vtbl *_ICoreWebView2ExecuteScriptCompletedHandlerVtbl
	impl _ICoreWebView2ExecuteScriptCompletedHandlerImpl
}

func _ICoreWebView2ExecuteScriptCompletedHandlerIUnknownQueryInterface(this *ICoreWebView2ExecuteScriptCompletedHandler, refiid, object uintptr) uintptr {
	return this.impl.QueryInterface(refiid, object)
}

func _ICoreWebView2ExecuteScriptCompletedHandlerIUnknownAddRef(this *ICoreWebView2ExecuteScriptCompletedHandler) uintptr {
	return this.impl.AddRef()
}

func _ICoreWebView2ExecuteScriptCompletedHandlerIUnknownRelease(this *ICoreWebView2ExecuteScriptCompletedHandler) uintptr {
	return this.impl.Release()
}

// Invoke receives (HRESULT errorCode, LPCWSTR resultObjectAsJson) and forwards
// to the impl. A nil/empty result string is valid (e.g. the script evaluated
// to undefined).
func _ICoreWebView2ExecuteScriptCompletedHandlerInvoke(this *ICoreWebView2ExecuteScriptCompletedHandler, res uintptr, result *uint16) uintptr {
	return this.impl.ExecuteScriptCompleted(res, result)
}

type _ICoreWebView2ExecuteScriptCompletedHandlerImpl interface {
	_IUnknownImpl
	ExecuteScriptCompleted(res uintptr, result *uint16) uintptr
}

var _ICoreWebView2ExecuteScriptCompletedHandlerFn = _ICoreWebView2ExecuteScriptCompletedHandlerVtbl{
	_IUnknownVtbl{
		NewComProc(_ICoreWebView2ExecuteScriptCompletedHandlerIUnknownQueryInterface),
		NewComProc(_ICoreWebView2ExecuteScriptCompletedHandlerIUnknownAddRef),
		NewComProc(_ICoreWebView2ExecuteScriptCompletedHandlerIUnknownRelease),
	},
	NewComProc(_ICoreWebView2ExecuteScriptCompletedHandlerInvoke),
}

func newICoreWebView2ExecuteScriptCompletedHandler(impl _ICoreWebView2ExecuteScriptCompletedHandlerImpl) *ICoreWebView2ExecuteScriptCompletedHandler {
	return &ICoreWebView2ExecuteScriptCompletedHandler{
		vtbl: &_ICoreWebView2ExecuteScriptCompletedHandlerFn,
		impl: impl,
	}
}

// ExecuteScriptCompletedHandler adapts a Go closure to
// ICoreWebView2ExecuteScriptCompletedHandler. Each call gets its own instance,
// so concurrent ExecuteScriptWithResult invocations never share state.
type ExecuteScriptCompletedHandler struct {
	cb func(res uintptr, result string)
}

func (h *ExecuteScriptCompletedHandler) QueryInterface(refiid, object uintptr) uintptr { return 0 }
func (h *ExecuteScriptCompletedHandler) AddRef() uintptr                               { return 1 }
func (h *ExecuteScriptCompletedHandler) Release() uintptr                              { return 1 }

func (h *ExecuteScriptCompletedHandler) ExecuteScriptCompleted(res uintptr, result *uint16) uintptr {
	if h.cb != nil {
		h.cb(res, w32.Utf16PtrToString(result))
	}
	return 0
}

// NewExecuteScriptCompletedHandler wraps cb into a fresh COM handler for a
// single ExecuteScriptWithResult call.
func NewExecuteScriptCompletedHandler(cb func(res uintptr, result string)) *ICoreWebView2ExecuteScriptCompletedHandler {
	return newICoreWebView2ExecuteScriptCompletedHandler(&ExecuteScriptCompletedHandler{cb: cb})
}
