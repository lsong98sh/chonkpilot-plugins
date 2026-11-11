//go:build windows

package filetool

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteFileInUse：目标被其他应用打开（os.Open 持有）时，独占打开重试
// 3 次（50/100/200ms）后仍失败 → failed，内容不变；关闭句柄后可写入。
func TestWriteFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inuse.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "new"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed while held open: %v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("file mutated while held open: %q", b)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "new"}},
	})
	if !ok {
		t.Fatalf("write after close failed: %v", out)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content=%q want new", b)
	}
}

// TestReplaceFileInUse：file_replace 在目标被占用时 failed 且内容不变。
func TestReplaceFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "replace-inuse.txt")
	if err := os.WriteFile(path, []byte("foo bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{"path": path, "find": "foo", "replace": "baz"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed while held open: %v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != "foo bar" {
		t.Fatalf("file mutated while held open: %q", b)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{"path": path, "find": "foo", "replace": "baz"}},
	})
	if !ok {
		t.Fatalf("replace after close failed: %v", out)
	}
	if b, _ := os.ReadFile(path); string(b) != "baz bar" {
		t.Fatalf("content=%q want baz bar", b)
	}
}

// TestPatchFileInUse：file_patch 在目标被占用时 failed 且内容不变。
func TestPatchFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch-inuse.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": path, "patch": patch}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed while held open: %v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != "a\nb\nc\n" {
		t.Fatalf("file mutated while held open: %q", b)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": path, "patch": patch}},
	})
	if !ok {
		t.Fatalf("patch after close failed: %v", out)
	}
	if b, _ := os.ReadFile(path); string(b) != "a\nB\nc\n" {
		t.Fatalf("content=%q want a\\nB\\nc\\n", b)
	}
}

// TestWriteFileInUseRetrySucceeds：占用方在 60ms 后释放，写应在 50/100/200ms
// 重试轮内成功（验证重试路径而非直接失败）。
func TestWriteFileInUseRetrySucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retry.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 60ms 后释放：50ms 的首轮重试仍可能失败，100ms 轮成功。
	time.AfterFunc(60*time.Millisecond, func() { _ = f.Close() })

	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "new"}},
	})
	if !ok {
		t.Fatalf("write should succeed after holder released: %v", out)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content=%q want new", b)
	}
}
