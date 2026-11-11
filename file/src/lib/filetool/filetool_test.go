package filetool

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-util/encodingx"
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
		{},                                  // 缺 targets
		{"targets": []any{}},                // 空数组
		{"targets": []any{"not-an-object"}}, // 元素非对象
		{"targets": "not-an-array"},         // 非数组
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
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("read failed: %v", out)
	}
	r = firstResult(t, out)
	if r["content"] != "line1\nline2" {
		t.Fatalf("content=%q", r["content"])
	}
	if r["total"].(float64) != 2 {
		t.Fatalf("total=%v", r["total"])
	}
}

func TestReadRangeAndInfoOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "start": 2, "limit": 2}},
	})
	if got := firstResult(t, out)["content"]; got != "b\nc" {
		t.Fatalf("range content=%q", got)
	}

	out, _ = runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "tail": 1}},
	})
	if got := firstResult(t, out)["content"]; got != "d" {
		t.Fatalf("tail content=%q", got)
	}

	out, _ = runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "infoOnly": true}},
	})
	r := firstResult(t, out)
	if _, has := r["content"]; has {
		t.Fatalf("infoOnly should not return content: %v", r)
	}
	if r["total"].(float64) != 4 {
		t.Fatalf("infoOnly total=%v", r["total"])
	}
}

func TestWriteOverwriteGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "new", "overwrite": false}},
	})
	if !ok {
		t.Fatalf("write dispatch should be batch-success: %v", out)
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
		"targets": []any{map[string]any{"path": path, "find": "missing", "replace": "x"}},
	})
	if !ok {
		t.Fatalf("batch dispatch should stay success on per-item failure: %v", out)
	}
	if firstResult(t, out)["status"] != statusFailed {
		t.Fatalf("want failed item: %v", out)
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
		"targets": []any{map[string]any{"path": sub}},
	})
	if firstResult(t, out)["status"] != statusFailed {
		t.Fatalf("want failed item without recursive: %v", out)
	}
	// missingOk → skipped
	out, _ = runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": filepath.Join(dir, "nope"), "missingOk": true}},
	})
	if firstResult(t, out)["status"] != statusSkipped {
		t.Fatalf("want skipped: %v", out)
	}
	// recursive → 成功
	out, ok = runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": sub, "recursive": true}},
	})
	if !ok {
		t.Fatalf("recursive delete failed: %v", out)
	}
}

func TestDeleteEmptyDirRequiresRecursive(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": empty}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed without recursive: %v", r)
	}
	if !strings.Contains(r["error"].(string), "recursive") {
		t.Fatalf("error=%v", r["error"])
	}
	if _, err := os.Stat(empty); err != nil {
		t.Fatalf("empty dir should remain: %v", err)
	}
}

func TestDeleteDirStats(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": tree, "recursive": true}},
	})
	if !ok {
		t.Fatalf("recursive delete failed: %v", out)
	}
	r := firstResult(t, out)
	if r["type"] != "dir" {
		t.Fatalf("type=%v", r["type"])
	}
	if got := r["removedFiles"].(float64); got != 2 {
		t.Fatalf("removedFiles=%v want 2", r["removedFiles"])
	}
	if got := r["removedBytes"].(float64); got != 8 {
		t.Fatalf("removedBytes=%v want 8", r["removedBytes"])
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree should be removed: %v", err)
	}
}

func TestDeleteDirWithMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "multi")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(tree, "a.txt"):           "aaa",
		filepath.Join(tree, "b.txt"):           "bbbb",
		filepath.Join(tree, "nested", "c.txt"): "ccccc",
		filepath.Join(tree, "nested", "d.txt"): "dddddd",
	}
	var wantBytes int64
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		wantBytes += int64(len(content))
	}
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": tree, "recursive": true}},
	})
	if !ok {
		t.Fatalf("recursive delete failed: %v", out)
	}
	r := firstResult(t, out)
	if r["type"] != "dir" {
		t.Fatalf("type=%v", r["type"])
	}
	if got := r["removedFiles"].(float64); got != float64(len(files)) {
		t.Fatalf("removedFiles=%v want %d", r["removedFiles"], len(files))
	}
	if got := r["removedBytes"].(float64); got != float64(wantBytes) {
		t.Fatalf("removedBytes=%v want %d", r["removedBytes"], wantBytes)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("tree should be removed: %v", err)
	}
}

func TestDeleteMaxFiles(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "many")
	if err := os.Mkdir(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxDeleteFiles; i++ {
		name := filepath.Join(tree, fmt.Sprintf("f%04d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": tree, "recursive": true}},
	})
	r := firstResult(t, out)
	if r["status"] != statusFailed {
		t.Fatalf("want failed for >%d files: %v", maxDeleteFiles, r)
	}
	if !strings.Contains(r["error"].(string), "too many files") {
		t.Fatalf("error=%v", r["error"])
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatalf("over-limit tree should remain: %v", err)
	}
}

func TestDeleteSymlinkOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// 不传 recursive：symlink 只删链接本身。
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": link}},
	})
	if !ok {
		t.Fatalf("symlink delete failed: %v", out)
	}
	r := firstResult(t, out)
	if r["type"] != "symlink" {
		t.Fatalf("type=%v want symlink", r["type"])
	}
	if lt, _ := r["link_target"].(string); lt != target {
		t.Fatalf("link_target=%v want %s", r["link_target"], target)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("link should be removed: %v", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "payload" {
		t.Fatalf("target should remain intact: %q err=%v", b, err)
	}
}

func TestDeleteBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "broken")
	if err := os.Symlink(filepath.Join(dir, "missing.txt"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// 断链 symlink 在 Lstat 下仍算存在，正常删除链接。
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{map[string]any{"path": link}},
	})
	if !ok {
		t.Fatalf("broken symlink delete failed: %v", out)
	}
	r := firstResult(t, out)
	if r["type"] != "symlink" {
		t.Fatalf("type=%v want symlink", r["type"])
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("broken link should be removed: %v", err)
	}
}

func TestDeleteDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_delete", map[string]any{
		"targets": []any{
			map[string]any{"path": path},
			map[string]any{"path": path},
		},
	})
	if !ok {
		t.Fatalf("per-item failure should not set IsError: %v", out)
	}
	rs, _ := out["results"].([]any)
	if len(rs) != 2 {
		t.Fatalf("results=%d", len(rs))
	}
	first, _ := rs[0].(map[string]any)
	second, _ := rs[1].(map[string]any)
	if first["status"] != statusOK {
		t.Fatalf("first status=%v", first["status"])
	}
	if second["status"] != statusFailed || !strings.Contains(second["error"].(string), "duplicate target") {
		t.Fatalf("second=%v", second)
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
		"targets": []any{map[string]any{"from": src, "to": copied}},
	}); !ok {
		t.Fatalf("copy failed: %v", out)
	}
	if b, _ := os.ReadFile(copied); string(b) != "data" {
		t.Fatalf("copied content=%q", b)
	}

	moved := filepath.Join(dir, "moved.txt")
	if out, ok := runTool(t, "file_move", map[string]any{
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
		"targets": []any{
			map[string]any{"path": good},
			map[string]any{"path": filepath.Join(dir, "missing.txt")},
		},
	})
	if !ok {
		t.Fatalf("batch dispatch should stay success on partial failure: %v", out)
	}
	s := summaryOf(t, out)
	if s["ok"].(float64) != 1 || s["failed"].(float64) != 1 {
		t.Fatalf("summary=%v", s)
	}
}

// TestReadMetricsAndGlobalBudget：total/file_md5 为全文；maxBytes 为跨文件合计预算；
// 第一个文件截断后，后续 target skipped。
func TestReadMetricsAndGlobalBudget(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(a, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_read", map[string]any{
		"maxBytes": 8,
		"targets":  []any{map[string]any{"path": a}, map[string]any{"path": b}},
	})
	if !ok {
		t.Fatalf("read should stay batch-success: %v", out)
	}
	rs := out["results"].([]any)
	r0 := rs[0].(map[string]any)
	wantMD5, err := md5File(a)
	if err != nil {
		t.Fatal(err)
	}
	if r0["total"].(float64) != 3 || r0["file_md5"] != wantMD5 {
		t.Fatalf("全文指标错误：total=%v md5=%v want=%s", r0["total"], r0["file_md5"], wantMD5)
	}
	if r0["content"] != "one\ntwo" || r0["returned"].(float64) != 2 {
		t.Fatalf("预算截断内容错误：content=%q returned=%v", r0["content"], r0["returned"])
	}
	if r0["truncated"] != true || r0["truncated_reason"] != "maxBytes" || r0["next_start"].(float64) != 3 {
		t.Fatalf("截断标记错误：%v", r0)
	}
	r1 := rs[1].(map[string]any)
	if r1["status"] != statusSkipped {
		t.Fatalf("预算耗尽后应 skipped：%v", r1)
	}
	s := summaryOf(t, out)
	if s["ok"].(float64) != 1 || s["skipped"].(float64) != 1 {
		t.Fatalf("summary=%v", s)
	}
}

