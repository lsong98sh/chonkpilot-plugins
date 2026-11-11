//go:build windows

package filetool

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// holdDeleteSemantics 表示该平台使用「持有独占句柄 + delete-on-close」删除。
const holdDeleteSemantics = true

// heldFile 是持有中的独占文件句柄；close 前始终保持 dwShareMode=0。
type heldFile struct {
	path string
	h    windows.Handle
}

// holdExclusive 以 DELETE（需要读取时再加 GENERIC_READ）权限、dwShareMode=0、
// OPEN_EXISTING 打开文件并把句柄一直持有到 markDeleteOnClose + close。
//
// 共享冲突/锁冲突 → errFileInUse（可重试）；ACCESS_DENIED → errAccessDenied。
func holdExclusive(path string, forRead bool) (*heldFile, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	access := uint32(windows.DELETE)
	if forRead {
		access |= windows.GENERIC_READ
	}
	h, err := windows.CreateFile(p, access, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil && forRead && errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		// md5 只是审计信息：ACL 允许删除但拒绝读取时，退化为 DELETE-only 打开，
		// 不因为“读不到”而阻止删除；md5 随后会记 md5_skipped。
		h, err = windows.CreateFile(p, windows.DELETE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	}
	if err != nil {
		switch {
		case errors.Is(err, windows.ERROR_SHARING_VIOLATION), errors.Is(err, windows.ERROR_LOCK_VIOLATION):
			return nil, errFileInUse
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return nil, errAccessDenied
		}
		return nil, err
	}
	return &heldFile{path: path, h: h}, nil
}

// fileDispositionInfo 对应 FILE_DISPOSITION_INFO（x/sys/windows 未导出该结构）。
type fileDispositionInfo struct {
	DeleteFile byte
}

// markDeleteOnClose 标记 delete-on-close；之后 CloseHandle 即删除文件。
func (f *heldFile) markDeleteOnClose() error {
	info := fileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(f.h, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// clearDeleteOnClose 清除 delete-on-close 标记（DeleteFile:0）。
func (f *heldFile) clearDeleteOnClose() error {
	info := fileDispositionInfo{DeleteFile: 0}
	return windows.SetFileInformationByHandle(f.h, windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
}

// md5 从持有的句柄流式计算 md5（句柄需以 forRead=true 打开）。
func (f *heldFile) md5() (string, error) {
	h := md5.New()
	buf := make([]byte, 64*1024)
	for {
		var n uint32
		err := windows.ReadFile(f.h, buf, &n, nil)
		if n > 0 {
			_, _ = h.Write(buf[:n])
		}
		if err == nil {
			// 同步句柄在 EOF 处可能返回 (n=0, err=nil)，据此结束循环。
			if n == 0 {
				break
			}
			continue
		}
		if errors.Is(err, windows.ERROR_HANDLE_EOF) {
			break
		}
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// close 关闭句柄；若已标记 delete-on-close，关闭即删除文件。
func (f *heldFile) close() error {
	if f.h == 0 || f.h == windows.InvalidHandle {
		return nil
	}
	err := windows.CloseHandle(f.h)
	f.h = windows.InvalidHandle
	return err
}
