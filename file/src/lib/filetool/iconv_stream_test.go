package filetool

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
)

// assertNoIconvTemp 断言目标目录没有 file_iconv 的临时文件残留。
func assertNoIconvTemp(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".ck-iconv-*"))
	if err != nil {
		t.Fatalf("glob iconv temp: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("iconv temp files left behind: %v", matches)
	}
}

// TestIconvLargeStreaming：≥4MiB 的 GBK / UTF-8 文件走流式转码，
// 断言内容、字节数、前后 md5 正确且无临时文件残留。
func TestIconvLargeStreaming(t *testing.T) {
	dir := t.TempDir()
	unit := "中文内容测试，流式转码 abc123。\n"
	unitGBK, err := encodeByName(unit, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	repeat := (4<<20)/len(unitGBK) + 1
	content := strings.Repeat(unit, repeat)
	utf8Bytes := []byte(content)
	if len(utf8Bytes) <= 4<<20 {
		t.Fatalf("test fixture UTF-8 size=%d want >4MiB", len(utf8Bytes))
	}
	gbk, err := encodeByName(content, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if len(gbk) <= 4<<20 {
		t.Fatalf("test fixture GBK size=%d want >4MiB", len(gbk))
	}

	// GBK → UTF-8（显式 from 触发大文件 round-trip + 分段比较）。
	gbkPath := filepath.Join(dir, "large_gbk.txt")
	mustWrite(t, gbkPath, gbk)
	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": gbkPath, "from": "gbk", "to": "utf-8", "bom": "strip"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("gbk->utf8 want ok: %v", r)
	}
	if r["from"] != "gbk" || r["to"] != "utf-8" {
		t.Fatalf("from/to=%v/%v", r["from"], r["to"])
	}
	if got := int64(r["bytesBefore"].(float64)); got != int64(len(gbk)) {
		t.Fatalf("bytesBefore=%d want %d", got, len(gbk))
	}
	if got := int64(r["bytesAfter"].(float64)); got != int64(len(utf8Bytes)) {
		t.Fatalf("bytesAfter=%d want %d", got, len(utf8Bytes))
	}
	if r["md5Before"] != md5Hex(gbk) {
		t.Fatalf("md5Before=%v want %s", r["md5Before"], md5Hex(gbk))
	}
	if r["md5After"] != md5Hex(utf8Bytes) {
		t.Fatalf("md5After=%v want %s", r["md5After"], md5Hex(utf8Bytes))
	}
	if got := mustRead(t, gbkPath); !bytes.Equal(got, utf8Bytes) {
		t.Fatalf("gbk->utf8 content mismatch (len %d)", len(got))
	}
	assertNoIconvTemp(t, dir)

	// UTF-8 → UTF-16LE（显式 from 同样走大文件 round-trip；目标带 BOM）。
	u16Path := filepath.Join(dir, "large_utf8.txt")
	mustWrite(t, u16Path, utf8Bytes)
	wantU16, _, err := encodeWithEncoding(content, "utf-16le", true)
	if err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": u16Path, "from": "utf-8", "to": "utf-16le", "bom": "add"}},
	})
	r = firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("utf8->utf16 want ok: %v", r)
	}
	if r["bom"] != "utf-16le" {
		t.Fatalf("bom=%v want utf-16le", r["bom"])
	}
	if got := int64(r["bytesBefore"].(float64)); got != int64(len(utf8Bytes)) {
		t.Fatalf("utf16 bytesBefore=%d want %d", got, len(utf8Bytes))
	}
	if got := int64(r["bytesAfter"].(float64)); got != int64(len(wantU16)) {
		t.Fatalf("utf16 bytesAfter=%d want %d", got, len(wantU16))
	}
	if r["md5Before"] != md5Hex(utf8Bytes) {
		t.Fatalf("utf16 md5Before=%v want %s", r["md5Before"], md5Hex(utf8Bytes))
	}
	if r["md5After"] != md5Hex(wantU16) {
		t.Fatalf("utf16 md5After=%v want %s", r["md5After"], md5Hex(wantU16))
	}
	got := mustRead(t, u16Path)
	if !bytes.Equal(got, wantU16) {
		t.Fatalf("utf8->utf16 bytes mismatch (len %d)", len(got))
	}
	decoded, derr := encodingx.Decode("utf-16le", got)
	if derr != nil {
		t.Fatal(derr)
	}
	if decoded != content {
		t.Fatalf("utf16 decoded content mismatch (len %d)", len(decoded))
	}
	assertNoIconvTemp(t, dir)
}

