package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateGetSymbolInfoArgs(t *testing.T) {
	id, file, name, err := validateGetSymbolInfoArgs(map[string]any{
		"workdir": "E:/p", "id": "a.go:1:X:func", "file": "ignored.go", "name": "Ignored",
	})
	if err != nil || id == "" || file != "ignored.go" || name != "Ignored" {
		t.Fatalf("id 参数: id=%q file=%q name=%q err=%v", id, file, name, err)
	}
	if _, _, _, err := validateGetSymbolInfoArgs(map[string]any{"workdir": "E:/p", "file": "a.go"}); err == nil {
		t.Fatal("缺 name 应报错")
	}
	for name, args := range map[string]map[string]any{
		"未知参数": {"workdir": "E:/p", "id": "x", "foo": 1},
		"类型错误": {"workdir": "E:/p", "id": 1},
		"全部缺失": {"workdir": "E:/p"},
	} {
		if _, _, _, err := validateGetSymbolInfoArgs(args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestToolGetSymbolInfoCandidatesAndNotes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(`package main

type A struct{}
type B struct{}
func (A) Helper() {}
func (B) Helper() {}
func Alpha() { A{}.Helper(); B{}.Helper() }
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
	alpha := w.SearchSymbol("Alpha", "func", "", 1)
	if len(alpha) != 1 {
		t.Fatalf("Alpha search=%v", alpha)
	}

	// id 命中，且 id 优先于错误的 file/name。
	out, err := toolGetSymbolInfo(context.Background(), map[string]any{
		"workdir": dir, "id": alpha[0].ID, "file": "nope.go", "name": "Nope",
	})
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	m := out.(map[string]any)
	if m["matches"] != 1 || m["ambiguous"] != false || m["workdir"] != w.Dir {
		t.Fatalf("by id=%v", m)
	}
	if got := m["symbols"].([]Symbol); len(got) != 1 || got[0].Name != "Alpha" {
		t.Fatalf("symbols=%v", got)
	}

	// file+name 多命中：返回全部候选。
	out, err = toolGetSymbolInfo(context.Background(), map[string]any{
		"workdir": dir, "file": "A.GO", "name": "helper",
	})
	if err != nil {
		t.Fatalf("by file+name: %v", err)
	}
	m = out.(map[string]any)
	if m["matches"] != 2 || m["ambiguous"] != true || m["symbol"] == nil {
		t.Fatalf("ambiguous=%v", m)
	}
	if got := m["symbols"].([]Symbol); len(got) != 2 || got[0].Name != "Helper" || got[1].Name != "Helper" {
		t.Fatalf("symbols=%v", got)
	}

	// not_found 原因。
	out, _ = toolGetSymbolInfo(context.Background(), map[string]any{"workdir": dir, "file": "nope.go", "name": "X"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "file_not_indexed" {
		t.Fatalf("file_not_indexed=%v", m)
	}
	out, _ = toolGetSymbolInfo(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Nope"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "name_not_found" {
		t.Fatalf("name_not_found=%v", m)
	}
	out, _ = toolGetSymbolInfo(context.Background(), map[string]any{"workdir": dir, "id": "missing"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "id_not_found" {
		t.Fatalf("id_not_found=%v", m)
	}

	// 参数错误。
	if _, err := toolGetSymbolInfo(context.Background(), map[string]any{"workdir": dir}); err == nil {
		t.Fatal("缺定位参数应报错")
	}
	if _, err := toolGetSymbolInfo(context.Background(), map[string]any{"workdir": dir, "id": "x", "foo": 1}); err == nil {
		t.Fatal("未知参数应报错")
	}
}
