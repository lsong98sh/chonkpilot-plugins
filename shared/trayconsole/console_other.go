//go:build !windows

package trayconsole

// platformState 在非 Windows 平台无窗口句柄（Start 恒返回 ErrUnsupported）。
type platformState struct {
	hwnd uintptr
}

// Start 在非 Windows 平台不支持托盘窗口（调用方回落为仅 stderr）。
func Start(opts Options) (*Console, error) {
	_ = normalizeOptions(opts)
	return nil, ErrUnsupported
}