// TestReadEmptyInfoOnly：空文件 infoOnly 应成功返回 total=0，而不是旧的 EOF 失败。
func TestReadEmptyInfoOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "infoOnly": true}},
	})
	if !ok {
		t.Fatalf("empty infoOnly: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusOK || r["total"].(float64) != 0 {
		t.Fatalf("empty infoOnly 结果错误：%v", r)
	}
	if _, has := r["content"]; has {
		t.Fatalf("infoOnly 不应返回 content：%v", r)
	}
}

// TestReadGBKDecodesToUTF8：源文件 GBK，content 输出 UTF-8 文本，encoding 标记源编码。
func TestReadGBKDecodesToUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gbk.txt")
	// “中文内容测试\n” 的 GBK 字节。
	raw := []byte{0xD6, 0xD0, 0xCE, 0xC4, 0xC4, 0xDA, 0xC8, 0xDD, 0xB2, 0xE2, 0xCA, 0xD4, 0x0A}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("gbk read: %v", out)
	}
	r := firstResult(t, out)
	enc, _ := r["encoding"].(string)
	if r["content"] != "中文内容测试" || (enc != "gbk" && enc != "gb18030") || r["total"].(float64) != 1 {
		t.Fatalf("gbk 解码/编码标记错误：%v", r)
	}
}

// TestReadBinaryRejected：二进制文件不做内容返回。
func TestReadBinaryRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin.txt")
	if err := os.WriteFile(path, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path}},
	})
	if !ok {
		t.Fatalf("逐项失败不应把整次调用标为 IsError：%v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "BINARY_UNSUPPORTED" || r["binary"] != true {
		t.Fatalf("二进制应显式拒绝：%v", r)
	}
}

// TestReadExplicitEncodingStillRejectsBinary：显式 encoding 不能绕过二进制探测。
func TestReadExplicitEncodingStillRejectsBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bin.dat")
	if err := os.WriteFile(path, []byte{'a', 0, 'b', 0, 'c'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "encoding": "utf-8"}},
	})
	if !ok {
		t.Fatalf("逐项失败不应把整次调用标为 IsError：%v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "BINARY_UNSUPPORTED" || r["binary"] != true {
		t.Fatalf("显式 encoding 仍应拒绝二进制：%v", r)
	}
}

// TestReadInfoOnlyAfterBudgetExhausted：maxBytes 只限制 content；预算耗尽后 infoOnly
// 仍必须返回元信息（total/file_md5），只有需要正文的后续 target 才 skipped。
func TestReadInfoOnlyAfterBudgetExhausted(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	c := filepath.Join(dir, "c.txt")
	if err := os.WriteFile(a, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("meta\nonly\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c, []byte("content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_read", map[string]any{
		"maxBytes": 8,
		"targets": []any{
			map[string]any{"path": a},
			map[string]any{"path": b, "infoOnly": true},
			map[string]any{"path": c},
		},
	})
	if !ok {
		t.Fatalf("read should stay batch-success: %v", out)
	}
	rs := out["results"].([]any)
	r0 := rs[0].(map[string]any)
	r1 := rs[1].(map[string]any)
	r2 := rs[2].(map[string]any)
	if r0["status"] != statusOK || r0["truncated"] != true {
		t.Fatalf("第一个文件应按预算截断：%v", r0)
	}
	if r1["status"] != statusOK || r1["total"].(float64) != 2 {
		t.Fatalf("预算耗尽后 infoOnly 仍应成功并返回完整 total：%v", r1)
	}
	if _, has := r1["content"]; has {
		t.Fatalf("infoOnly 不应返回 content：%v", r1)
	}
	if r2["status"] != statusSkipped || r2["reason"] != "maxBytes exhausted" {
		t.Fatalf("需要正文的后续 target 应 skipped：%v", r2)
	}
	s := summaryOf(t, out)
	if s["ok"].(float64) != 2 || s["skipped"].(float64) != 1 {
		t.Fatalf("summary=%v", s)
	}
	if out["ok"] != true {
		t.Fatalf("无失败项时顶层 ok 应为 true：%v", out["ok"])
	}
}

// TestReadStartBeyondEOFIsFailed：无内容可返回的 START_BEYOND_EOF 归入 failed（code 仍保留）。
func TestReadStartBeyondEOFIsFailed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": path, "start": 99}},
	})
	if !ok {
		t.Fatalf("逐项失败不应把整次调用标为 IsError：%v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "START_BEYOND_EOF" || r["error"] == nil {
		t.Fatalf("start 超出 EOF 应为 failed：%v", r)
	}
}

// TestReadAllFailedEnvelopeOkFalse：逐项全失败 → 顶层 ok=false；IsError 仍 false（批量约定）。
func TestReadAllFailedEnvelopeOkFalse(t *testing.T) {
	out, batchOK := runTool(t, "file_read", map[string]any{
		"targets": []any{map[string]any{"path": filepath.Join(t.TempDir(), "missing.txt")}},
	})
	if !batchOK {
		t.Fatalf("逐项失败不应把整次调用标为 IsError：%v", out)
	}
	if out["ok"] != false {
		t.Fatalf("全部 target 失败时顶层 ok 应为 false：%v", out["ok"])
	}
	s := summaryOf(t, out)
	if s["failed"].(float64) != 1 {
		t.Fatalf("summary=%v", s)
	}
}

// TestWriteBOMOnlyForUTF8：BOM 仅对 UTF-8 输出生效，默认无 BOM。
func TestWriteBOMOnlyForUTF8(t *testing.T) {
	dir := t.TempDir()

	// 新文件默认：无 BOM。
	p1 := filepath.Join(dir, "new.txt")
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": p1, "content": "hi"}},
	})
	if !ok {
		t.Fatalf("write: %v", out)
	}
	if r := firstResult(t, out); r["bom"] != "none" {
		t.Fatalf("新文件默认应无 BOM：%v", r)
	}
	if b, _ := os.ReadFile(p1); string(b) != "hi" {
		t.Fatalf("新文件字节=% x", b)
	}

	// add → UTF-8 BOM。
	p2 := filepath.Join(dir, "add.txt")
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": p2, "content": "hi", "bom": "add"}},
	})
	if r := firstResult(t, out); r["bom"] != "utf-8" {
		t.Fatalf("bom=add 应返回 utf-8：%v", r)
	}
	if b, _ := os.ReadFile(p2); !hasBOM(b) || string(b[3:]) != "hi" {
		t.Fatalf("bom=add 字节=% x", b)
	}

	// 旧 UTF-8 + BOM，默认 → 去掉 BOM。
	p3 := filepath.Join(dir, "strip.txt")
	if err := os.WriteFile(p3, append([]byte{0xEF, 0xBB, 0xBF}, []byte("old")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": p3, "content": "new"}},
	})
	if r := firstResult(t, out); r["bom"] != "none" {
		t.Fatalf("默认应去掉旧 BOM：%v", r)
	}
	if b, _ := os.ReadFile(p3); string(b) != "new" {
		t.Fatalf("默认写后字节=% x", b)
	}

	// 旧 UTF-8 + BOM，keep → 保留 BOM。
	p4 := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(p4, append([]byte{0xEF, 0xBB, 0xBF}, []byte("old")...), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": p4, "content": "keep", "bom": "keep"}},
	})
	if r := firstResult(t, out); r["bom"] != "utf-8" {
		t.Fatalf("keep 应保留 BOM：%v", r)
	}
	if b, _ := os.ReadFile(p4); !hasBOM(b) || string(b[3:]) != "keep" {
		t.Fatalf("keep 字节=% x", b)
	}

	// 非 UTF-8 编码：bom 不生效，恒 none。
	p5 := filepath.Join(dir, "gbk.txt")
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": p5, "content": "中文", "encoding": "gbk", "bom": "add"}},
	})
	if r := firstResult(t, out); r["bom"] != "none" {
		t.Fatalf("非 UTF-8 应恒 none：%v", r)
	}
	if b, _ := os.ReadFile(p5); hasBOM(b) {
		t.Fatalf("非 UTF-8 不应有 BOM：% x", b)
	}

	// 非法 bom → 该项 failed。
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": filepath.Join(dir, "bad.txt"), "content": "x", "bom": "wat"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "bom must be") {
		t.Fatalf("非法 bom 应 failed：%v", r)
	}
}

