//go:build windows

package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// canonicalPath 归一化 work-dir：Clean + 折叠大小写（Windows 路径不区分大小写，
// 同一目录的不同拼写须映射到同一把锁，否则占用校验可被绕过）。
func canonicalPath(p string) string {
	return strings.ToLower(filepath.Clean(p))
}

// Lock 是持有中的文件锁（随进程存活；Release 或进程退出即释放）。
type Lock struct {
	f *os.File
}

// Acquire 非阻塞获取 path 对应的排他锁；已被其它进程持有 → ErrBusy。
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := winLock(f); err != nil {
		_ = f.Close()
		// 仅「已被占用」映射 ErrBusy；权限不足等设施故障原样返回，
		// 避免调用方把加锁失败误报为「已被打开」。
		if isBusyErr(err) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, path)
		}
		return nil, fmt.Errorf("lockfile: 加锁失败 %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release 释放锁（幂等）；未释放则进程退出时由 OS 回收。
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = winUnlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

// winLock 对文件首字节加非阻塞排他锁（LOCKFILE_FAIL_IMMEDIATELY → 已锁即失败，不等待）。
func winLock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
}

// isBusyErr 判定 LockFileEx 错误是否确为「已被占用」：仅锁冲突 / 共享冲突 → true；
// 权限不足等其它错误 → false（不应误报 ErrBusy）。
func isBusyErr(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}

// winUnlock 释放文件首字节锁。
func winUnlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
