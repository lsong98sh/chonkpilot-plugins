//go:build windows

package filetool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDeleteFileInUse：文件被持有期间 delete 在重试后仍 failed，文件保留；关闭后可删。
func TestDeleteFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inuse.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	out, _ := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "FILE_IN_USE" {
		t.Fatalf("want FILE_IN_USE failure: %v", r)
	}
	if r["locked_path"] != path {
		t.Fatalf("locked_path=%v want %s", r["locked_path"], path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("in-use file should remain: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("delete after close failed: %v", out)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be removed after handle closed: %v", err)
	}
}

// TestDeleteFileInUseRetrySucceeds：持有 60ms 后释放，delete 应在重试轮成功。
func TestDeleteFileInUseRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.txt")
	if err := os.WriteFile(path, []byte("retry"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = f.Close()
		close(done)
	}()

	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	<-done
	if !ok {
		t.Fatalf("delete should succeed after retry: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("status=%v err=%v", r["status"], r["error"])
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be removed: %v", err)
	}
}

// TestDeleteAllOrNone：目录 2 个文件、持有其中一个 → failed，两个文件都仍在。
func TestDeleteAllOrNone(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(tree, "a.txt")
	b := filepath.Join(tree, "b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	out, _ := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": tree, "recursive": true}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "FILE_IN_USE" {
		t.Fatalf("want FILE_IN_USE failure: %v", r)
	}
	if files, _ := r["locked_files"].([]any); len(files) != 1 || files[0] != a {
		t.Fatalf("locked_files=%v want [%s]", r["locked_files"], a)
	}
	for _, p := range []string{a, b} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("all-or-none violated, %s should remain: %v", p, err)
		}
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatalf("tree should remain: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": tree, "recursive": true}},
	})
	if !ok {
		t.Fatalf("delete after close failed: %v", out)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree should be removed: %v", err)
	}
}

// TestHoldExclusive 验证持有句柄期间他人无法打开。
func TestHoldExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hold.txt")
	if err := os.WriteFile(path, []byte("hold"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := md5File(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := holdExclusive(path, true)
	if err != nil {
		t.Fatalf("hold idle file: %v", err)
	}
	got, err := h.md5()
	if err != nil {
		t.Fatalf("md5 from handle: %v", err)
	}
	if got != want {
		t.Fatalf("md5=%s want %s", got, want)
	}
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Fatalf("held file should not be openable by others")
	}
	if err := h.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("file should be openable after close: %v", err)
	}
	_ = f.Close()
}

// TestHoldExclusiveMarkDelete 验证 delete-on-close 标记后关闭即删除。
func TestHoldExclusiveMarkDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mark.txt")
	if err := os.WriteFile(path, []byte("mark"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := holdExclusive(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.markDeleteOnClose(); err != nil {
		t.Fatalf("mark delete-on-close: %v", err)
	}
	if err := h.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("file should be deleted on close: %v", err)
	}
}
