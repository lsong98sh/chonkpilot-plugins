package edge

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ICoreWebviewSettings is the merged settings class

type _ICoreWebViewSettingsVtbl struct {
	_IUnknownVtbl
	GetIsScriptEnabled                  ComProc
	PutIsScriptEnabled                  ComProc
	GetIsWebMessageEnabled              ComProc
	PutIsWebMessageEnabled              ComProc
	GetAreDefaultScriptDialogsEnabled   ComProc
	PutAreDefaultScriptDialogsEnabled   ComProc
	GetIsStatusBarEnabled               ComProc
	PutIsStatusBarEnabled               ComProc
	GetAreDevToolsEnabled               ComProc
	PutAreDevToolsEnabled               ComProc
	GetAreDefaultContextMenusEnabled    ComProc
	PutAreDefaultContextMenusEnabled    ComProc
	GetAreHostObjectsAllowed            ComProc
	PutAreHostObjectsAllowed            ComProc
	GetIsZoomControlEnabled             ComProc
	PutIsZoomControlEnabled             ComProc
	GetIsBuiltInErrorPageEnabled        ComProc
	PutIsBuiltInErrorPageEnabled        ComProc
	GetUserAgent                        ComProc
	PutUserAgent                        ComProc
	GetAreBrowserAcceleratorKeysEnabled ComProc
	PutAreBrowserAcceleratorKeysEnabled ComProc
	GetIsPasswordAutosaveEnabled        ComProc
	PutIsPasswordAutosaveEnabled        ComProc
	GetIsGeneralAutofillEnabled         ComProc
	PutIsGeneralAutofillEnabled         ComProc
	GetIsPinchZoomEnabled               ComProc
	PutIsPinchZoomEnabled               ComProc
	GetIsSwipeNavigationEnabled         ComProc
	PutIsSwipeNavigationEnabled         ComProc
	// ICoreWebView2Settings7
	GetHiddenPdfToolbarItems ComProc
	PutHiddenPdfToolbarItems ComProc
	// ICoreWebView2Settings8
	GetIsReputationCheckingRequired ComProc
	PutIsReputationCheckingRequired ComProc
	// ICoreWebView2Settings9
	GetIsNonClientRegionSupportEnabled ComProc
	PutIsNonClientRegionSupportEnabled ComProc
}

type ICoreWebViewSettings struct {
	vtbl *_ICoreWebViewSettingsVtbl
}

func (i *ICoreWebViewSettings) AddRef() uintptr {
	r, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))
	return r
}

func (i *ICoreWebViewSettings) GetIsScriptEnabled() (bool, error) {
	var err error
	var isScriptEnabled bool
	_, _, err = i.vtbl.GetIsScriptEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&isScriptEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return isScriptEnabled, nil
}

