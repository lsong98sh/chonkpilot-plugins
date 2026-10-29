package filetool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
)

// runTool 调用工具并解析结果 JSON。
func runTool(t *testing.T, tool string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res := Dispatch(tool, args)
	if res == nil {
		t.Fatalf("%s: dispatch returned nil", tool)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("%s: output is not JSON: %v\n%s", tool, err, res.Output)
	}
	return out, res.Success
}

// firstResult 取 results[0]。
func firstResult(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	rs, _ := out["results"].([]any)
	if len(rs) == 0 {
		t.Fatalf("results empty: %v", out)
	}
	r, _ := rs[0].(map[string]any)
	return r
}

// summaryOf 取 summary。
func summaryOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	s, _ := out["summary"].(map[string]any)
	return s
}

func TestParseRequestRejectsBadShapes(t *testing.T) {
	cases := []map[string]any{
		{"targets": []any{map[string]any{"path": "x"}}},                  // 缺 purpose
		{"purpose": "  ", "targets": []any{map[string]any{"path": "x"}}}, // purpose 空白
		{"purpose": "p"},                                    // 缺 targets
		{"purpose": "p", "targets": []any{}},                // 空数组
		{"purpose": "p", "targets": []any{"not-an-object"}}, // 元素非对象
		{"purpose": "p", "targets": "not-an-array"},         // 非数组
	}
	for i, args := range cases {
		res := Dispatch("file_read", args)
		if res == nil || res.Success {
			t.Fatalf("case %d: want parameter failure, got %+v", i, res)
		}
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	out, ok := runTool(t, "file_write", map[string]any{
		"purpose": "写入",
		"targets": []any{map[string]any{"path": path, "content": "line1\nline2\n"}},
	})
	if !ok {
		t.Fatalf("write failed: %v", out)
	}
	r := firstResult(t, out)
	if r["action"] != "created" {
		t.Fatalf("action=%v", r["action"])
	}

	out, ok = runTool(t, "file_read", map[string]any{
		"purpose": "读取",
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("read failed: %v", out)
	}
	r = firstResult(t, out)
	if r["content"] != "line1\nline2" {
		t.Fatalf("content=%q", r["content"])
	}
	if r["lines"].(float64) != 2 {
		t.Fatalf("lines=%v", r["lines"])
	}
}

func TestReadRangeAndInfoOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_read", map[string]any{
		"purpose": "区间",
		"targets": []any{map[string]any{"path": path, "start": 2, "limit": 2}},
	})
	if got := firstResult(t, out)["content"]; got != "b\nc" {
		t.Fatalf("range content=%q", got)
	}

	out, _ = runTool(t, "file_read", map[string]any{
		"purpose": "尾部",
		"targets": []any{map[string]any{"path": path, "tail": 1}},
	})
	if got := firstResult(t, out)["content"]; got != "d" {
		t.Fatalf("tail content=%q", got)
	}

	out, _ = runTool(t, "file_read", map[string]any{
		"purpose": "元信息",
		"targets": []any{map[string]any{"path": path, "infoOnly": true}},
	})
	r := firstResult(t, out)
	if _, has := r["content"]; has {
		t.Fatalf("infoOnly should not return content: %v", r)
	}
	if r["lines"].(float64) != 4 {
		t.Fatalf("infoOnly lines=%v", r["lines"])
	}
}

func TestWriteOverwriteGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{
		"purpose": "不覆盖",
		"targets": []any{map[string]any{"path": path, "content": "new", "overwrite": false}},
	})
	if ok {
		t.Fatal("want failure when overwrite=false and target exists")
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("status=%v", r["status"])
	}
	b, _ := os.ReadFile(path)
	if string(b) != "old" {
		t.Fatalf("file mutated: %q", b)
	}
}

func TestReplaceAndNotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_replace", map[string]any{
		"purpose": "全替换",
		"targets": []any{map[string]any{"path": path, "find": "foo", "replace": "baz"}},
	})
	if !ok {
		t.Fatalf("replace failed: %v", out)
	}
	if n := firstResult(t, out)["replacements"].(float64); n != 2 {
		t.Fatalf("replacements=%v", n)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "baz bar baz" {
		t.Fatalf("content=%q", b)
	}

	out, ok = runTool(t, "file_replace", map[string]any{
		"purpose": "找不到",
		"targets": []any{map[string]any{"path": path, "find": "nope", "replace": "x"}},
	})
	if ok {
		t.Fatal("want failure when search not found")
	}
	if firstResult(t, out)["status"] != statusFailed {
		t.Fatalf("status=%v", firstResult(t, out)["status"])
	}
}

func TestReplaceExpectMD5Skip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"purpose": "md5 不符",
		"targets": []any{map[string]any{
			"path": path, "find": "a", "replace": "z", "expectMd5": strings.Repeat("0", 32),
		}},
	})
	r := firstResult(t, out)
	if r["status"] != statusSkipped {
		t.Fatalf("status=%v (want skipped)", r["status"])
	}
	b, _ := os.ReadFile(path)
	if string(b) != "abc" {
		t.Fatalf("file mutated: %q", b)
	}
}

func TestPatchAndDiff(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "l.txt")
	right := filepath.Join(dir, "r.txt")
	if err := os.WriteFile(left, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte("a\nB\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_diff", map[string]any{
		"purpose": "比较",
		"targets": []any{map[string]any{"left": left, "right": right}},
	})
	if !ok {
		t.Fatalf("diff failed: %v", out)
	}
	r := firstResult(t, out)
	if r["identical"] != false {
		t.Fatalf("identical=%v", r["identical"])
	}
	if !strings.Contains(r["diff"].(string), "-b") || !strings.Contains(r["diff"].(string), "+B") {
		t.Fatalf("diff missing lines:\n%s", r["diff"])
	}
}