// TestWriteAutoAmbiguousEncodingFails：auto 低置信度不猜 UTF-8，要求显式 encoding。
func TestWriteAutoAmbiguousEncodingFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ambiguous.txt")
	if err := os.WriteFile(path, []byte{0x81}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "x"}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError：%v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || !strings.Contains(r["error"].(string), "encoding ambiguous") {
		t.Fatalf("低置信度应 failed：%v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != string([]byte{0x81}) {
		t.Fatalf("失败时不应改写文件：% x", b)
	}
}

// TestWritePerTargetIsolation：一个目标失败不影响同批其他目标写入。
func TestWritePerTargetIsolation(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	bad := filepath.Join(dir, "bad.txt")
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{
			map[string]any{"path": bad, "content": 123}, // content 非字符串 → failed
			map[string]any{"path": good, "content": "ok"},
		},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError：%v", out)
	}
	rs := out["results"].([]any)
	if rs[0].(map[string]any)["status"] != statusFailed {
		t.Fatalf("第一个目标应 failed：%v", rs[0])
	}
	if rs[1].(map[string]any)["status"] != statusOK {
		t.Fatalf("第二个目标应 ok：%v", rs[1])
	}
	if b, _ := os.ReadFile(good); string(b) != "ok" {
		t.Fatalf("第二个目标应落盘：%q", b)
	}
}

// TestWriteDuplicateTargetMessage：同路径不再 failed；前面的重复项 skipped，
// 文案为 superseded_by_later_target（last-wins）。
func TestWriteDuplicateTargetMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dup.txt")
	out, _ := runTool(t, "file_write", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "content": "a"},
			map[string]any{"path": path, "content": "b"},
		},
	})
	rs := out["results"].([]any)
	first := rs[0].(map[string]any)
	if first["status"] != statusSkipped || first["reason"] != "superseded_by_later_target" ||
		first["error"] != "superseded_by_later_target" {
		t.Fatalf("前面的重复项应 skipped：%v", first)
	}
	if rs[1].(map[string]any)["status"] != statusOK {
		t.Fatalf("最后一个 target 应 ok：%v", rs[1])
	}
}

// TestWriteDuplicateLastWins：同路径 A/B/C → 只执行 C；前两项 skipped，结果保持入参顺序。
func TestWriteDuplicateLastWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lastwins.txt")
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "content": "a"},
			map[string]any{"path": path, "content": "b"},
			map[string]any{"path": path, "content": "c"},
		},
	})
	if !ok {
		t.Fatalf("write last-wins should succeed: %v", out)
	}
	rs := out["results"].([]any)
	if len(rs) != 3 {
		t.Fatalf("results 长度=%d want 3", len(rs))
	}
	for i := 0; i < 2; i++ {
		r := rs[i].(map[string]any)
		if r["status"] != statusSkipped || r["reason"] != "superseded_by_later_target" {
			t.Fatalf("results[%d] 应 skipped：%v", i, r)
		}
	}
	if last := rs[2].(map[string]any); last["status"] != statusOK {
		t.Fatalf("最后一个 target 应 ok：%v", last)
	}
	if b, _ := os.ReadFile(path); string(b) != "c" {
		t.Fatalf("content=%q want c", b)
	}
	s := summaryOf(t, out)
	if s["total"].(float64) != 3 || s["ok"].(float64) != 1 || s["skipped"].(float64) != 2 {
		t.Fatalf("summary=%v", s)
	}
}

// TestWriteStatAndParentGuards：目录 / createDirs=false 的父目录缺失均显式 failed。
func TestWriteStatAndParentGuards(t *testing.T) {
	dir := t.TempDir()
	out, _ := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": dir, "content": "x"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || r["error"] != "is a directory" {
		t.Fatalf("目录目标应 failed：%v", r)
	}

	missing := filepath.Join(dir, "no", "such", "a.txt")
	out, _ = runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": missing, "content": "x", "createDirs": false}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "parent dir not found") {
		t.Fatalf("父目录缺失应 failed：%v", r)
	}
}

// TestWriteInPlaceTruncates：旧文件比新内容长时，就地写必须截断旧尾部。
func TestWriteInPlaceTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncate.txt")
	if err := os.WriteFile(path, []byte("a much longer old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_write", map[string]any{
		"targets": []any{map[string]any{"path": path, "content": "short"}},
	})
	if !ok {
		t.Fatalf("write: %v", out)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "short" {
		t.Fatalf("content=%q (old tail should be truncated)", b)
	}
}

// TestReplaceReturnsOnlyStatusAndCount：成功不返回 diff/md5，只回 status + replacements。
func TestReplaceReturnsOnlyStatusAndCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{"path": path, "find": "foo", "replace": "baz"}},
	})
	if !ok {
		t.Fatalf("replace: %v", out)
	}
	r := firstResult(t, out)
	if r["replacements"].(float64) != 2 || r["status"] != statusOK {
		t.Fatalf("result=%v", r)
	}
	for _, banned := range []string{"diff", "md5Before", "md5After"} {
		if _, has := r[banned]; has {
			t.Fatalf("成功项不应返回 %s：%v", banned, r)
		}
	}
}

// TestPatchReturnsOnlyStatusAndCount：成功只回 status + hunks，不返回 md5。
func TestPatchReturnsOnlyStatusAndCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"

	out, ok := runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": path, "patch": patch}},
	})
	if !ok {
		t.Fatalf("patch: %v", out)
	}
	r := firstResult(t, out)
	if r["hunks"].(float64) != 1 || r["status"] != statusOK {
		t.Fatalf("result=%v", r)
	}
	for _, banned := range []string{"diff", "md5Before", "md5After"} {
		if _, has := r[banned]; has {
			t.Fatalf("成功项不应返回 %s：%v", banned, r)
		}
	}
}

// TestReplacePatchDuplicateTarget：同批重复路径不再 failed；replace/patch 同路径
// 合并为一次打开/读/写，组内按输入顺序应用，两项都 ok。
func TestReplacePatchDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	replacePath := filepath.Join(dir, "d-replace.txt")
	if err := os.WriteFile(replacePath, []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{
			map[string]any{"path": replacePath, "find": "foo", "replace": "bar"},
			map[string]any{"path": replacePath, "find": "bar", "replace": "baz"},
		},
	})
	rs := out["results"].([]any)
	for i := range rs {
		if r := rs[i].(map[string]any); r["status"] != statusOK {
			t.Fatalf("replace results[%d] 应 ok：%v", i, r)
		}
	}
	if b, _ := os.ReadFile(replacePath); string(b) != "baz" {
		t.Fatalf("replace 最终内容=%q want baz", b)
	}

	patchPath := filepath.Join(dir, "d-patch.txt")
	if err := os.WriteFile(patchPath, []byte("foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_patch", map[string]any{
		"targets": []any{
			map[string]any{"path": patchPath, "patch": "--- a\n+++ b\n@@ -1 +1 @@\n-foo\n+bar\n"},
			map[string]any{"path": patchPath, "patch": "--- a\n+++ b\n@@ -1 +1 @@\n-bar\n+baz\n"},
		},
	})
	rs = out["results"].([]any)
	for i := range rs {
		if r := rs[i].(map[string]any); r["status"] != statusOK {
			t.Fatalf("patch results[%d] 应 ok：%v", i, r)
		}
	}
	if b, _ := os.ReadFile(patchPath); string(b) != "baz\n" {
		t.Fatalf("patch 最终内容=%q want baz\\n", b)
	}
}

