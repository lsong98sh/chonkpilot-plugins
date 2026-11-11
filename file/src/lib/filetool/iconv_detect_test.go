package filetool

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
)

// resultsOf 取 results 数组（按入参顺序）。
func resultsOf(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, _ := out["results"].([]any)
	rs := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		rs = append(rs, it.(map[string]any))
	}
	return rs
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ─── file_detect ─────────────────────────────────────────────────

// TestDetectSampleBytesTooLarge：sampleBytes 超过 4MiB 直接 failed SAMPLE_TOO_LARGE，
// 不读取、不分配（无 panic）。
func TestDetectSampleBytesTooLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.txt")
	mustWrite(t, path, []byte("hello\n"))

	out, _ := runTool(t, "file_detect", map[string]any{
		"targets": []any{map[string]any{"path": path, "sampleBytes": maxSampleBytes + 1}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed: %v", r)
	}
	if r["code"] != "SAMPLE_TOO_LARGE" {
		t.Fatalf("code=%v want SAMPLE_TOO_LARGE", r["code"])
	}
	if r["error"] != "sampleBytes too large (max 4MiB)" {
		t.Fatalf("error=%v", r["error"])
	}
}

// TestDetectEmptyFileFields：空文件返回 encoding=unknown、confidence=low、
// binary=false、bom=none（不返回 sample/candidates）。
func TestDetectEmptyFileFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	mustWrite(t, path, nil)

	out, _ := runTool(t, "file_detect", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("want ok: %v", r)
	}
	if r["encoding"] != "unknown" {
		t.Fatalf("encoding=%v want unknown", r["encoding"])
	}
	if r["confidence"] != "low" {
		t.Fatalf("confidence=%v want low", r["confidence"])
	}
	if r["binary"] != false {
		t.Fatalf("binary=%v want false", r["binary"])
	}
	if r["bom"] != "none" {
		t.Fatalf("bom=%v want none", r["bom"])
	}
	if _, has := r["sample"]; has {
		t.Fatalf("empty file should not return sample: %v", r)
	}
	if _, has := r["candidates"]; has {
		t.Fatalf("empty file should not return candidates: %v", r)
	}
}

// TestDetectBinaryFields：含 NUL → encoding=binary、confidence=high、binary=true，
// 无 sample/candidates。
func TestDetectBinaryFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bin.dat")
	mustWrite(t, path, []byte{'A', 0x00, 'B'})

	out, _ := runTool(t, "file_detect", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	r := firstResult(t, out)
	if r["encoding"] != "binary" {
		t.Fatalf("encoding=%v want binary", r["encoding"])
	}
	if r["confidence"] != "high" {
		t.Fatalf("confidence=%v want high", r["confidence"])
	}
	if r["binary"] != true {
		t.Fatalf("binary=%v want true", r["binary"])
	}
	if _, has := r["sample"]; has {
		t.Fatalf("binary should not return sample: %v", r)
	}
	if _, has := r["candidates"]; has {
		t.Fatalf("binary should not return candidates: %v", r)
	}
}

// TestDetectSampleBytesFullHead：小文件 + 大 sampleBytes → 头部完整读入，
// sample 内容正确（sampleBytes 只控制检测窗口，sample 最多 512 rune）。
func TestDetectSampleBytesFullHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "head.txt")
	content := strings.Repeat("line\n", 100)
	mustWrite(t, path, []byte(content))

	out, _ := runTool(t, "file_detect", map[string]any{
		"targets": []any{map[string]any{"path": path, "sampleBytes": 64 << 10}},
	})
	r := firstResult(t, out)
	if r["sample"] != content {
		t.Fatalf("sample=%q want full head", r["sample"])
	}
}

// ─── file_iconv ──────────────────────────────────────────────────

// TestIconvFromMismatchNoCorruption：UTF-8 中文文件显式声明 from=gbk、to=utf-8。
// chardet 高置信识别为 UTF-8（已是目标编码）→ skipped already_target；字节不变（绝不写）。
func TestIconvFromMismatchNoCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "utf8.txt")
	old := []byte("这是一个测试文件")
	mustWrite(t, path, old)

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "gbk", "to": "utf-8"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "already_target" {
		t.Fatalf("want skipped already_target: %v", r)
	}
	if got := mustRead(t, path); !bytes.Equal(got, old) {
		t.Fatalf("file corrupted: %q", got)
	}
}

// TestIconvIdempotentSecondCall：GBK → UTF-8 第一次 ok；第二次同参数（from=gbk）
// skipped already_target，内容不变（不会把 UTF-8 当 GBK 再解码写回）。
func TestIconvIdempotentSecondCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.txt")
	content := "中文内容测试"
	gbk, err := encodeByName(content, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, gbk)

	targets := []any{map[string]any{"path": path, "from": "gbk", "to": "utf-8"}}
	out, _ := runTool(t, "file_iconv", map[string]any{"targets": targets})
	if r := firstResult(t, out); r["status"] != statusOK {
		t.Fatalf("first call want ok: %v", r)
	}
	afterFirst := mustRead(t, path)
	if string(afterFirst) != content {
		t.Fatalf("converted content=%q", afterFirst)
	}

	out, _ = runTool(t, "file_iconv", map[string]any{"targets": targets})
	r := firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "already_target" {
		t.Fatalf("second call want skipped already_target: %v", r)
	}
	if got := mustRead(t, path); !bytes.Equal(got, afterFirst) {
		t.Fatalf("second call mutated file: %q", got)
	}
}

