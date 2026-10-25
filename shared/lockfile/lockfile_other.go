//go:build !windows

// 非 Windows 平台 stub（项目目标平台为 Windows，此文件保证跨平台编译）：
// 不提供文件锁 → Acquire 返回 ErrUnsupported；WorkDirFree 据此放行（占用校验不生效，属预期降级）。
package lockfile

import (
	"path/filepath"
)

// canonicalPath 非 Windows：仅 Clean（大小写敏感）。
func canonicalPath(p string) string { return filepath.Clean(p) }

// Lock 是占位类型（本平台恒不返回实例）。
type Lock struct{}

// Acquire 本平台不支持 → 返回 ErrUnsupported。
func Acquire(string) (*Lock, error) { return nil, ErrUnsupported }

// Release 空实现（本平台不会有实例）。
func (l *Lock) Release() {}