// TestReplaceSamePathGrouped：同路径两个 replace → 合并一次写，两处都生效，
// 每项各自返回 replacements。
func TestReplaceSamePathGrouped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grouped.txt")
	if err := os.WriteFile(path, []byte("foo and keep bar"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_replace", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "find": "foo", "replace": "alpha"},
			map[string]any{"path": path, "find": "bar", "replace": "beta"},
		},
	})
	if !ok {
		t.Fatalf("replace group: %v", out)
	}
	rs := out["results"].([]any)
	if len(rs) != 2 {
		t.Fatalf("results 长度=%d want 2", len(rs))
	}
	for i := range rs {
		r := rs[i].(map[string]any)
		if r["status"] != statusOK || r["replacements"].(float64) != 1 {
			t.Fatalf("results[%d] 应 ok+replacements=1：%v", i, r)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "alpha and keep beta" {
		t.Fatalf("content=%q want alpha and keep beta", b)
	}
}

// TestPatchSamePathSequential：同路径两个 patch 按输入顺序依次应用，两处都生效，
// 每项返回自己的 hunks。
func TestPatchSamePathSequential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seq-patch.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_patch", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "patch": "--- a\n+++ b\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"},
			map[string]any{"path": path, "patch": "--- a\n+++ b\n@@ -1,3 +1,3 @@\n one\n TWO\n-three\n+THREE\n"},
		},
	})
	if !ok {
		t.Fatalf("patch group: %v", out)
	}
	rs := out["results"].([]any)
	if len(rs) != 2 {
		t.Fatalf("results 长度=%d want 2", len(rs))
	}
	for i := range rs {
		r := rs[i].(map[string]any)
		if r["status"] != statusOK || r["hunks"].(float64) != 1 {
			t.Fatalf("results[%d] 应 ok+hunks=1：%v", i, r)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "one\nTWO\nTHREE\n" {
		t.Fatalf("content=%q want one\\nTWO\\nTHREE\\n", b)
	}
}

// TestReplaceGroupAtomicFailure：同路径两项，第二项 find 不存在 → 文件不变；
// 第一项 skipped（not_applied），第二项 failed。
func TestReplaceGroupAtomicFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.txt")
	if err := os.WriteFile(path, []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "find": "foo", "replace": "bar"},
			map[string]any{"path": path, "find": "missing", "replace": "x"},
		},
	})
	rs := out["results"].([]any)
	if r := rs[0].(map[string]any); r["status"] != statusSkipped || r["reason"] != "not_applied" {
		t.Fatalf("results[0] 应 skipped not_applied：%v", r)
	}
	if r := rs[1].(map[string]any); r["status"] != statusFailed || r["error"] != "search not found" {
		t.Fatalf("results[1] 应 failed search not found：%v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != "foo" {
		t.Fatalf("文件不应被改写：%q", b)
	}
}

// TestReplaceGroupExpectMd5：同组两个不同的非空 expectMd5 → 两项都 failed，
// code=conflicting_expectMd5，文件不变。
func TestReplaceGroupExpectMd5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflict-md5.txt")
	if err := os.WriteFile(path, []byte("foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{
			map[string]any{"path": path, "find": "foo", "replace": "bar", "expectMd5": strings.Repeat("a", 32)},
			map[string]any{"path": path, "find": "bar", "replace": "baz", "expectMd5": strings.Repeat("b", 32)},
		},
	})
	rs := out["results"].([]any)
	for i := range rs {
		r := rs[i].(map[string]any)
		if r["status"] != statusFailed || r["code"] != "conflicting_expectMd5" {
			t.Fatalf("results[%d] 应 failed conflicting_expectMd5：%v", i, r)
		}
	}
	if b, _ := os.ReadFile(path); string(b) != "foo" {
		t.Fatalf("文件不应被改写：%q", b)
	}
}

// TestResultsInputOrder：混合成功/失败/skipped 时，results 顺序仍与 targets 一一对应。
func TestResultsInputOrder(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "order-a.txt")
	pathB := filepath.Join(dir, "order-b.txt")
	pathC := filepath.Join(dir, "order-c.txt")
	out, _ := runTool(t, "file_write", map[string]any{
		"targets": []any{
			map[string]any{"path": pathA, "content": "a1"}, // 被后面的同路径 target 覆盖 → skipped
			map[string]any{"path": pathB, "content": 123},  // content 非字符串 → failed
			map[string]any{"path": pathC, "content": "c"},  // ok
			map[string]any{"path": pathA, "content": "a2"}, // winner → ok
		},
	})
	rs := out["results"].([]any)
	if len(rs) != 4 {
		t.Fatalf("results 长度=%d want 4", len(rs))
	}
	wantPaths := []string{pathA, pathB, pathC, pathA}
	wantStatus := []string{statusSkipped, statusFailed, statusOK, statusOK}
	for i := range rs {
		r := rs[i].(map[string]any)
		if r["path"] != wantPaths[i] || r["status"] != wantStatus[i] {
			t.Fatalf("results[%d]=%v want path=%s status=%s", i, r, wantPaths[i], wantStatus[i])
		}
	}
	if b, _ := os.ReadFile(pathA); string(b) != "a2" {
		t.Fatalf("pathA content=%q want a2", b)
	}
}

// TestReplacePatchBinaryRejected：二进制源文件不做文本替换/打补丁。
func TestReplacePatchBinaryRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(path, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{"path": path, "find": "a", "replace": "z"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "binary file") {
		t.Fatalf("replace 二进制应 failed：%v", r)
	}
	out, _ = runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": path, "patch": "--- a\n+++ b\n@@ -1 +1 @@\n-a\n+z\n"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "binary file") {
		t.Fatalf("patch 二进制应 failed：%v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != string([]byte{'a', 0, 'b'}) {
		t.Fatalf("二进制文件不应被改写：% x", b)
	}
}

// TestReplacePatchInvalidBytesNoWrite：无法识别的非 UTF-8 字节不应被静默重写；
// replace 未命中 / patch 上下文不匹配时保持原字节。
func TestReplacePatchInvalidBytesNoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "invalid.txt")
	original := []byte{0x81}
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{"path": path, "find": "a", "replace": "b"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || r["error"] != "search not found" {
		t.Fatalf("replace 未命中应 failed：%v", r)
	}
	out, _ = runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": path, "patch": "--- a\n+++ b\n@@ -1 +1 @@\n-a\n+b\n"}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed {
		t.Fatalf("patch 上下文不匹配应 failed：%v", r)
	}
	if b, _ := os.ReadFile(path); string(b) != string(original) {
		t.Fatalf("失败时不应改写文件：% x", b)
	}
}

// TestReplaceSkipHasReasonAndError：skipped 同时给 reason/error（兼容契约示例）。
func TestReplaceSkipHasReasonAndError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_replace", map[string]any{
		"targets": []any{map[string]any{
			"path": path, "find": "a", "replace": "z", "expectMd5": strings.Repeat("0", 32),
		}},
	})
	r := firstResult(t, out)
	if r["reason"] != "md5 mismatch" || r["error"] != "md5 mismatch" || r["md5"] == nil {
		t.Fatalf("skipped 字段不符：%v", r)
	}
}

// TestReplacePreservesSourceEncoding：replace 的“读→改→写”必须使用同一个源编码，
// 并保持源文件 BOM 状态（UTF-8 BOM 不能变成双 BOM）。
func TestReplacePreservesSourceEncoding(t *testing.T) {
	cases := []struct {
		name, enc, old, find, repl, want string
		bom                              bool
	}{
		{"gbk", "gbk", "中文内容", "中文", "测试", "测试内容", false},
		{"utf8-bom", "utf-8", "foo bar foo", "foo", "baz", "baz bar baz", true},
		{"utf16le", "utf-16le", "a\nb\nc\n", "b", "B", "a\nB\nc\n", true},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), c.name+".txt")
		data := encodeForTest(t, c.enc, c.old, c.bom)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		out, ok := runTool(t, "file_replace", map[string]any{
			"targets": []any{map[string]any{"path": path, "find": c.find, "replace": c.repl}},
		})
		if !ok {
			t.Fatalf("%s replace: %v", c.name, out)
		}
		gotBytes, _ := os.ReadFile(path)
		if hasBOM(gotBytes) != c.bom {
			t.Fatalf("%s BOM 状态错误：% x", c.name, gotBytes)
		}
		got := decodeForTest(t, c.enc, gotBytes)
		if got != c.want {
			t.Fatalf("%s content=%q want %q (bytes=% x)", c.name, got, c.want, gotBytes)
		}
	}
}

