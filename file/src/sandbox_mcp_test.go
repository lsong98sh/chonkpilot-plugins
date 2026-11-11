package main

// sandbox_mcp_test.go —— `_meta.chonkpilot.sandbox` 写白名单校验（C4，2026-11-03）。
//
// 口径：
//   - 写工具（write/patch/replace/copy/move/delete/iconv）的目标必须落在 writable_dirs 内；
//   - 只读工具（read/find/diff/detect）不受限；
//   - 未启用沙箱（无 meta.sandbox / enabled=false）→ 不校验（保持旧行为）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sandboxMeta(dirs ...string) map[string]any {
	return map[string]any{"chonkpilot": map[string]any{
		"sandbox": map[string]any{"enabled": true, "writable_dirs": dirs},
	}}
}

// TestFileToolsSandboxWritable：白名单内可写、白名单外失败；只读工具不受限；未启用不校验。
func TestFileToolsSandboxWritable(t *testing.T) {
	cs := newMCPFace(t)
	allowed := t.TempDir()
	denied := t.TempDir()
	meta := sandboxMeta(allowed)

	okPath := filepath.Join(allowed, "ok.txt")
	if text, isErr := callTool(t, cs, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": okPath, "content": "ok\n"}},
	}, meta); isErr {
		t.Fatalf("白名单内写入应成功: %s", text)
	}
	if _, err := os.Stat(okPath); err != nil {
		t.Fatalf("白名单内文件未落盘: %v", err)
	}

	badPath := filepath.Join(denied, "bad.txt")
	text, isErr := callTool(t, cs, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": badPath, "content": "bad\n"}},
	}, meta)
	if !isErr || !strings.Contains(text, "sandbox:") {
		t.Fatalf("白名单外写入应失败并带 sandbox 说明: isErr=%v text=%s", isErr, text)
	}
	if _, err := os.Stat(badPath); err == nil {
		t.Fatalf("白名单外文件不应被创建: %s", badPath)
	}

	// 只读工具不受限（读白名单外的文件）。
	outside := filepath.Join(denied, "readable.txt")
	if err := os.WriteFile(outside, []byte("readable\n"), 0o644); err != nil {
		t.Fatalf("准备只读文件: %v", err)
	}
	if text, isErr := callTool(t, cs, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": outside}},
	}, meta); isErr {
		t.Fatalf("只读工具不应受写白名单限制: %s", text)
	}

	// copy：to 越界 → 失败；from 越界（删除语义）→ 失败。
	if text, isErr := callTool(t, cs, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": okPath, "to": filepath.Join(denied, "copy.txt")}},
	}, meta); !isErr || !strings.Contains(text, "sandbox:") {
		t.Fatalf("copy 目标越界应失败: isErr=%v text=%s", isErr, text)
	}
	if text, isErr := callTool(t, cs, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": outside, "to": filepath.Join(allowed, "moved.txt")}},
	}, meta); !isErr || !strings.Contains(text, "sandbox:") {
		t.Fatalf("move 源越界应失败: isErr=%v text=%s", isErr, text)
	}
	// patch / delete 同属写工具：目标越界同样在预检阶段整次拒绝（不进入 handler）。
	if text, isErr := callTool(t, cs, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": outside, "patch": "@@ -1,1 +1,1 @@\n-x\n+y\n"}},
	}, meta); !isErr || !strings.Contains(text, "sandbox:") {
		t.Fatalf("patch 目标越界应失败: isErr=%v text=%s", isErr, text)
	}
	if text, isErr := callTool(t, cs, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": outside}},
	}, meta); !isErr || !strings.Contains(text, "sandbox:") {
		t.Fatalf("delete 目标越界应失败: isErr=%v text=%s", isErr, text)
	}

	// 未启用沙箱（无 meta.sandbox）→ 不校验：白名单外写入成功。
	freePath := filepath.Join(denied, "free.txt")
	if text, isErr := callTool(t, cs, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": freePath, "content": "free\n"}},
	}, nil); isErr {
		t.Fatalf("未启用沙箱时不应限制写入: %s", text)
	}
}