// TestIconvAutoTwice：auto → UTF-8 两次；第二次 skipped already_target，内容不变。
func TestIconvAutoTwice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "u16.txt")
	content := "中文内容测试"
	u16, _, err := encodeWithEncoding(content, "utf-16le", true)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, u16)

	targets := []any{map[string]any{"path": path, "to": "utf-8", "bom": "strip"}}
	out, _ := runTool(t, "file_iconv", map[string]any{"targets": targets})
	if r := firstResult(t, out); r["status"] != statusOK || r["from"] != "utf-16le" {
		t.Fatalf("first call want ok from utf-16le: %v", r)
	}
	afterFirst := mustRead(t, path)
	if string(afterFirst) != content {
		t.Fatalf("converted content=%q", afterFirst)
	}

	out, _ = runTool(t, "file_iconv", map[string]any{"targets": targets})
	r := firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "already_target" {
		t.Fatalf("second call want skipped already_target: %v", r)
	}
	if got := mustRead(t, path); !bytes.Equal(got, afterFirst) {
		t.Fatalf("second call mutated file: %q", got)
	}
}

// TestIconvBOMSource：UTF-8 BOM 源 → UTF-16LE，目标只有一个 BOM，
// 解码后内容不含 U+FEFF。
func TestIconvBOMSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.txt")
	content := "中文内容测试"
	src := append([]byte{0xEF, 0xBB, 0xBF}, []byte(content)...)
	mustWrite(t, path, src)

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("want ok: %v", r)
	}
	if r["bom"] != "utf-16le" {
		t.Fatalf("bom=%v want utf-16le", r["bom"])
	}
	got := mustRead(t, path)
	want, _, err := encodeWithEncoding(content, "utf-16le", true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("utf-16 bytes mismatch: %x want %x", got, want)
	}
	decoded, derr := encodingx.Decode("utf-16le", got)
	if derr != nil {
		t.Fatal(derr)
	}
	if strings.ContainsRune(decoded, '\ufeff') {
		t.Fatalf("decoded content still contains U+FEFF: %q", decoded)
	}
	if decoded != content {
		t.Fatalf("decoded=%q want %q", decoded, content)
	}
}

// TestIconvExpectMd5：expectMd5 不符 → skipped + 当前 md5，内容不变。
func TestIconvExpectMd5(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "md5.txt")
	old := []byte("中文内容测试")
	mustWrite(t, path, old)

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{
			"path": path, "from": "utf-8", "to": "utf-16le",
			"expectMd5": strings.Repeat("0", 32),
		}},
	})
	r := firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "md5 mismatch" {
		t.Fatalf("want skipped md5 mismatch: %v", r)
	}
	if r["md5"] != md5Hex(old) {
		t.Fatalf("md5=%v want current %s", r["md5"], md5Hex(old))
	}
	if got := mustRead(t, path); !bytes.Equal(got, old) {
		t.Fatalf("file mutated: %q", got)
	}
}

// TestIconvUnsupportedTo / TestIconvUnsupportedFrom：不支持的编码名 → failed
// unsupported_encoding（不是 to is required）。
func TestIconvUnsupportedTo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	mustWrite(t, path, []byte("abc"))

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "to": "utf-32"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["error"] != "unsupported_encoding: utf-32" {
		t.Fatalf("want unsupported_encoding: %v", r)
	}
}

func TestIconvUnsupportedFrom(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	mustWrite(t, path, []byte("abc"))

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "utf-32", "to": "utf-8"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["error"] != "unsupported_encoding: utf-32" {
		t.Fatalf("want unsupported_encoding: %v", r)
	}
}

// TestIconvDuplicateTarget：同批重复 path → 后项 failed duplicate target，
// 第一项正常执行（不合并、不做 last-wins）。
func TestIconvDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.txt")
	content := "中文内容测试"
	mustWrite(t, path, []byte(content))

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"},
			map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"},
		},
	})
	rs := resultsOf(t, out)
	if len(rs) != 2 {
		t.Fatalf("results=%d want 2", len(rs))
	}
	if rs[0]["status"] != statusOK {
		t.Fatalf("first occurrence should execute: %v", rs[0])
	}
	if rs[1]["status"] != statusFailed || rs[1]["error"] != "duplicate target" {
		t.Fatalf("second occurrence want duplicate target: %v", rs[1])
	}
	s := summaryOf(t, out)
	if s["total"].(float64) != 2 {
		t.Fatalf("summary.total=%v want 2 (input count)", s["total"])
	}
	// 只转换一次。
	decoded, err := encodingx.Decode("utf-16le", mustRead(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if decoded != content {
		t.Fatalf("decoded=%q want %q", decoded, content)
	}
}
