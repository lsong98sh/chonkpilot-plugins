package filetool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectEncodingChardetAndBinary(t *testing.T) {
	text := "这是一个用于编码检测的中文测试文件，包含足够多的常用汉字。"
	gbk, err := encodeByName(text, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	det := detectEncoding(gbk)
	if det.Binary || normalizeEncoding(det.Encoding) != "gbk" {
		t.Fatalf("GBK 检测=%+v", det)
	}
	if det.Confidence == "low" {
		t.Fatalf("GBK 置信度过低: %+v", det)
	}

	bin := []byte{0x00, 0x01, 0x02, 0x00, 0xFF}
	if det := detectEncoding(bin); !det.Binary {
		t.Fatalf("NUL 字节应判为二进制: %+v", det)
	}

	// 极短输入不应触发 chardet panic。
	if det := detectEncoding([]byte{0x80}); det.Binary {
		t.Fatalf("单字节不应判二进制: %+v", det)
	}
}

func TestReadExplicitEncodingFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.txt")
	content := "中文内容测试：显式编码失败后应自动识别。"
	gbk, err := encodeByName(content, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, gbk, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "encoding": "utf-8"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("read=%v", r)
	}
	if r["content"] != content || r["encoding"] != "gbk" {
		t.Fatalf("fallback 内容/编码=%v", r)
	}
	if r["encoding_requested"] != "utf-8" || r["encoding_fallback"] != true {
		t.Fatalf("fallback 标识=%v", r)
	}
}

func TestWriteDefaultEncodingNewAndExisting(t *testing.T) {
	dir := t.TempDir()
	content := "写入测试中文内容"

	// 新文件：无显式 encoding → 配置默认 GBK。
	newPath := filepath.Join(dir, "new.txt")
	out, _ := runTool(t, "file_write", map[string]any{
		"defaultEncoding": "gbk",
		"targets":         []any{map[string]any{"path": newPath, "content": content}},
	})
	if r := firstResult(t, out); r["status"] != statusOK || r["encoding"] != "gbk" {
		t.Fatalf("new=%v", r)
	}
	if b, _ := os.ReadFile(newPath); !bytes.Equal(b, mustEncode(t, content, "gbk")) {
		t.Fatalf("新文件不是 GBK: %q", b)
	}

	// 已有 UTF-8：保持原编码，不因默认 GBK 改变。
	utf8Path := filepath.Join(dir, "utf8.txt")
	if err := os.WriteFile(utf8Path, []byte("旧 UTF-8 内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_write", map[string]any{
		"defaultEncoding": "gbk",
		"targets":         []any{map[string]any{"path": utf8Path, "content": content}},
	})
	if r := firstResult(t, out); r["encoding"] != "utf-8" {
		t.Fatalf("UTF-8 旧文件应保持: %v", r)
	}
	if b, _ := os.ReadFile(utf8Path); string(b) != content {
		t.Fatalf("UTF-8 旧文件内容=%q", b)
	}

	// 已有 ANSI(GBK)：配置默认 UTF-8 → 转默认字符集。
	ansiPath := filepath.Join(dir, "ansi.txt")
	if err := os.WriteFile(ansiPath, mustEncode(t, "旧 GBK 内容", "gbk"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_write", map[string]any{
		"defaultEncoding": "utf-8",
		"targets":         []any{map[string]any{"path": ansiPath, "content": content}},
	})
	if r := firstResult(t, out); r["encoding"] != "utf-8" {
		t.Fatalf("ANSI 应转默认: %v", r)
	}
	if b, _ := os.ReadFile(ansiPath); string(b) != content {
		t.Fatalf("ANSI 转 UTF-8 内容=%q", b)
	}

	// 显式 encoding 覆盖默认。
	explicitPath := filepath.Join(dir, "explicit.txt")
	out, _ = runTool(t, "file_write", map[string]any{
		"defaultEncoding": "gbk",
		"targets":         []any{map[string]any{"path": explicitPath, "content": content, "encoding": "utf-8"}},
	})
	if r := firstResult(t, out); r["encoding"] != "utf-8" {
		t.Fatalf("显式 encoding 应覆盖: %v", r)
	}
}

func TestPatchAnsiToDefaultEncoding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ansi.txt")
	if err := os.WriteFile(path, mustEncode(t, "one\n中文旧行\nthree\n", "gbk"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_patch", map[string]any{
		"defaultEncoding": "utf-8",
		"targets": []any{map[string]any{
			"path":  path,
			"patch": "--- a\n+++ b\n@@ -1,3 +1,3 @@\n one\n-中文旧行\n+中文新行\n three\n",
		}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK || r["encoding"] != "utf-8" {
		t.Fatalf("patch=%v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != "one\n中文新行\nthree\n" {
		t.Fatalf("patch 后内容=%q", b)
	}

	// UTF-8 源文件：默认 GBK 不应改变原编码。
	utf8Path := filepath.Join(dir, "utf8.txt")
	if err := os.WriteFile(utf8Path, []byte("one\n中文旧行\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_patch", map[string]any{
		"defaultEncoding": "gbk",
		"targets": []any{map[string]any{
			"path":  utf8Path,
			"patch": "--- a\n+++ b\n@@ -1,3 +1,3 @@\n one\n-中文旧行\n+中文新行\n three\n",
		}},
	})
	if r := firstResult(t, out); r["status"] != statusOK || r["encoding"] != "utf-8" {
		t.Fatalf("UTF-8 patch=%v", r)
	}
	if b, _ := os.ReadFile(utf8Path); string(b) != "one\n中文新行\nthree\n" {
		t.Fatalf("UTF-8 patch 内容=%q", b)
	}
}

func mustEncode(t *testing.T, s, enc string) []byte {
	t.Helper()
	b, err := encodeByName(s, enc)
	if err != nil {
		t.Fatalf("encode %s: %v", enc, err)
	}
	return b
}
