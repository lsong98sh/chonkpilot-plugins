//go:build windows

package filetool

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBatchLockSkipThenRetry：批 targets 顺序 [A, B]，调用前持有 A。
// 第一遍 A 记 pending 后继续处理 B（B 正常成功）；3 轮重试后 A 单独 failed
// （code=FILE_IN_USE），A 的占用不阻塞 B。关闭 A 后同一调用可成功。
func TestBatchLockSkipThenRetry(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "held-a.txt")
	pathB := filepath.Join(dir, "free-b.txt")
	if err := os.WriteFile(pathA, []byte("a-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, []byte("b-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(pathA)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()

	targets := []any{
		map[string]any{"path": pathA, "content": "a-new"},
		map[string]any{"path": pathB, "content": "b-new"},
	}
	out, _ := runTool(t, "file_write", map[string]any{"targets": targets})
	rs := out["results"].([]any)
	if len(rs) != 2 {
		t.Fatalf("results 长度=%d want 2", len(rs))
	}
	if a := rs[0].(map[string]any); a["status"] != statusFailed || a["code"] != "FILE_IN_USE" {
		t.Fatalf("results[0] (A) 应 failed FILE_IN_USE：%v", a)
	}
	if b := rs[1].(map[string]any); b["status"] != statusOK {
		t.Fatalf("results[1] (B) 应 ok（A 不应阻塞 B）：%v", b)
	}
	if b, _ := os.ReadFile(pathB); string(b) != "b-new" {
		t.Fatalf("B content=%q want b-new", b)
	}
	if b, _ := os.ReadFile(pathA); string(b) != "a-old" {
		t.Fatalf("A 被占用时不应改写：%q", b)
	}

	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{"targets": targets})
	if !ok {
		t.Fatalf("关闭 A 后同一调用应成功：%v", out)
	}
	if b, _ := os.ReadFile(pathA); string(b) != "a-new" {
		t.Fatalf("A content=%q want a-new", b)
	}
	if b, _ := os.ReadFile(pathB); string(b) != "b-new" {
		t.Fatalf("B content=%q want b-new", b)
	}
}
