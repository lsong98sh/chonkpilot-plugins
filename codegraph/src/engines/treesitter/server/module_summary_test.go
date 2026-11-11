package server

import (
	"context"
	"path/filepath"
	"testing"
)

func TestToolModuleSummaryScopeAndCoverage(t *testing.T) {
	dir := t.TempDir()
	slashDir := filepath.ToSlash(dir)
	writeFiles(t, dir, map[string]string{
		"src/a.go": `package main

type T struct{}

func Low(x int) int {
	if x > 0 {
		return x
	}
	return 0
}
`,
		"src/deep/c.go": `package deep

func Deep() {}
`,
		"srcfoo/b.go": `package srcfoo

func Foo() {}
`,
		"SRC/x.go": `package src

func Case() {}
`,
		"bad.go": `package main

func (
`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	call := func(args map[string]any) map[string]any {
		t.Helper()
		out, err := toolModuleSummary(context.Background(), args)
		if err != nil {
			t.Fatalf("module summary: %v", err)
		}
		return out.(map[string]any)
	}

	// 全项目：scope=workdir，bad.go 计入 files 与 parseErrorFiles。
	m := call(map[string]any{"workdir": dir})
	if m["scope"] != slashDir || m["files"] != 5 {
		t.Fatalf("all=%v", m)
	}
	if m["parseErrorFiles"] != 1 {
		t.Fatalf("parseErrorFiles=%v", m["parseErrorFiles"])
	}
	if _, ok := m["lastIndexedAt"]; !ok {
		t.Fatalf("缺少 lastIndexedAt: %v", m)
	}
	for _, s := range m["topComplexity5"].([]Symbol) {
		if s.Kind != "func" && s.Kind != "method" && s.Kind != "constructor" {
			t.Fatalf("topComplexity5 不应含非函数: %+v", s)
		}
	}

	// 路径段边界：src 不吞 srcfoo；大小写不敏感（SRC）。
	for _, p := range []string{"src", "SRC", "./src/"} {
		m = call(map[string]any{"workdir": dir, "path": p})
		if m["files"] != 3 {
			t.Fatalf("path=%q files=%v", p, m["files"])
		}
		if m["parseErrorFiles"] != 0 {
			t.Fatalf("path=%q parseErrorFiles=%v", p, m["parseErrorFiles"])
		}
	}
	if m := call(map[string]any{"workdir": dir, "path": "src"}); m["scope"] != slashDir+"/src" {
		t.Fatalf("scope=%v", m["scope"])
	}
	// 绝对路径（大小写不同）同样命中。
	if m := call(map[string]any{"workdir": dir, "path": dir + "/SRC"}); m["files"] != 3 {
		t.Fatalf("absolute path files=%v", m["files"])
	}
	// srcfoo 独立。
	if m := call(map[string]any{"workdir": dir, "path": "srcfoo"}); m["files"] != 1 {
		t.Fatalf("srcfoo files=%v", m["files"])
	}
	// 未命中。
	m = call(map[string]any{"workdir": dir, "path": "nope"})
	if m["files"] != 0 || m["note"] != "path_not_indexed" {
		t.Fatalf("miss=%v", m)
	}

	// 严格参数。
	for name, args := range map[string]map[string]any{
		"未知参数":    {"workdir": dir, "foo": 1},
		"path 类型": {"workdir": dir, "path": 1},
	} {
		if _, err := toolModuleSummary(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}
