package edge

import (
	"io"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type IStreamVtbl struct {
	_IUnknownVtbl
	Read  ComProc
	Write ComProc
	Seek  ComProc
}

type IStream struct {
	vtbl *IStreamVtbl
}

func (i *IStream) AddRef() uintptr {
	r, _, _ := i.vtbl.AddRef.Call(uintptr(unsafe.Pointer(i)))
	return r
}

func (i *IStream) Release() uint32 {
	r, _, _ := i.vtbl.Release.Call(uintptr(unsafe.Pointer(i)))
	return uint32(r)
}

func (i *IStream) Read(p []byte) (int, error) {
	bufLen := len(p)
	if bufLen == 0 {
		return 0, nil
	}

	var n int
	// r1 即 HRESULT（ComProc.Call 已把 r1 映射为 lastErr，二者一致）。
	// 用 res 判断以区分 S_OK / S_FALSE（EOF）。
	res, _, _ := i.vtbl.Read.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(unsafe.Pointer(&p[0])),
		uintptr(bufLen),
		uintptr(unsafe.Pointer(&n)),
	)

	switch windows.Handle(res) {
	case windows.S_OK:
		// The buffer has been completely filled
		return n, nil
	case windows.S_FALSE:
		// The buffer has been filled with less than len data and the stream is EOF
		return n, io.EOF
	default:
		return 0, syscall.Errno(res)
	}
}

// ReadAll reads the whole stream into a byte slice.
func (i *IStream) ReadAll() ([]byte, error) {
	var out []byte
	buf := make([]byte, 4096)
	for {
		n, err := i.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

// StreamSeekOrigin is the dwOrigin argument of IStream::Seek.
const (
	StreamSeekSet uint32 = iota // STREAM_SEEK_SET
	StreamSeekCur               // STREAM_SEEK_CUR
	StreamSeekEnd               // STREAM_SEEK_END
)

// Seek moves the stream pointer. When newPos is non-nil it receives the new
// absolute position (useful with StreamSeekEnd to learn the stream size).
func (i *IStream) Seek(offset int64, origin uint32, newPos *uint64) error {
	res, _, _ := i.vtbl.Seek.Call(
		uintptr(unsafe.Pointer(i)),
		uintptr(offset), // LARGE_INTEGER (64-bit value, passed by value)
		uintptr(origin),
		uintptr(unsafe.Pointer(newPos)),
	)
	if windows.Handle(res) != windows.S_OK {
		return syscall.Errno(res)
	}
	return nil
}