// TestPatchPreservesSourceEncoding：patch 同样使用源编码回写并保持 BOM。
func TestPatchPreservesSourceEncoding(t *testing.T) {
	cases := []struct {
		name, enc, old, patch, want string
		bom                         bool
	}{
		{"gbk", "gbk", "中文内容\n",
			"--- a\n+++ b\n@@ -1 +1 @@\n-中文内容\n+测试内容\n", "测试内容\n", false},
		{"utf8-bom", "utf-8", "a\nb\nc\n",
			"--- a\n+++ b\n@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n", "a\nB\nc\n", true},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), c.name+".txt")
		data := encodeForTest(t, c.enc, c.old, c.bom)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		out, ok := runTool(t, "file_patch", map[string]any{
			"targets": []any{map[string]any{"path": path, "patch": c.patch}},
		})
		if !ok {
			t.Fatalf("%s patch: %v", c.name, out)
		}
		gotBytes, _ := os.ReadFile(path)
		if hasBOM(gotBytes) != c.bom {
			t.Fatalf("%s BOM 状态错误：% x", c.name, gotBytes)
		}
		got := decodeForTest(t, c.enc, gotBytes)
		if got != c.want {
			t.Fatalf("%s content=%q want %q (bytes=% x)", c.name, got, c.want, gotBytes)
		}
	}
}

// encodeForTest 按 test 的编码名造文件（UTF-8 BOM 由 encodingx.Encode 不处理，手动补）。
func encodeForTest(t *testing.T, enc, s string, bom bool) []byte {
	t.Helper()
	if enc == "utf-8" || enc == "utf8" {
		b := []byte(s)
		if bom {
			return append([]byte{0xEF, 0xBB, 0xBF}, b...)
		}
		return b
	}
	b, err := encodingx.Encode(enc, s, bom)
	if err != nil {
		t.Fatalf("encode %s: %v", enc, err)
	}
	return b
}

// decodeForTest 按 test 的编码名解码（UTF-8 先剥 BOM，UTF-16 由 Decode 处理）。
func decodeForTest(t *testing.T, enc string, b []byte) string {
	t.Helper()
	if enc == "utf-8" || enc == "utf8" {
		return string(stripUTF8BOM(b))
	}
	s, err := encodingx.Decode(enc, b)
	if err != nil {
		t.Fatalf("decode %s: %v", enc, err)
	}
	return s
}

// TestDiffSameTextDifferentEncoding：文本一致但编码/BOM 不同时，identical=true，
// 同时返回两侧编码与 BOM（不返回 byte_identical/encoding_same 聚合字段）。
func TestDiffSameTextDifferentEncoding(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left_gbk.txt")
	right := filepath.Join(dir, "right_utf8.txt")
	gbk, err := encodingx.Encode("gbk", "中文内容\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, gbk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte("中文内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_diff", map[string]any{
		"targets": []any{map[string]any{"left": left, "right": right}},
	})
	if !ok {
		t.Fatalf("diff: %v", out)
	}
	r := firstResult(t, out)
	if r["identical"] != true || r["diff"] != "" {
		t.Fatalf("文本一致应 identical=true：%v", r)
	}
	if r["left_encoding"] != "gbk" || r["right_encoding"] != "utf-8" {
		t.Fatalf("编码字段=%v / %v", r["left_encoding"], r["right_encoding"])
	}
	if r["left_bom"] != false || r["right_bom"] != false {
		t.Fatalf("BOM 字段=%v / %v", r["left_bom"], r["right_bom"])
	}
	for _, banned := range []string{"byte_identical", "encoding_same"} {
		if _, has := r[banned]; has {
			t.Fatalf("不应返回 %s：%v", banned, r)
		}
	}

	// BOM 差异：identical 仍为 true，left_bom/right_bom 不同。
	leftBOM := filepath.Join(dir, "left_bom.txt")
	rightNoBOM := filepath.Join(dir, "right_nobom.txt")
	if err := os.WriteFile(leftBOM, []byte{0xEF, 0xBB, 0xBF, 'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rightNoBOM, []byte{'x'}, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_diff", map[string]any{
		"targets": []any{map[string]any{"left": leftBOM, "right": rightNoBOM}},
	})
	r = firstResult(t, out)
	if r["identical"] != true || r["left_bom"] != true || r["right_bom"] != false {
		t.Fatalf("BOM 差异处理错误：%v", r)
	}
}

// TestDiffOutputCap128K：diff 超过 128K 时截断，保留完整 added/removed 统计并标识未返回。
func TestDiffOutputCap128K(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left.txt")
	right := filepath.Join(dir, "right.txt")
	var a, b strings.Builder
	for i := 0; i < 10000; i++ {
		fmt.Fprintf(&a, "old_%05d\n", i)
		fmt.Fprintf(&b, "new_%05d\n", i)
	}
	if err := os.WriteFile(left, []byte(a.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(right, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_diff", map[string]any{
		"targets": []any{map[string]any{"left": left, "right": right}},
	})
	if !ok {
		t.Fatalf("diff: %v", out)
	}
	r := firstResult(t, out)
	diff := r["diff"].(string)
	if r["diff_truncated"] != true || r["diff_omitted"] != true {
		t.Fatalf("应标识截断：%v", r)
	}
	if len(diff) > maxDiffReturnBytes {
		t.Fatalf("返回 diff=%d 超过上限 %d", len(diff), maxDiffReturnBytes)
	}
	if r["diff_returned_bytes"].(float64) != float64(len(diff)) {
		t.Fatalf("diff_returned_bytes=%v want %d", r["diff_returned_bytes"], len(diff))
	}
	if r["diff_bytes"].(float64) <= float64(maxDiffReturnBytes) {
		t.Fatalf("diff_bytes=%v 应大于上限", r["diff_bytes"])
	}
	if r["added"].(float64) != 10000 || r["removed"].(float64) != 10000 {
		t.Fatalf("完整统计应保留：added=%v removed=%v", r["added"], r["removed"])
	}
}

// TestDiffBinaryRejected：任一侧为二进制（含 NUL）时该项 failed + code=BINARY_UNSUPPORTED，
// 不做文本比较；逐项失败不把整次调用标为 IsError。
func TestDiffBinaryRejected(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "text.txt")
	bin := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(text, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		left  string
		right string
		side  string
	}{
		{"right binary", text, bin, "right"},
		{"left binary", bin, text, "left"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := runTool(t, "file_diff", map[string]any{
				"targets": []any{map[string]any{"left": tc.left, "right": tc.right}},
			})
			if !ok {
				t.Fatalf("逐项失败不应把整次调用标为 IsError：%v", out)
			}
			r := firstResult(t, out)
			if r["status"] != statusFailed || r["code"] != "BINARY_UNSUPPORTED" || r["binary"] != true {
				t.Fatalf("二进制应显式拒绝：%v", r)
			}
			if msg, _ := r["error"].(string); !strings.HasPrefix(msg, tc.side+":") {
				t.Fatalf("错误应指出 %s 侧：%v", tc.side, r)
			}
			for _, banned := range []string{"diff", "identical", "added", "removed",
				"left_encoding", "right_encoding"} {
				if _, has := r[banned]; has {
					t.Fatalf("二进制不应产出 %s：%v", banned, r)
				}
			}
			if s := summaryOf(t, out); s["failed"].(float64) != 1 || s["ok"].(float64) != 0 {
				t.Fatalf("summary 应为 1 failed：%v", s)
			}
			if out["ok"] != false {
				t.Fatalf("顶层 ok 应为 false：%v", out)
			}
		})
	}
}

// 确保 Result 类型断言可用（cli.Err 路径）。
var _ = cli.Err

// TestFindDirModeReturnsDirectories：dir 模式（depth=1）必须把目录作为 is_dir 项返回，且只列直接子项。
func TestFindDirModeReturnsDirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "child.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "depth": 1}},
	})
	if !ok {
		t.Fatalf("find dir mode failed: %v", out)
	}
	r := firstResult(t, out)
	items, _ := r["items"].([]any)
	gotDir, gotChild := false, false
	for _, it := range items {
		m := it.(map[string]any)
		switch m["rel"] {
		case "sub":
			if m["is_dir"] != true {
				t.Fatalf("目录 sub 的 is_dir=%v，期望 true", m["is_dir"])
			}
			gotDir = true
		case "sub/child.txt", "sub/nested":
			gotChild = true
		}
	}
	if !gotDir {
		t.Fatalf("dir 模式未返回目录项: %v", items)
	}
	if gotChild {
		t.Fatalf("depth=1 不应递归子目录内容: %v", items)
	}
	if r["matched_total"].(float64) != 2 {
		t.Fatalf("matched_total=%v，期望 2（top.txt + sub）", r["matched_total"])
	}

	// tree 输出包含真实目录节点（"sub/"）与子文件。
	out, _ = runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "output": "tree"}},
	})
	tree, _ := firstResult(t, out)["text"].(string)
	if !strings.Contains(tree, "sub/\n") || !strings.Contains(tree, "child.txt") {
		t.Fatalf("tree 缺目录分支:\n%s", tree)
	}

	// grep 模式只返回命中文件，不掺入目录。
	out, _ = runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "grep": "x"}},
	})
	for _, it := range firstResult(t, out)["items"].([]any) {
		if it.(map[string]any)["is_dir"] == true {
			t.Fatalf("grep 模式不应返回目录项: %v", it)
		}
	}
}

