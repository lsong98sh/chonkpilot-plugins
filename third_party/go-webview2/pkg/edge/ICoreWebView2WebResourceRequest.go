package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type _ICoreWebView2WebResourceRequestVtbl struct {
	_IUnknownVtbl
	GetUri     ComProc
	PutUri     ComProc
	GetMethod  ComProc
	PutMethod  ComProc
	GetContent ComProc
	PutContent ComProc
	GetHeaders ComProc
}

type ICoreWebView2WebResourceRequest struct {
	vtbl *_ICoreWebView2WebResourceRequestVtbl
}

func (i *ICoreWebView2WebResourceRequest) AddRef() uintptr {
	r, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))
	return r
}

func (i *ICoreWebView2WebResourceRequest) GetUri() (string, error) {
	var err error
	// Create *uint16 to hold result
	var _uri *uint16
	_, _, err = i.vtbl.GetUri.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&_uri)),
	)
	if err != windows.ERROR_SUCCESS {
		return "", err
	} // Get result and cleanup
	uri := windows.UTF16PtrToString(_uri)
	windows.CoTaskMemFree(unsafe.Pointer(_uri))
	return uri, nil
}

func (i *ICoreWebView2WebResourceRequest) GetMethod() (string, error) {
	var _method *uint16
	_, _, err := i.vtbl.GetMethod.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&_method)),
	)
	if err != windows.ERROR_SUCCESS {
		return "", err
	}
	method := windows.UTF16PtrToString(_method)
	windows.CoTaskMemFree(unsafe.Pointer(_method))
	return method, nil
}

// GetContent returns the request body stream (nil if empty / GET request).
func (i *ICoreWebView2WebResourceRequest) GetContent() (*IStream, error) {
	var content *IStream
	_, _, err := i.vtbl.GetContent.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&content)),
	)
	if err != windows.ERROR_SUCCESS {
		return nil, err
	}
	return content, nil
}

// GetContentBytes reads the request body fully, returning nil when absent.
func (i *ICoreWebView2WebResourceRequest) GetContentBytes() ([]byte, error) {
	stream, err := i.GetContent()
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, nil
	}
	defer stream.Release()
	return stream.ReadAll()
}

// GetHeaders returns the request header collection (caller must Release it).
//
// 用途：WebResourceRequested 回调中读取请求头（如 `Cookie`）——虚拟宿主源
// （无 HTTP server）下无法从别处拿到请求头，这是唯一途径。
func (i *ICoreWebView2WebResourceRequest) GetHeaders() (*ICoreWebView2HttpRequestHeaders, error) {
	var headers *ICoreWebView2HttpRequestHeaders
	_, _, err := i.vtbl.GetHeaders.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&headers)),
	)
	if err != windows.ERROR_SUCCESS {
		return nil, err
	}
	return headers, nil
}
