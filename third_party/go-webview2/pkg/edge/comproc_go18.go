//go:build go1.18
// +build go1.18

package edge

import "syscall"

// Call calls a COM procedure.
//
//go:uintptrescapes
func (p ComProc) Call(a ...uintptr) (r1, r2 uintptr, lastErr error) {
	// The magic uintptrescapes comment is needed to prevent moving uintptr(unsafe.Pointer(p)) so calls to .Call() also
	// satisfy the unsafe.Pointer rule "(4) Conversion of a Pointer to a uintptr when calling syscall.Syscall."
	// Otherwise it might be that pointers get moved, especially pointer onto the Go stack which might grow dynamically.
	// See https://pkg.go.dev/unsafe#Pointer and https://github.com/golang/go/issues/34474
	//
	// 关键修复：Windows x64 上 syscall.SyscallN 的第三返回值是 GetLastError（可能残留非零值），
	// 不是 COM 的 HRESULT。COM 方法成败看 r1（RAX）。这里把 r1 映射为 lastErr，
	// 使所有调用方的 `if err != windows.ERROR_SUCCESS` 判断语义正确。
	r1, r2, _ = syscall.SyscallN(uintptr(p), a...)
	lastErr = syscall.Errno(r1)
	return r1, r2, lastErr
}
