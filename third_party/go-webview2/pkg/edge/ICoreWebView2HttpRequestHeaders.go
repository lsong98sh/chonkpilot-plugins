package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ICoreWebView2HttpRequestHeaders —— 请求头集合（WebView2 SDK）。
//
// 本文件为**只读用途**的最小封装（GetHeader / HasHeader）：供 WebResourceRequested
// 回调读取请求头（如 `Cookie`）。写操作（RemoveHeader / SetHeader）与迭代器
// （GetHeaders / GetIterator）暂未封装 —— 按需再补。
//
// vtable 顺序（Must match WebView2 SDK ICoreWebView2HttpRequestHeaders）：
//
//	GetHeader · GetHeaders · HasHeader · RemoveHeader · GetIterator
type _ICoreWebView2HttpRequestHeadersVtbl struct {
	_IUnknownVtbl
	GetHeader    ComProc
	GetHeaders   ComProc
	HasHeader    ComProc
	RemoveHeader ComProc
	GetIterator  ComProc
}

// ICoreWebView2HttpRequestHeaders 是请求头集合。
type ICoreWebView2HttpRequestHeaders struct {
	vtbl *_ICoreWebView2HttpRequestHeadersVtbl
}

func (i *ICoreWebView2HttpRequestHeaders) AddRef() uintptr {
	r, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))
	return r
}

func (i *ICoreWebView2HttpRequestHeaders) Release() uint32 {
	r, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))
	return uint32(r)
}

// GetHeader 取单个请求头的值；头不存在 → 空串且无错误（与 WebView2 语义一致：
// 返回 S_OK 但 value 为 nil）。
func (i *ICoreWebView2HttpRequestHeaders) GetHeader(name string) (string, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	var val *uint16
	_, _, callErr := i.vtbl.GetHeader.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&val)),
	)
	if callErr != windows.ERROR_SUCCESS {
		return "", callErr
	}
	if val == nil {
		return "", nil
	}
	value := windows.UTF16PtrToString(val)
	windows.CoTaskMemFree(unsafe.Pointer(val))
	return value, nil
}

// HasHeader 判断请求头是否存在。
func (i *ICoreWebView2HttpRequestHeaders) HasHeader(name string) (bool, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	var has int32
	_, _, callErr := i.vtbl.HasHeader.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&has)),
	)
	if callErr != windows.ERROR_SUCCESS {
		return false, callErr
	}
	return has != 0, nil
}
