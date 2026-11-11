//go:build windows

package filetool

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
)

// TestIconvStreamingRollbackPartial：Windows 就地提交失败且回滚也失败时，
// 结果必须标 partial:true（目标可能不完整），并清理临时文件。
func TestIconvStreamingRollbackPartial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.txt")
	old := []byte("中文内容测试")
	mustWrite(t, path, old)

	prev := commitStreamHook
	commitStreamHook = func(stage string) error {
		return errors.New("injected " + stage + " failure")
	}
	defer func() { commitStreamHook = prev }()

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed: %v", r)
	}
	if r["partial"] != true {
		t.Fatalf("want partial:true when rollback fails: %v", r)
	}
	msg, _ := r["error"].(string)
	if !bytes.Contains([]byte(msg), []byte("rollback failed")) {
		t.Fatalf("error should mention rollback failure: %v", r["error"])
	}
	// 注入在就地写之前失败，目标内容不应被改动。
	if got := mustRead(t, path); !bytes.Equal(got, old) {
		t.Fatalf("target unexpectedly changed: %q", got)
	}
	assertNoIconvTemp(t, dir)
}