// TestIconvStreamingFromMismatch：大 UTF-8 文件显式声明 from=gbk →
// from_mismatch；文件字节不变、临时文件清理干净。
func TestIconvStreamingFromMismatch(t *testing.T) {
	dir := t.TempDir()
	// 前缀 "x\u0080" 的 UTF-8 字节在 GBK 下是非法序列（0x78 不是合法的 GBK 前导字节），
	// GBK 解码会出现 U+FFFD，重新编码失败 → 稳定触发 from_mismatch。
	content := "x\u0080" + strings.Repeat("中文内容测试", (4<<20)/18+1)
	old := []byte(content)
	if len(old) <= 4<<20 {
		t.Fatalf("test fixture size=%d want >4MiB", len(old))
	}
	path := filepath.Join(dir, "large_utf8_as_gbk.txt")
	mustWrite(t, path, old)

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "gbk", "to": "utf-16le"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["error"] != "from_mismatch" {
		t.Fatalf("want failed from_mismatch: %v", r)
	}
	if got := mustRead(t, path); !bytes.Equal(got, old) {
		t.Fatalf("file mutated on from_mismatch (len %d)", len(got))
	}
	assertNoIconvTemp(t, dir)
}

// TestIconvStreamingCommitFailure：注入提交失败 → 目标恢复原内容（Windows 用
// backup 回滚；非 Windows rename 失败目标不变），partial 不置位，临时文件清理。
func TestIconvStreamingCommitFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit_fail.txt")
	old := []byte("中文内容测试")
	mustWrite(t, path, old)

	prev := commitStreamHook
	commitStreamHook = func(stage string) error {
		if stage == "commit" {
			return os.ErrPermission
		}
		return nil
	}
	defer func() { commitStreamHook = prev }()

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed on injected commit error: %v", r)
	}
	if r["partial"] != nil {
		t.Fatalf("partial should be absent after successful rollback: %v", r)
	}
	if got := mustRead(t, path); !bytes.Equal(got, old) {
		t.Fatalf("target not restored after commit failure: %q", got)
	}
	assertNoIconvTemp(t, dir)
}

// TestIconvAlreadyTargetExpectMd5：文件已是目标编码时，expectMd5 必须先于
// already_target 快判生效（与旧实现顺序一致）。
func TestIconvAlreadyTargetExpectMd5(t *testing.T) {
	dir := t.TempDir()
	content := "中文内容测试"
	src := append([]byte{0xEF, 0xBB, 0xBF}, []byte(content)...)
	path := filepath.Join(dir, "already_target.txt")
	mustWrite(t, path, src)

	good := md5Hex(src)
	bad := strings.Repeat("0", 32)
	cases := []struct {
		name   string
		args   map[string]any
		reason string
	}{
		{"auto-wrong-md5", map[string]any{"to": "utf-8", "expectMd5": bad}, "md5 mismatch"},
		{"auto-correct-md5", map[string]any{"to": "utf-8", "expectMd5": good}, "already_target"},
		{"explicit-wrong-md5", map[string]any{"from": "gbk", "to": "utf-8", "expectMd5": bad}, "md5 mismatch"},
		{"explicit-correct-md5", map[string]any{"from": "gbk", "to": "utf-8", "expectMd5": good}, "already_target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := map[string]any{"path": path}
			for k, v := range c.args {
				args[k] = v
			}
			out, _ := runTool(t, "file_iconv", map[string]any{
				"targets": []any{args},
			})
			r := firstResult(t, out)
			if r["status"] != statusSkipped || r["reason"] != c.reason {
				t.Fatalf("want skipped reason %q: %v", c.reason, r)
			}
			if c.reason == "md5 mismatch" && r["md5"] != good {
				t.Fatalf("md5=%v want %s", r["md5"], good)
			}
			if got := mustRead(t, path); !bytes.Equal(got, src) {
				t.Fatalf("file mutated: %q", got)
			}
			assertNoIconvTemp(t, dir)
		})
	}
}

