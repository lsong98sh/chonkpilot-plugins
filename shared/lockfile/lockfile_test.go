//go:build windows

// lockfile_test.go — 跨进程文件锁原语（I-74）白盒：互斥 / 释放 / work-dir 键归一化。
package lockfile

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAcquireMutualExclusion：同路径互斥；Release 后可再取。
func TestAcquireMutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次获取失败: %v", err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("占用中应返回 ErrBusy，got %v", err)
	}
	l1.Release()
	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后应可再次获取: %v", err)
	}
	l2.Release()
}

// TestWorkDirLockPathNormalized：同一目录的不同拼写（大小写 / 尾分隔符）→ 同一把锁；
// 不同目录 → 不同锁（否则占用校验会互相误判）。
func TestWorkDirLockPathNormalized(t *testing.T) {
	a := WorkDirLockPath(`E:\BizWorks\Proj`)
	b := WorkDirLockPath(`e:\bizworks\proj\`)
	if a != b {
		t.Fatalf("归一化后应得同一锁路径：%s vs %s", a, b)
	}
	if c := WorkDirLockPath(`E:\BizWorks\Other`); c == a {
		t.Fatal("不同目录不应共用锁路径")
	}
}

// TestAcquireWorkDirAndFree：AcquireWorkDir / WorkDirFree 三态（空闲 → 占用 → 释放）。
func TestAcquireWorkDirAndFree(t *testing.T) {
	dir := t.TempDir()
	if !WorkDirFree(dir) {
		t.Fatal("未占用应判为空闲")
	}
	l, err := AcquireWorkDir(dir)
	if err != nil {
		t.Fatalf("AcquireWorkDir 失败: %v", err)
	}
	if WorkDirFree(dir) {
		t.Fatal("占用期间应判为不空闲")
	}
	l.Release()
	if !WorkDirFree(dir) {
		t.Fatal("释放后应判为空闲")
	}
}

// TestIsBusyErr：仅锁 / 共享冲突映射「已被占用」；权限不足等设施故障不误报 ErrBusy。
func TestIsBusyErr(t *testing.T) {
	if !isBusyErr(windows.ERROR_LOCK_VIOLATION) {
		t.Fatal("ERROR_LOCK_VIOLATION 应判为占用")
	}
	if !isBusyErr(windows.ERROR_SHARING_VIOLATION) {
		t.Fatal("ERROR_SHARING_VIOLATION 应判为占用")
	}
	if isBusyErr(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("ERROR_ACCESS_DENIED 不应判为占用")
	}
}