func TestDeleteGuards(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 非空目录且未声明 recursive → 失败
	out, ok := runTool(t, "file_delete", map[string]any{
		"purpose": "删目录",
		"targets": []any{map[string]any{"path": sub}},
	})
	if ok || firstResult(t, out)["status"] != statusFailed {
		t.Fatalf("want failure without recursive: %v", out)
	}
	// missingOk → skipped
	out, _ = runTool(t, "file_delete", map[string]any{
		"purpose": "不存在",
		"targets": []any{map[string]any{"path": filepath.Join(dir, "nope"), "missingOk": true}},
	})
	if firstResult(t, out)["status"] != statusSkipped {
		t.Fatalf("want skipped: %v", out)
	}
	// recursive → 成功
	out, ok = runTool(t, "file_delete", map[string]any{
		"purpose": "递归删",
		"targets": []any{map[string]any{"path": sub, "recursive": true}},
	})
	if !ok {
		t.Fatalf("recursive delete failed: %v", out)
	}
}

func TestCopyAndMove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, "out", "copy.txt")
	if out, ok := runTool(t, "file_copy", map[string]any{
		"purpose": "复制",
		"targets": []any{map[string]any{"from": src, "to": copied}},
	}); !ok {
		t.Fatalf("copy failed: %v", out)
	}
	if b, _ := os.ReadFile(copied); string(b) != "data" {
		t.Fatalf("copied content=%q", b)
	}

	moved := filepath.Join(dir, "moved.txt")
	if out, ok := runTool(t, "file_move", map[string]any{
		"purpose": "移动",
		"targets": []any{map[string]any{"from": copied, "to": moved}},
	}); !ok {
		t.Fatalf("move failed: %v", out)
	}
	if _, err := os.Stat(copied); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move")
	}
}

func TestFindGlobAndLimit(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.go", "b.go", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, ok := runTool(t, "file_find", map[string]any{
		"purpose": "找 go",
		"targets": []any{map[string]any{"root": dir, "glob": "*.go", "meta": "basic"}},
	})
	if !ok {
		t.Fatalf("find failed: %v", out)
	}
	r := firstResult(t, out)
	items, _ := r["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d (want 2)", len(items))
	}
	if r["truncated"] != false {
		t.Fatalf("truncated=%v", r["truncated"])
	}

	// limit=1 → 截断，matched_total 仍为 2
	out, _ = runTool(t, "file_find", map[string]any{
		"purpose": "限量",
		"targets": []any{map[string]any{"root": dir, "glob": "*.go", "limit": 1}},
	})
	r = firstResult(t, out)
	if items, _ := r["items"].([]any); len(items) != 1 {
		t.Fatalf("limited items=%d", len(items))
	}
	if r["truncated"] != true || r["matched_total"].(float64) != 2 {
		t.Fatalf("truncated=%v matched=%v", r["truncated"], r["matched_total"])
	}
}

func TestFindGrepHits(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hit\nmiss\nhit again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_find", map[string]any{
		"purpose": "找内容",
		"targets": []any{map[string]any{"root": dir, "grep": "hit", "maxHitsPerFile": 1}},
	})
	r := firstResult(t, out)
	items, _ := r["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d", len(items))
	}
	item := items[0].(map[string]any)
	hits, _ := item["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits=%d (want 1 by maxHitsPerFile)", len(hits))
	}
	if item["rel"] != "a.txt" {
		t.Fatalf("rel=%v", item["rel"])
	}
}

func TestDetectAndIconv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gbk.txt")
	gbk, err := encodeByName("中文内容测试", "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, gbk, 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_detect", map[string]any{
		"purpose": "检测",
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("detect failed: %v", out)
	}
	r := firstResult(t, out)
	if r["binary"] != false {
		t.Fatalf("binary=%v", r["binary"])
	}
	if r["sample"] == "" {
		t.Fatalf("sample empty")
	}
	if _, has := r["candidates"]; !has {
		t.Fatalf("candidates missing")
	}

	// 显式 from=gbk → utf-8
	out, ok = runTool(t, "file_iconv", map[string]any{
		"purpose": "转码",
		"targets": []any{map[string]any{"path": path, "from": "gbk", "to": "utf-8"}},
	})
	if !ok {
		t.Fatalf("iconv failed: %v", out)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "中文内容测试" {
		t.Fatalf("converted content=%q", b)
	}

	// 再次转同一目标 → skipped（幂等）
	out, _ = runTool(t, "file_iconv", map[string]any{
		"purpose": "重复转码",
		"targets": []any{map[string]any{"path": path, "from": "utf-8", "to": "utf-8"}},
	})
	if firstResult(t, out)["status"] != statusSkipped {
		t.Fatalf("want skipped on same encoding: %v", out)
	}
}

func TestPartialFailureSummary(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(good, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"purpose": "部分失败",
		"targets": []any{
			map[string]any{"path": good},
			map[string]any{"path": filepath.Join(dir, "missing.txt")},
		},
	})
	if ok {
		t.Fatal("want success=false when one target failed")
	}
	s := summaryOf(t, out)
	if s["ok"].(float64) != 1 || s["failed"].(float64) != 1 {
		t.Fatalf("summary=%v", s)
	}
}

// 确保 Result 类型断言可用（cli.Err 路径）。
var _ = cli.Err