// TestFindRootMustBeRealDirectory：root 为普通文件 / symlink 时必须明确失败。
func TestFindRootMustBeRealDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": file}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "不是目录") {
		t.Fatalf("root 为文件应 failed: %v", r)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("当前环境无法创建 symlink: %v", err)
	}
	out, _ = runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": link}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "symlink") {
		t.Fatalf("root 为 symlink 应 failed: %v", r)
	}
}

// TestDiffPatchRoundTrip：file_diff 的产物必须能被 file_patch 直接消费并还原目标内容。
func TestDiffPatchRoundTrip(t *testing.T) {
	dir := t.TempDir()
	left := filepath.Join(dir, "left.txt")
	right := filepath.Join(dir, "right.txt")
	work := filepath.Join(dir, "work.txt")
	leftText := "one\ntwo\nthree\nfour\nfive\nsix\n"
	rightText := "one\nTWO\nthree\nfour\nfive\nSIX\n"
	for _, f := range []struct{ path, content string }{{left, leftText}, {right, rightText}, {work, leftText}} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, ok := runTool(t, "file_diff", map[string]any{
		"targets": []any{map[string]any{"left": left, "right": right, "context": 1}},
	})
	if !ok {
		t.Fatalf("diff failed: %v", out)
	}
	diff, _ := firstResult(t, out)["diff"].(string)
	if !strings.Contains(diff, "@@ -") {
		t.Fatalf("diff 缺 @@ hunk 头:\n%s", diff)
	}

	out, ok = runTool(t, "file_patch", map[string]any{
		"targets": []any{map[string]any{"path": work, "patch": diff}},
	})
	if !ok {
		t.Fatalf("patch failed: %v", out)
	}
	if r := firstResult(t, out); r["status"] != statusOK {
		t.Fatalf("patch 未成功: %v", r)
	}
	got, _ := os.ReadFile(work)
	if string(got) != rightText {
		t.Fatalf("diff→patch 往返不一致:\n got %q\nwant %q", got, rightText)
	}
}

// itemsOf 取结果 items 的 rel 列表。
func itemsOf(t *testing.T, r map[string]any) []string {
	t.Helper()
	items, _ := r["items"].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any)["rel"].(string))
	}
	return out
}

// TestFindDirModeDefaultDepth：output=dir 缺省 depth=1（只列直接子项），显式 depth 始终优先。
func TestFindDirModeDefaultDepth(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"top.txt", "sub/child.txt", "sub/nested/deep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, ok := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "output": "dir"}},
	})
	if !ok {
		t.Fatalf("dir 模式失败: %v", out)
	}
	r := firstResult(t, out)
	got := itemsOf(t, r)
	if len(got) != 2 || got[0] != "sub" || got[1] != "top.txt" {
		t.Fatalf("缺省 depth=1 应只含 [sub top.txt]: %v", got)
	}
	if r["matched_total"].(float64) != 2 {
		t.Fatalf("matched_total=%v，期望 2", r["matched_total"])
	}

	// 显式 depth=0（不限）优先于 dir 的缺省 1。
	out, _ = runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "output": "dir", "depth": 0}},
	})
	got = itemsOf(t, firstResult(t, out))
	joined := strings.Join(got, ",")
	for _, want := range []string{"sub/child.txt", "sub/nested", "sub/nested/deep.txt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("显式 depth=0 应递归包含 %s: %v", want, got)
		}
	}
}

// TestFindTreeHasDirectoryNode：tree 输出必须渲染真实目录节点（含空目录）。
func TestFindTreeHasDirectoryNode(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "output": "tree"}},
	})
	tree, _ := firstResult(t, out)["text"].(string)
	if !strings.Contains(tree, "empty/") || !strings.Contains(tree, "a.txt") {
		t.Fatalf("tree 缺目录/文件节点:\n%s", tree)
	}
}

// TestFindDeterministicTruncation：limit 小于总数时，返回字典序前 N 项且重复调用稳定。
func TestFindDeterministicTruncation(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zzz.txt", "b.txt", "mid/x.txt", "mid/y.txt", "a.txt"} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"a.txt", "b.txt"}
	for i := 0; i < 5; i++ {
		out, _ := runTool(t, "file_find", map[string]any{
			"targets": []any{map[string]any{"root": dir, "glob": "*.txt,**/*.txt", "limit": 2}},
		})
		r := firstResult(t, out)
		got := itemsOf(t, r)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("第 %d 次: items=%v，期望字典序前 2 项 %v", i, got, want)
		}
		if r["truncated"] != true || r["matched_total"].(float64) != 5 {
			t.Fatalf("truncated=%v matched_total=%v", r["truncated"], r["matched_total"])
		}
	}
}

// TestFindGrepStreamsLargeFile：大文件命中正确（命中行远在头部探测窗口之后），并覆盖 legacy 逐行解码。
func TestFindGrepStreamsLargeFile(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	const lines = 30000
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "filler line %06d ....................................................\n", i)
	}
	b.WriteString("NEEDLE at the tail\n")
	path := filepath.Join(dir, "big.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() <= int64(encodingProbeBytes) {
		t.Fatalf("测试文件不够大: %d", st.Size())
	}

	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "grep": "NEEDLE"}},
	})
	items, _ := firstResult(t, out)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d，期望 1: %v", len(items), items)
	}
	hits, _ := items[0].(map[string]any)["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits=%d，期望 1", len(hits))
	}
	h := hits[0].(map[string]any)
	if int(h["line"].(float64)) != lines+1 || !strings.Contains(h["text"].(string), "NEEDLE") {
		t.Fatalf("命中位置错误: %v", h)
	}

	// legacy（GBK）逐行解码：多字节字符 + 命中行。
	gbkDir := t.TempDir()
	gbkText := "第一行普通内容\n第二行包含命中词\n第三行普通内容\n"
	gbk, err := encodeByName(gbkText, "gbk")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gbkDir, "gbk.txt"), gbk, 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": gbkDir, "grep": "命中"}},
	})
	items, _ = firstResult(t, out)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("GBK grep items=%d: %v", len(items), items)
	}
	hits, _ = items[0].(map[string]any)["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("GBK hits=%d，期望 1", len(hits))
	}
	h = hits[0].(map[string]any)
	if int(h["line"].(float64)) != 2 || !strings.Contains(h["text"].(string), "命中词") {
		t.Fatalf("GBK 命中解码错误: %v", h)
	}
}

// TestFindGrepUTF8BOM：UTF-8 BOM 不应残留到首行命中文本，也不影响首行匹配。
func TestFindGrepUTF8BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bom.txt")
	if err := os.WriteFile(path, []byte("\xEF\xBB\xBF命中词尾部\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "grep": "命中词"}},
	})
	items, _ := firstResult(t, out)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d，期望 1: %v", len(items), items)
	}
	hits, _ := items[0].(map[string]any)["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits=%d，期望 1", len(hits))
	}
	h := hits[0].(map[string]any)
	if int(h["line"].(float64)) != 1 || h["text"].(string) != "命中词尾部" {
		t.Fatalf("BOM 首行命中错误: %v", h)
	}
}

// TestFindMaxTotalHits：全局命中预算达到上限后停止追加，hits_truncated=true 且 truncated=true。
func TestFindMaxTotalHits(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("hit\nhit\nhit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, ok := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "grep": "hit", "maxHitsPerFile": 10, "maxTotalHits": 2}},
	})
	if !ok {
		t.Fatalf("find failed: %v", out)
	}
	r := firstResult(t, out)
	if r["hits_truncated"] != true || r["truncated"] != true {
		t.Fatalf("hits_truncated=%v truncated=%v，期望均为 true", r["hits_truncated"], r["truncated"])
	}
	total := 0
	for _, it := range r["items"].([]any) {
		hits, _ := it.(map[string]any)["hits"].([]any)
		total += len(hits)
	}
	if total != 2 {
		t.Fatalf("命中总数=%d，期望被预算截到 2", total)
	}
	if r["matched_total"].(float64) > 3 {
		t.Fatalf("matched_total=%v 超出预算截点", r["matched_total"])
	}
}

