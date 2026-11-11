package server

import (
	"context"
	"testing"
)

func TestComplexityPrunesNestedFunctions(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"n.go": `package main

func Outer(x int) int {
	if x > 0 {
	}
	f := func() {
		if x > 1 {
			if x > 2 {
			}
		}
	}
	_ = f
	return 0
}
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
	hits := w.FindSymbol("", "n.go", "Outer")
	if len(hits) != 1 {
		t.Fatalf("Outer=%v", hits)
	}
	// 只计外层 1 个 if；闭包内 2 个 if 不计入。
	if hits[0].Complexity != 2 {
		t.Fatalf("嵌套闭包应剪枝：complexity=%d", hits[0].Complexity)
	}

	out, err := toolComplexity(context.Background(), map[string]any{"workdir": dir})
	if err != nil {
		t.Fatal(err)
	}
	cov := out.(map[string]any)["coverage"].(map[string]any)
	if cov["indexed_files"] != 1 || cov["parse_error_files"] != 0 {
		t.Fatalf("coverage=%v", cov)
	}
}

func TestToolComplexityFiltersAndCap(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": `package main

type T struct{}

func Simple() {}

func (T) M() {}

func Branch(a, b int) int {
	if a > 0 {
		return b
	}
	for i := 0; i < 3; i++ {
	}
	switch a {
	case 1:
	default:
	}
	return 0
}
`,
		"b.go": `package main

func Complex(x int) int {
	if x > 0 {
		if x > 1 {
			for i := 0; i < x; i++ {
				if i%2 == 0 {
					return i
				}
			}
		}
	}
	return 0
}
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
		out, err := toolComplexity(context.Background(), args)
		if err != nil {
			t.Fatalf("complexity: %v", err)
		}
		return out.(map[string]any)
	}

	// 默认只含函数类。
	m := call(map[string]any{"workdir": dir})
	if m["kind"] != "function" || m["total"] != 4 || m["returned"] != 4 || m["truncated"] != false {
		t.Fatalf("default=%v", m)
	}
	for _, s := range m["symbols"].([]Symbol) {
		if s.Kind != "func" && s.Kind != "method" && s.Kind != "constructor" {
			t.Fatalf("默认不应含非函数符号: %+v", s)
		}
	}
	if cov := m["coverage"].(map[string]any); cov["indexed_files"] != 2 || cov["parse_error_files"] != 0 {
		t.Fatalf("coverage=%v", cov)
	}

	// kind=all 纳入 type；kind=method 单类。
	m = call(map[string]any{"workdir": dir, "kind": "all"})
	if m["total"] != 5 {
		t.Fatalf("kind=all=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "kind": "method"})
	if m["total"] != 1 || m["returned"] != 1 {
		t.Fatalf("kind=method=%v", m)
	}

	// topN 截断 + total 真实 + 稳定排序。
	m = call(map[string]any{"workdir": dir, "topN": 2})
	if m["topN"] != 2 || m["returned"] != 2 || m["total"] != 4 || m["truncated"] != true {
		t.Fatalf("topN2=%v", m)
	}
	first := m["symbols"].([]Symbol)
	if first[0].Complexity < first[1].Complexity {
		t.Fatalf("应为复杂度降序: %v", first)
	}
	again := call(map[string]any{"workdir": dir, "topN": 2})["symbols"].([]Symbol)
	for i := range first {
		if first[i].ID != again[i].ID {
			t.Fatalf("排序不稳定: %v vs %v", first, again)
		}
	}

	// topN 上限截到 200（值本身回报为 200）。
	m = call(map[string]any{"workdir": dir, "topN": 1000000})
	if m["topN"] != 200 || m["returned"] != 4 || m["truncated"] != false {
		t.Fatalf("topN cap=%v", m)
	}

	// minCc 过滤。
	m = call(map[string]any{"workdir": dir, "minCc": 100})
	if m["total"] != 0 || m["returned"] != 0 || m["note"] != "no_matches" {
		t.Fatalf("minCc=%v", m)
	}

	// file 子串过滤与未命中。
	m = call(map[string]any{"workdir": dir, "file": "a.go"})
	if m["total"] != 3 {
		t.Fatalf("file=a.go=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "file": "nope.go"})
	if m["total"] != 0 || m["note"] != "file_not_indexed" {
		t.Fatalf("file miss=%v", m)
	}

	// 严格参数。
	for name, args := range map[string]map[string]any{
		"未知参数":    {"workdir": dir, "foo": 1},
		"topN 0":  {"workdir": dir, "topN": 0},
		"minCc 0": {"workdir": dir, "minCc": 0},
		"kind 非法": {"workdir": dir, "kind": "type"},
		"file 类型": {"workdir": dir, "file": 1},
		"topN 小数": {"workdir": dir, "topN": 1.5},
	} {
		if _, err := toolComplexity(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}