// TestIconvNonTargetExpectMd5：非 already_target 时 expectMd5 正确 → 正常转换，
// 错误 → skipped md5 mismatch 且内容不变。
func TestIconvNonTargetExpectMd5(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not_target.txt")
	content := "中文内容测试"
	utf8Bytes := []byte(content)
	good := md5Hex(utf8Bytes)
	bad := strings.Repeat("0", 32)

	// 正确 md5 → 正常转换。
	mustWrite(t, path, utf8Bytes)
	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{
			"path": path, "from": "utf-8", "to": "utf-16le", "expectMd5": good,
		}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK || r["to"] != "utf-16le" {
		t.Fatalf("correct expectMd5 want ok: %v", r)
	}
	decoded, derr := encodingx.Decode("utf-16le", mustRead(t, path))
	if derr != nil {
		t.Fatal(derr)
	}
	if decoded != content {
		t.Fatalf("decoded=%q want %q", decoded, content)
	}
	assertNoIconvTemp(t, dir)

	// 错误 md5 → skipped + md5 mismatch，内容不变。
	mustWrite(t, path, utf8Bytes)
	out, _ = runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{
			"path": path, "from": "utf-8", "to": "utf-16le", "expectMd5": bad,
		}},
	})
	r = firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "md5 mismatch" {
		t.Fatalf("wrong expectMd5 want skipped md5 mismatch: %v", r)
	}
	if r["md5"] != good {
		t.Fatalf("md5=%v want %s", r["md5"], good)
	}
	if got := mustRead(t, path); !bytes.Equal(got, utf8Bytes) {
		t.Fatalf("file mutated after md5 mismatch: %q", got)
	}
	assertNoIconvTemp(t, dir)
}

// TestIconvStreamingAutoLarge：大 UTF-16LE（带 BOM）文件 auto 检测后流式转 UTF-8
// （auto 路径不做 round-trip），内容与 md5 正确、源 BOM 被跳过不会写成双重 BOM。
func TestIconvStreamingAutoLarge(t *testing.T) {
	dir := t.TempDir()
	unit := "中文内容测试流式转码"
	unitU16, _, err := encodeWithEncoding(unit, "utf-16le", true)
	if err != nil {
		t.Fatal(err)
	}
	repeat := (4<<20)/(len(unitU16)-2) + 1
	content := strings.Repeat(unit, repeat)
	u16, _, err := encodeWithEncoding(content, "utf-16le", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(u16) <= 4<<20 {
		t.Fatalf("test fixture UTF-16 size=%d want >4MiB", len(u16))
	}
	path := filepath.Join(dir, "auto_large_u16.txt")
	mustWrite(t, path, u16)

	out, _ := runTool(t, "file_iconv", map[string]any{
		"targets": []any{map[string]any{"path": path, "to": "utf-8", "bom": "strip"}},
	})
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("auto large utf16 want ok: %v", r)
	}
	if r["from"] != "utf-16le" {
		t.Fatalf("auto detected from=%v want utf-16le", r["from"])
	}
	if got := mustRead(t, path); string(got) != content {
		t.Fatalf("auto converted content mismatch (len %d)", len(got))
	}
	if r["md5Before"] != md5Hex(u16) {
		t.Fatalf("auto md5Before=%v want %s", r["md5Before"], md5Hex(u16))
	}
	if r["md5After"] != md5Hex([]byte(content)) {
		t.Fatalf("auto md5After=%v want %s", r["md5After"], md5Hex([]byte(content)))
	}
	assertNoIconvTemp(t, dir)
}