// TestFindDepthNegativeFails：depth<0 → 该项 failed。
func TestFindDepthNegativeFails(t *testing.T) {
	dir := t.TempDir()
	out, ok := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "depth": -1}},
	})
	if !ok {
		t.Fatalf("逐项失败不应导致整批 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || !strings.Contains(r["error"].(string), "depth must be >= 0") {
		t.Fatalf("depth<0 应 failed: %v", r)
	}
}

// TestFindDateBoundaryLocalTimezone：from/to 按本地时区解释，包含当天本地凌晨的 mtime。
func TestFindDateBoundaryLocalTimezone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "today.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mtime := time.Date(now.Year(), now.Month(), now.Day(), 0, 30, 0, 0, time.Local)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	date := mtime.Format("2006-01-02")
	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "from": date, "to": date}},
	})
	r := firstResult(t, out)
	if r["matched_total"].(float64) != 1 {
		t.Fatalf("本地日期边界应包含当天文件（mtime=%s, date=%s）: %v", mtime, date, r)
	}
}

// TestFindGrepUTF8HeadBoundary：UTF-8 多字节字符跨越 64KiB 头部边界时不得误判为 legacy。
func TestFindGrepUTF8HeadBoundary(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	// "测" 为 3 字节；21845 个 = 65535 字节，使下一个 3 字节汉字跨 encodingProbeBytes 边界。
	b.WriteString(strings.Repeat("测", (encodingProbeBytes-1)/3))
	b.WriteString("命中词A\n")
	b.WriteString(strings.Repeat("filler ", 1000))
	b.WriteString("\n命中词B\n")
	path := filepath.Join(dir, "utf8.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() <= int64(encodingProbeBytes) {
		t.Fatalf("测试文件不够大: %d", st.Size())
	}

	out, _ := runTool(t, "file_find", map[string]any{
		"targets": []any{map[string]any{"root": dir, "grep": "命中词B"}},
	})
	items, _ := firstResult(t, out)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items=%d，期望 1: %v", len(items), items)
	}
	hits, _ := items[0].(map[string]any)["hits"].([]any)
	if len(hits) != 1 {
		t.Fatalf("hits=%d，期望 1", len(hits))
	}
	h := hits[0].(map[string]any)
	if int(h["line"].(float64)) != 3 || !strings.Contains(h["text"].(string), "命中词B") {
		t.Fatalf("UTF-8 边界后命中错误: %v", h)
	}
}

// TestCopyStreamsLargeFile：>8MB 文件流式复制，内容 md5 一致，bytes 为实际写入字节。
func TestCopyStreamsLargeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "large.bin")
	chunk := strings.Repeat("0123456789abcdef", 4096) // 64 KiB
	var b strings.Builder
	for i := 0; i < 160; i++ { // ~10 MiB
		b.WriteString(chunk)
	}
	if err := os.WriteFile(src, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= 8<<20 {
		t.Fatalf("测试文件不够大: %d", st.Size())
	}
	wantMD5, err := md5File(src)
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "large-copy.bin")
	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("copy: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("status=%v", r["status"])
	}
	if got := r["bytes"].(float64); got != float64(st.Size()) {
		t.Fatalf("bytes=%v want %d", r["bytes"], st.Size())
	}
	gotMD5, err := md5File(dst)
	if err != nil {
		t.Fatal(err)
	}
	if gotMD5 != wantMD5 {
		t.Fatalf("md5 mismatch: got %s want %s", gotMD5, wantMD5)
	}
}

// TestCopyDirectoryRollbackWhenTargetNew：新建目标树中途复制失败 → 整体回滚，目标不存在。
func TestCopyDirectoryRollbackWhenTargetNew(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dstdir")

	old := copyFileAtomicFn
	defer func() { copyFileAtomicFn = old }()
	copyFileAtomicFn = func(string, string, os.FileInfo, bool) (int64, error) {
		return 0, errors.New("injected copy failure")
	}

	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || !strings.Contains(r["error"].(string), "injected copy failure") {
		t.Fatalf("want failed with injected error: %v", r)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("新建目标树应被回滚，stat err=%v", err)
	}
}

// TestCopyDirectoryPartialWhenTargetExists：目标已存在时中途失败 → 保留目标，partial+copiedFiles。
func TestCopyDirectoryPartialWhenTargetExists(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dstdir")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := copyFileAtomicFn
	defer func() { copyFileAtomicFn = old }()
	calls := 0
	copyFileAtomicFn = func(s, d string, info os.FileInfo, overwrite bool) (int64, error) {
		calls++
		if calls == 2 {
			return 0, errors.New("injected mid-copy failure")
		}
		return old(s, d, info, overwrite)
	}

	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["partial"] != true {
		t.Fatalf("want failed + partial=true: %v", r)
	}
	if got := r["copiedFiles"].(float64); got < 1 {
		t.Fatalf("copiedFiles=%v want >=1", r["copiedFiles"])
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("已存在的目标不应被删除: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "keep" {
		t.Fatalf("目标已有文件不应被改动: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dst, "a.txt")); err != nil {
		t.Fatalf("失败前已复制的文件应保留: %v", err)
	}
}

// TestCopyPreservesFileModeAndMtime：复制保留源 mtime；非 Windows 同时保留 mode。
func TestCopyPreservesFileModeAndMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mode.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(src, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.Local)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "mode-copy.txt")
	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("copy: %v", out)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if diff := st.ModTime().Sub(mt); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("mtime 未保留: got %v want %v", st.ModTime(), mt)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o755 {
		t.Fatalf("mode=%o want 755", st.Mode().Perm())
	}
}

// TestCopyDuplicateTarget：同批重复 to → 第二项 duplicate target。
func TestCopyDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dup.txt")
	out, _ := runTool(t, "file_copy", map[string]any{
		"targets": []any{
			map[string]any{"from": src, "to": dst},
			map[string]any{"from": src, "to": dst},
		},
	})
	rs := out["results"].([]any)
	if rs[1].(map[string]any)["error"] != "duplicate target" {
		t.Fatalf("重复目标文案=%v", rs[1])
	}
}

// TestCopyDirectoryCounts：目录复制返回 type=dir、files、bytes（总字节）。
func TestCopyDirectoryCounts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("defgh"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dstdir")
	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("copy: %v", out)
	}
	r := firstResult(t, out)
	if r["type"] != "dir" || r["files"].(float64) != 2 || r["bytes"].(float64) != 8 {
		t.Fatalf("目录计数错误: %v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "b.txt")); string(b) != "defgh" {
		t.Fatalf("子文件内容=%q", b)
	}
}

// TestCopyOverwriteFalse：目标存在且 overwrite=false → failed（文件与目录均适用）。
func TestCopyOverwriteFalse(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": false}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "target exists") {
		t.Fatalf("want target exists failure: %v", r)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatalf("overwrite=false 不应改写目标: %q", b)
	}

	srcDir := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(dir, "dstdir")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": srcDir, "to": dstDir}},
	})
	if r := firstResult(t, out); r["status"] != statusFailed || !strings.Contains(r["error"].(string), "target exists") {
		t.Fatalf("目录目标已存在应 failed: %v", r)
	}
}

// TestCopyNoLockFileAfterSuccess：复制成功后目标旁不存在 `<to>.chonk.lock`
// （copy 已改为目标文件独占 / 非 Windows 原子写，不再使用锁文件）。
func TestCopyNoLockFileAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	}); !ok {
		t.Fatalf("copy: %v", out)
	}
	if _, err := os.Stat(dst + ".chonk.lock"); !os.IsNotExist(err) {
		t.Fatalf("copy 后不应存在锁文件 %s.chonk.lock: %v", dst, err)
	}
}

