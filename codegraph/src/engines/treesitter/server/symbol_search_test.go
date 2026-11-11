package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchSymbolPage(t *testing.T) {
	dir := t.TempDir()
	w := makeTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	syms, total := w.SearchSymbolPage("helper", "", "", 1, 0)
	if total != 2 || len(syms) != 1 {
		t.Fatalf("page0 total=%d len=%d", total, len(syms))
	}
	syms, total = w.SearchSymbolPage("helper", "", "", 1, 1)
	if total != 2 || len(syms) != 1 || syms[0].Line != 10 {
		t.Fatalf("page1 total=%d syms=%+v", total, syms)
	}
	syms, total = w.SearchSymbolPage("helper", "", "", 1, 2)
	if total != 2 || len(syms) != 0 || syms == nil {
		t.Fatalf("offset beyond total=%d syms=%v", total, syms)
	}

	// kind 大小写不敏感；非法 kind 由入口校验拒绝。
	if _, total := w.SearchSymbolPage("", "FUNC", "", 0, 0); total != 2 {
		t.Fatalf("case-insensitive kind total=%d", total)
	}
	if !w.HasIndexedFile("constant") || w.HasIndexedFile("nope") {
		t.Fatalf("HasIndexedFile 判定错误")
	}
	if w.IndexedFileCount() != 3 {
		t.Fatalf("IndexedFileCount=%d", w.IndexedFileCount())
	}
}

func TestValidateSymbolSearchArgs(t *testing.T) {
	kind, limit, offset, err := validateSymbolSearchArgs(map[string]any{"kind": "FUNC"})
	if err != nil || kind != "func" || limit != 50 || offset != 0 {
		t.Fatalf("默认值: kind=%q limit=%d offset=%d err=%v", kind, limit, offset, err)
	}
	if _, limit, offset, err = validateSymbolSearchArgs(map[string]any{"limit": float64(999), "offset": float64(3)}); err != nil || limit != 500 || offset != 3 {
		t.Fatalf("上限/offset: limit=%d offset=%d err=%v", limit, offset, err)
	}
	for name, args := range map[string]map[string]any{
		"未知参数":      {"foo": true},
		"kind 非法":   {"kind": "variable"},
		"kind 非字符串": {"kind": 1},
		"limit 0":   {"limit": 0},
		"limit 小数":  {"limit": 1.5},
		"limit 字符串": {"limit": "10"},
		"offset 负数": {"offset": -1},
	} {
		if _, _, _, err := validateSymbolSearchArgs(args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestToolSymbolSearchPaginationAndNotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(`package main

func Alpha() {}
func Beta() {}
func main() { Alpha(); Beta() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	out, err := toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "limit": 1})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	m := out.(map[string]any)
	if m["total"].(int) < 3 || m["returned"] != 1 || m["truncated"] != true || m["limit"] != 1 {
		t.Fatalf("分页返回=%v", m)
	}
	if got := m["symbols"].([]Symbol); len(got) != 1 {
		t.Fatalf("symbols=%v", got)
	}

	// kind 大小写不敏感。
	out, err = toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "kind": "FUNC", "limit": 500})
	if err != nil {
		t.Fatalf("kind: %v", err)
	}
	if m := out.(map[string]any); m["total"].(int) < 3 || m["truncated"] != false {
		t.Fatalf("kind 返回=%v", m)
	}

	// 空结果 note：file 未索引 / 无匹配 / 翻页越界。
	out, err = toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "file": "nope.zzz"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["note"] != "file_not_indexed" || len(m["symbols"].([]Symbol)) != 0 {
		t.Fatalf("file_not_indexed=%v", m)
	}
	out, _ = toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "query": "zzzz"})
	if m := out.(map[string]any); m["note"] != "no_match" {
		t.Fatalf("no_match=%v", m)
	}
	out, _ = toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "offset": 999})
	if m := out.(map[string]any); m["note"] != "offset_beyond_total" || m["returned"] != 0 {
		t.Fatalf("offset_beyond_total=%v", m)
	}

	// 非法 kind / 未知参数由入口报错。
	if _, err := toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "kind": "variable"}); err == nil {
		t.Fatal("非法 kind 应报错")
	}
	if _, err := toolSymbolSearch(context.Background(), map[string]any{"workdir": dir, "name": "x"}); err == nil {
		t.Fatal("未知参数应报错")
	}
}
