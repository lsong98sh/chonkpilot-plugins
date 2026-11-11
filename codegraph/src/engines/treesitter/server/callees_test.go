package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestToolCalleesTargetsAndCandidates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(`package main

type A struct{}
type B struct{}
type T struct{}

func Helper() {}
func Run() {}
func (A) Dup() {}
func (B) Dup() {}
func Main() { Helper(); Run(); External(); A{}.Dup(); B{}.Dup() }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(`package main

func Run() {}
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

	out, err := toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Main", "limit": 1})
	if err != nil {
		t.Fatalf("callees: %v", err)
	}
	m := out.(map[string]any)
	if m["matches"] != 1 || m["ambiguous"] != false || m["total"] != 4 || m["returned"] != 1 || m["truncated"] != true {
		t.Fatalf("分页/目标返回=%v", m)
	}
	if m["symbol"] == nil || len(m["symbols"].([]Symbol)) != 1 {
		t.Fatalf("target=%v", m)
	}

	// 全量检查 candidates：External=0、Helper=1、Run=2、Dup=2。
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Main", "limit": 500})
	refs := out.(map[string]any)["callees"].([]CalleeRef)
	byName := map[string]CalleeRef{}
	for _, r := range refs {
		byName[r.Name] = r
	}
	if r := byName["External"]; r.Resolved || r.Candidates != 0 {
		t.Fatalf("External=%+v", r)
	}
	if r := byName["Helper"]; !r.Resolved || r.Candidates != 1 || r.File == "" {
		t.Fatalf("Helper=%+v", r)
	}
	if r := byName["Run"]; r.Resolved || r.Candidates != 2 {
		t.Fatalf("Run=%+v", r)
	}
	if r := byName["Dup"]; r.Resolved || r.Candidates != 2 {
		t.Fatalf("Dup=%+v", r)
	}

	// id 优先：错误的 file/name 被忽略。
	main := w.SearchSymbol("Main", "func", "", 1)
	if len(main) != 1 {
		t.Fatalf("Main search=%v", main)
	}
	out, err = toolCallees(context.Background(), map[string]any{
		"workdir": dir, "id": main[0].ID, "file": "nope.go", "name": "Nope", "limit": 500,
	})
	if err != nil || out.(map[string]any)["total"] != 4 {
		t.Fatalf("id 优先: out=%v err=%v", out, err)
	}

	// 目标多命中：返回 symbols 候选，callees 只针对首条。
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Dup"})
	m = out.(map[string]any)
	if m["matches"] != 2 || m["ambiguous"] != true || len(m["symbols"].([]Symbol)) != 2 {
		t.Fatalf("ambiguous target=%v", m)
	}
	if m["note"] != "no_calls" {
		t.Fatalf("Dup 无调用 note=%v", m)
	}

	// 非函数目标：not_callable。
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "T"})
	if m := out.(map[string]any); m["note"] != "not_callable" || m["total"] != 0 {
		t.Fatalf("not_callable=%v", m)
	}

	// offset 越界。
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Main", "offset": 99})
	if m := out.(map[string]any); m["returned"] != 0 || m["note"] != "offset_beyond_total" {
		t.Fatalf("offset=%v", m)
	}

	// not_found 原因。
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "nope.go", "name": "X"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "file_not_indexed" {
		t.Fatalf("file_not_indexed=%v", m)
	}
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Nope"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "name_not_found" {
		t.Fatalf("name_not_found=%v", m)
	}
	out, _ = toolCallees(context.Background(), map[string]any{"workdir": dir, "id": "missing"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "id_not_found" {
		t.Fatalf("id_not_found=%v", m)
	}

	// 参数校验。
	for name, args := range map[string]map[string]any{
		"缺定位":      {"workdir": dir},
		"未知参数":     {"workdir": dir, "file": "a.go", "name": "Main", "foo": 1},
		"limit 0":  {"workdir": dir, "file": "a.go", "name": "Main", "limit": 0},
		"offset 负": {"workdir": dir, "file": "a.go", "name": "Main", "offset": -1},
		"类型错误":     {"workdir": dir, "file": 1, "name": "Main"},
	} {
		if _, err := toolCallees(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}