// TestCopyOverwriteInPlacePartial：目标已存在且 overwrite=true，文件复制中途失败 →
// failed + partial=true，目标保留（可能不完整），不回滚用户已有目标。
func TestCopyOverwriteInPlacePartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("new-content-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(dst, []byte("old-content"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := copyFileAtomicFn
	defer func() { copyFileAtomicFn = old }()
	copyFileAtomicFn = func(_, d string, _ os.FileInfo, _ bool) (int64, error) {
		if werr := os.WriteFile(d, []byte("part"), 0o644); werr != nil {
			return 0, werr
		}
		return 3, errors.New("injected in-place failure")
	}

	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["partial"] != true {
		t.Fatalf("want failed + partial=true: %v", r)
	}
	if !strings.Contains(r["error"].(string), "injected in-place failure") {
		t.Fatalf("error=%v", r["error"])
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("已存在的目标不应被删除: %v", err)
	}
}

// TestMoveNoLockFileAfterSuccess：移动成功后目标旁不存在 `<to>.chonk.lock`
// （同卷 rename / 跨卷复制均不再使用锁文件）。
func TestMoveNoLockFileAfterSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")
	if out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	}); !ok {
		t.Fatalf("move: %v", out)
	}
	if _, err := os.Stat(dst + ".chonk.lock"); !os.IsNotExist(err) {
		t.Fatalf("move 后不应存在锁文件 %s.chonk.lock: %v", dst, err)
	}
}

// TestMoveRenameNonEXDEVKeepsSource：rename 非 EXDEV 错误 → failed(rename:...)，不复制、不删源。
func TestMoveRenameNonEXDEVKeepsSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")

	old := renameFn
	defer func() { renameFn = old }()
	renameFn = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EACCES}
	}

	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || !strings.Contains(r["error"].(string), "rename:") {
		t.Fatalf("want failed rename error: %v", r)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("非 EXDEV rename 失败不应删除源: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("非 EXDEV rename 失败不应创建目标: %v", err)
	}
	if b, _ := os.ReadFile(src); string(b) != "keep me" {
		t.Fatalf("源内容被改动: %q", b)
	}
}

// TestMoveCrossVolumeFallback：rename=EXDEV → 复制成功后删源，bytes 为实际复制字节。
func TestMoveCrossVolumeFallback(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	content := "cross-volume payload"
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out", "dst.txt")

	old := renameFn
	defer func() { renameFn = old }()
	renameFn = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("move: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusOK {
		t.Fatalf("status=%v err=%v", r["status"], r["error"])
	}
	if got := r["bytes"].(float64); got != float64(len(content)) {
		t.Fatalf("bytes=%v want %d", r["bytes"], len(content))
	}
	if b, _ := os.ReadFile(dst); string(b) != content {
		t.Fatalf("目标内容=%q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("跨卷移动成功后源应删除: %v", err)
	}
}

// TestMoveSourceRemoveFailurePartial：复制成功但删源失败 → partial/source_removed:false，目标完整、源仍在。
func TestMoveSourceRemoveFailurePartial(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	content := "remove failure payload"
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")

	oldRename := renameFn
	defer func() { renameFn = oldRename }()
	renameFn = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	oldRemove := removeAllFn
	defer func() { removeAllFn = oldRemove }()
	removeAllFn = func(p string) error {
		if samePath(p, src) {
			return errors.New("injected remove failure")
		}
		return os.RemoveAll(p)
	}

	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["partial"] != true || r["source_removed"] != false {
		t.Fatalf("want failed+partial+source_removed=false: %v", r)
	}
	if !strings.Contains(r["error"].(string), "copied but failed to remove source") {
		t.Fatalf("error=%v", r["error"])
	}
	if got := r["copiedFiles"].(float64); got != 1 {
		t.Fatalf("copiedFiles=%v want 1", r["copiedFiles"])
	}
	if got := r["bytes"].(float64); got != float64(len(content)) {
		t.Fatalf("bytes=%v want %d", r["bytes"], len(content))
	}
	if b, _ := os.ReadFile(dst); string(b) != content {
		t.Fatalf("目标应完整: %q", b)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("删源失败时源应保留: %v", err)
	}
}

// TestMoveSamePathSkipped：from == to → skipped + reason same_path，源仍存在。
func TestMoveSamePathSkipped(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "same.txt")
	if err := os.WriteFile(src, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": src}},
	})
	if !ok {
		t.Fatalf("same path 不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusSkipped || r["reason"] != "same_path" {
		t.Fatalf("want skipped same_path: %v", r)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("same path 不应移动/删除源: %v", err)
	}
	if s := summaryOf(t, out); s["skipped"].(float64) != 1 || s["failed"].(float64) != 0 {
		t.Fatalf("summary=%v", s)
	}
}

// TestMoveDirectoryMergeNotMirror：目标目录已存在（overwrite=true）→ 合并，目标额外文件保留，源删除。
func TestMoveDirectoryMergeNotMirror(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dstdir")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	if !ok {
		t.Fatalf("move: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusOK || r["type"] != "dir" {
		t.Fatalf("status/type=%v/%v err=%v", r["status"], r["type"], r["error"])
	}
	if got := r["files"].(float64); got != 1 {
		t.Fatalf("files=%v want 1", r["files"])
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "alpha" {
		t.Fatalf("源文件未合并: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "keep" {
		t.Fatalf("目标额外文件不应被删除（非镜像）: %q", b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("合并成功后源应删除: %v", err)
	}
}

// TestMoveDuplicateTarget：同批重复 to → 后项 duplicate target。
func TestMoveDuplicateTarget(t *testing.T) {
	dir := t.TempDir()
	src1 := filepath.Join(dir, "src1.txt")
	src2 := filepath.Join(dir, "src2.txt")
	for _, p := range []string{src1, src2} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(dir, "dup.txt")
	out, _ := runTool(t, "file_move", map[string]any{
		"targets": []any{
			map[string]any{"from": src1, "to": dst},
			map[string]any{"from": src2, "to": dst},
		},
	})
	rs := out["results"].([]any)
	if rs[1].(map[string]any)["error"] != "duplicate target" {
		t.Fatalf("重复目标文案=%v", rs[1])
	}
	if b, _ := os.ReadFile(dst); string(b) != "x" {
		t.Fatalf("首项应已移动: %q", b)
	}
}

// TestMoveDuplicateSource：同批重复 from → 后项 duplicate source。
func TestMoveDuplicateSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst1 := filepath.Join(dir, "dst1.txt")
	dst2 := filepath.Join(dir, "dst2.txt")
	out, _ := runTool(t, "file_move", map[string]any{
		"targets": []any{
			map[string]any{"from": src, "to": dst1},
			map[string]any{"from": src, "to": dst2},
		},
	})
	rs := out["results"].([]any)
	if rs[1].(map[string]any)["error"] != "duplicate source" {
		t.Fatalf("重复源文案=%v", rs[1])
	}
	if _, err := os.Stat(dst2); !os.IsNotExist(err) {
		t.Fatalf("重复源后项不应执行: %v", err)
	}
}

// TestMoveCrossVolumePreservesMtime：强制 EXDEV 走复制回退，目标 mtime 与源一致。
func TestMoveCrossVolumePreservesMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	if err := os.WriteFile(src, []byte("mtime"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2021, 2, 3, 4, 5, 6, 0, time.Local)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.txt")

	old := renameFn
	defer func() { renameFn = old }()
	renameFn = func(oldPath, newPath string) error {
		return &os.LinkError{Op: "rename", Old: oldPath, New: newPath, Err: syscall.EXDEV}
	}

	out, ok := runTool(t, "file_move", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst}},
	})
	if !ok {
		t.Fatalf("move: %v", out)
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if diff := st.ModTime().Sub(mt); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("跨卷移动 mtime 未保留: got %v want %v", st.ModTime(), mt)
	}
}

// TestCopyRejectsSymlinkedTargetSubdir：目标树内预置目录 symlink → failed，外部目录不被写入。
func TestCopyRejectsSymlinkedTargetSubdir(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "srcdir")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("escaped"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dstdir")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dst, "sub")); err != nil {
		t.Skipf("平台不支持创建目录 symlink（需权限）: %v", err)
	}

	out, ok := runTool(t, "file_copy", map[string]any{
		"targets": []any{map[string]any{"from": src, "to": dst, "overwrite": true}},
	})
	if !ok {
		t.Fatalf("逐项失败不应整体 IsError: %v", out)
	}
	r := firstResult(t, out)
	if r["status"] != statusFailed || !strings.Contains(r["error"].(string), "symlink") {
		t.Fatalf("want failed symlink refusal: %v", r)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("外部目录不应被写入: %v", entries)
	}
}