func (i *ICoreWebViewSettings) PutIsScriptEnabled(isScriptEnabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsScriptEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(isScriptEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsWebMessageEnabled() (bool, error) {
	var err error
	var isWebMessageEnabled bool
	_, _, err = i.vtbl.GetIsWebMessageEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&isWebMessageEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return isWebMessageEnabled, nil
}

func (i *ICoreWebViewSettings) PutIsWebMessageEnabled(isWebMessageEnabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsWebMessageEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(isWebMessageEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetAreDefaultScriptDialogsEnabled() (bool, error) {
	var err error
	var areDefaultScriptDialogsEnabled bool
	_, _, err = i.vtbl.GetAreDefaultScriptDialogsEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&areDefaultScriptDialogsEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return areDefaultScriptDialogsEnabled, nil
}

func (i *ICoreWebViewSettings) PutAreDefaultScriptDialogsEnabled(areDefaultScriptDialogsEnabled bool) error {
	var err error

	_, _, err = i.vtbl.PutAreDefaultScriptDialogsEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(areDefaultScriptDialogsEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsStatusBarEnabled() (bool, error) {
	var err error
	var isStatusBarEnabled bool
	_, _, err = i.vtbl.GetIsStatusBarEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&isStatusBarEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return isStatusBarEnabled, nil
}

func (i *ICoreWebViewSettings) PutIsStatusBarEnabled(isStatusBarEnabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsStatusBarEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(isStatusBarEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetAreDevToolsEnabled() (bool, error) {
	var err error
	var areDevToolsEnabled bool
	_, _, err = i.vtbl.GetAreDevToolsEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&areDevToolsEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return areDevToolsEnabled, nil
}

func (i *ICoreWebViewSettings) PutAreDevToolsEnabled(areDevToolsEnabled bool) error {
	var err error
	_, _, err = i.vtbl.PutAreDevToolsEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(areDevToolsEnabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetAreDefaultContextMenusEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetAreDefaultContextMenusEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutAreDefaultContextMenusEnabled(enabled bool) error {
	var err error
	_, _, err = i.vtbl.PutAreDefaultContextMenusEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetAreHostObjectsAllowed() (bool, error) {
	var err error
	var allowed bool
	_, _, err = i.vtbl.GetAreHostObjectsAllowed.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&allowed)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return allowed, nil
}

func (i *ICoreWebViewSettings) PutAreHostObjectsAllowed(allowed bool) error {
	var err error

	_, _, err = i.vtbl.PutAreHostObjectsAllowed.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(allowed)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsZoomControlEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetIsZoomControlEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutIsZoomControlEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsZoomControlEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsBuiltInErrorPageEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetIsBuiltInErrorPageEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutIsBuiltInErrorPageEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsBuiltInErrorPageEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetUserAgent() (string, error) {
	var err error
	// Create *uint16 to hold result
	var _userAgent *uint16
	_, _, err = i.vtbl.GetUserAgent.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_userAgent)),
	)
	if err != windows.ERROR_SUCCESS {
		return "", err
	} // Get result and cleanup
	userAgent := windows.UTF16PtrToString(_userAgent)
	windows.CoTaskMemFree(unsafe.Pointer(_userAgent))
	return userAgent, nil
}

func (i *ICoreWebViewSettings) PutUserAgent(userAgent string) error {
	var err error
	// Convert string 'userAgent' to *uint16
	_userAgent, err := windows.UTF16PtrFromString(userAgent)
	if err != nil {
		return err
	}

	_, _, err = i.vtbl.PutUserAgent.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(_userAgent)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetAreBrowserAcceleratorKeysEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetAreBrowserAcceleratorKeysEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutAreBrowserAcceleratorKeysEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutAreBrowserAcceleratorKeysEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsPinchZoomEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetIsPinchZoomEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutIsPinchZoomEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsPinchZoomEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

func (i *ICoreWebViewSettings) GetIsSwipeNavigationEnabled() (bool, error) {
	var err error
	var enabled bool
	_, _, err = i.vtbl.GetIsSwipeNavigationEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return enabled, nil
}

func (i *ICoreWebViewSettings) PutIsSwipeNavigationEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsSwipeNavigationEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}

// GetSettings9 通过 QueryInterface 取 ICoreWebView2Settings9 接口指针。
//
// **必须**经 QI 取扩展接口：`GetSettings` 返回的是 ICoreWebView2Settings（基接口）
// 指针，其 vtable 不保证含 Settings9 扩展槽位；直接经基接口调用
// PutIsNonClientRegionSupportEnabled 属未定义行为（实测返回成功但**不生效**，
// 表现为 `app-region: drag` 拖不动窗口）。IID 见 WebView2 SDK
// IID_ICoreWebView2Settings9 = {0528a73b-e92d-49f4-927a-e547dddaa37d}。
func (i *ICoreWebViewSettings) GetSettings9() (*ICoreWebViewSettings, error) {
	var result *ICoreWebViewSettings
	iid := NewGUID("{0528a73b-e92d-49f4-927a-e547dddaa37d}")
	_, _, err := i.vtbl.QueryInterface.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&result)),
	)
	if err != windows.ERROR_SUCCESS {
		return nil, err
	}
	return result, nil
}

// GetIsNonClientRegionSupportEnabled 读取 `app-region` 非客户区支持开关（Settings9）。
// 注意：应经 GetSettings9() 取得接口指针后再调用（见上）。COM BOOL = 4 字节 → int32。
func (i *ICoreWebViewSettings) GetIsNonClientRegionSupportEnabled() (bool, error) {
	var v int32
	var err error
	_, _, err = i.vtbl.GetIsNonClientRegionSupportEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&v)),
	)
	if err != windows.ERROR_SUCCESS {
		return false, err
	}
	return v != 0, nil
}

// PutIsNonClientRegionSupportEnabled enables/disables the `app-region` CSS
// feature (ICoreWebView2Settings9). When TRUE, elements styled with
// `-webkit-app-region: drag` are treated like the window title bar (dragging,
// system menu on right-click, double-click maximize/restore). Takes effect
// after the next navigation.
//
// 注意：应经 GetSettings9() 取得接口指针后再调用（见上）。
func (i *ICoreWebViewSettings) PutIsNonClientRegionSupportEnabled(enabled bool) error {
	var err error

	_, _, err = i.vtbl.PutIsNonClientRegionSupportEnabled.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(boolToInt(enabled)),
	)
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return nil
}
