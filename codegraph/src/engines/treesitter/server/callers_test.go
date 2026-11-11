package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestToolCallersPaginationAndTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(`package main

func Run() {}
func Main() { Run() }
func Other() { Run() }
func Lonely() {}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.go"), []byte(`package main

func Third() { Run() }
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

	out, err := toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run", "limit": 1})
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	m := out.(map[string]any)
	if m["total"] != 3 || m["returned"] != 1 || m["truncated"] != true || m["limit"] != 1 {
		t.Fatalf("分页返回=%v", m)
	}
	if m["target"] != "Run" || m["target_defined"] != true || m["target_matches"] != 1 || m["target_symbol"] == nil {
		t.Fatalf("目标身份=%v", m)
	}
	if got := m["callers"].([]Symbol); len(got) != 1 {
		t.Fatalf("callers=%v", got)
	}

	// id 优先：即使 name 指向别的符号，也按 id 解析。
	run := w.SearchSymbol("Run", "func", "", 1)
	if len(run) != 1 {
		t.Fatalf("Run search=%v", run)
	}
	out, err = toolCallers(context.Background(), map[string]any{
		"workdir": dir, "id": run[0].ID, "name": "Main", "limit": 500,
	})
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	m = out.(map[string]any)
	if m["target"] != "Run" || m["total"] != 3 || m["truncated"] != false {
		t.Fatalf("id 优先=%v", m)
	}

	// file 子串过滤 + offset 翻页。
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run", "file": "b.go"})
	if m := out.(map[string]any); m["total"] != 1 || m["callers"].([]Symbol)[0].Name != "Third" {
		t.Fatalf("file filter=%v", m)
	}
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run", "offset": 1, "limit": 1})
	if m := out.(map[string]any); m["returned"] != 1 || m["truncated"] != true {
		t.Fatalf("offset=%v", m)
	}
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run", "offset": 99})
	if m := out.(map[string]any); m["returned"] != 0 || m["note"] != "offset_beyond_total" {
		t.Fatalf("offset beyond=%v", m)
	}

	// 空结果 note：目标已定义但无调用方 / 目标未定义 / id 未命中。
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Lonely"})
	if m := out.(map[string]any); m["total"] != 0 || m["note"] != "no_callers" || m["target_defined"] != true {
		t.Fatalf("no_callers=%v", m)
	}
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "ExternalThing"})
	if m := out.(map[string]any); m["total"] != 0 || m["note"] != "target_not_indexed" || m["target_defined"] != false {
		t.Fatalf("target_not_indexed=%v", m)
	}
	out, _ = toolCallers(context.Background(), map[string]any{"workdir": dir, "id": "missing"})
	if m := out.(map[string]any); m["status"] != "not_found" || m["note"] != "id_not_found" {
		t.Fatalf("id_not_found=%v", m)
	}

	// 参数校验。
	for name, args := range map[string]map[string]any{
		"缺定位":      {"workdir": dir},
		"未知参数":     {"workdir": dir, "name": "Run", "foo": 1},
		"limit 0":  {"workdir": dir, "name": "Run", "limit": 0},
		"offset 负": {"workdir": dir, "name": "Run", "offset": -1},
		"类型错误":     {"workdir": dir, "name": 1},
	} {
		if _, err := toolCallers(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}
