package fileops

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAcquireLockSerializesAndRelease：同路径锁互斥；释放后可再次获取；锁文件清理。
func TestAcquireLockSerializesAndRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	release, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if _, err := acquireLock(path, 0); err == nil {
		t.Fatal("同路径第二次加锁应立即失败（retry=0）")
	}
	release()

	if _, err := os.Stat(lockFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("释放后锁文件应删除，stat err=%v", err)
	}
	release2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("释放后应可再次加锁: %v", err)
	}
	release2()
}
