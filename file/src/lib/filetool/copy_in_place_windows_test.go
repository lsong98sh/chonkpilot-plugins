//go:build windows

package filetool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCopyTargetInUse：目标被其他应用打开（os.Open 持有）时，copy 的独占打开
// 返回 errFileInUse → failed + code FILE_IN_USE，目标内容不变；关闭句柄后复制成功。
func TestCopyTargetInUse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("new-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(dst, []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "FILE_IN_USE" {
		t.Fatalf("want failed + FILE_IN_USE: %v", r)
	}
	if r["partial"] == true {
		t.Fatalf("独占打开失败时目标未被写入，不应标记 partial: %v", r)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "old-content" {
		t.Fatalf("目标内容不应改变: %q", b)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	if !ok {
		t.Fatalf("关闭句柄后 copy 应成功: %v", out)
	}
	if r := firstResult(t, out); r["status"] != statusOK {
		t.Fatalf("status=%v err=%v", r["status"], r["error"])
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new-content" {
		t.Fatalf("目标内容=%q err=%v", b, err)
	}
}
