package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCallersFileSegmentMatch file 过滤按路径段后缀，不再命中 data.go 这类子串。
func TestCallersFileSegmentMatch(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go":    "package main\nfunc Run() {}\nfunc Main() { Run() }\n",
		"data.go": "package main\nfunc Other() { Run() }\n",
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	out, err := toolCallers(context.Background(), map[string]any{
		"workdir": dir, "name": "Run", "file": "a.go", "limit": 500,
	})
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	m := out.(map[string]any)
	if m["total"] != 1 {
		t.Fatalf("file=a.go 应只命中 a.go 内的调用方: %v", m)
	}
	caller := m["callers"].([]Symbol)[0]
	if filepath.Base(caller.File) != "a.go" {
		t.Fatalf("命中错误文件: %s", caller.File)
	}
}

// TestCircularDepsMaxLengthAndPath 验证 max_cycle_length 与 path 子图过滤。
func TestCircularDepsMaxLengthAndPath(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.js":      `import "./b";`,
		"b.js":      `import "./a";`,
		"x/c.js":    `import "./d";`,
		"x/d.js":    `import "./c";`,
		"p.js":      `import "./q";`,
		"q.js":      `import "./r";`,
		"r.js":      `import "./p";`,
		"lonely.js": `const x = 1;`,
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
		out, err := toolCircularDeps(context.Background(), args)
		if err != nil {
			t.Fatalf("circular: %v", err)
		}
		return out.(map[string]any)
	}

	m := call(map[string]any{"workdir": dir, "limit": 500, "max_cycle_length": 2})
	if m["truncated"] != false || m["cycleCount"] != 2 {
		t.Fatalf("max_cycle_length=2 应只留两个 2 环: %v", m)
	}
	for _, cyc := range m["cycles"].([][]string) {
		if len(cyc)-1 > 2 {
			t.Fatalf("出现超长环: %v", cyc)
		}
	}

	m = call(map[string]any{"workdir": dir, "limit": 500, "path": "x"})
	if m["path"] != "x" || m["cycleCount"] != 1 {
		t.Fatalf("path=x 应只留子图内 1 个环: %v", m)
	}
	if cyc := m["cycles"].([][]string)[0]; !strings.HasPrefix(cyc[0], "x/") {
		t.Fatalf("子图路径错误: %v", cyc)
	}

	m = call(map[string]any{"workdir": dir, "limit": 500, "path": "nope"})
	if m["cycleCount"] != 0 || m["note"] != "path_not_indexed" {
		t.Fatalf("不存在的 path 应无环并提示: %v", m)
	}

	// ctx 取消：枚举中止并返回错误。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := toolCircularDeps(ctx, map[string]any{"workdir": dir, "limit": 500}); err == nil {
		t.Fatal("已取消的 ctx 应返回错误")
	}
}

// TestCoverageNotes dependency_graph / module_summary 均给出覆盖口径说明。
func TestCoverageNotes(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": "package main\nimport \"fmt\"\nfunc Main() { fmt.Println(1) }\n",
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	out, err := toolDependencyGraph(context.Background(), map[string]any{"workdir": dir})
	if err != nil {
		t.Fatalf("dep graph: %v", err)
	}
	cov, _ := out.(map[string]any)["coverage"].(map[string]any)
	if cov == nil || cov["note"] == nil || cov["indexed_files"] == nil {
		t.Fatalf("dependency graph 缺少 coverage: %v", out)
	}

	out, err = toolModuleSummary(context.Background(), map[string]any{"workdir": dir})
	if err != nil {
		t.Fatalf("module summary: %v", err)
	}
	if note, _ := out.(map[string]any)["coverageNote"].(string); note == "" {
		t.Fatalf("module summary 缺少 coverageNote: %v", out)
	}
}

// TestFindSymbolByIndexID 按 id 查询走 byID 索引（行为与旧线性扫描一致）。
func TestFindSymbolByIndexID(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": "package main\nfunc Run() {}\nfunc Main() { Run() }\n",
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	run := w.SearchSymbol("Run", "func", "", 1)
	if len(run) != 1 {
		t.Fatalf("Run=%v", run)
	}
	hits := w.FindSymbol(run[0].ID, "", "")
	if len(hits) != 1 || hits[0].ID != run[0].ID {
		t.Fatalf("by id 命中=%v want=%v", hits, run[0])
	}
	if got := w.FindSymbol("", "a.go", "Run"); len(got) != 1 {
		t.Fatalf("file+name 命中=%v", got)
	}
	// 不存在的 id 不应回落到全量扫描。
	if got := w.FindSymbol("nope.go:1:X:func", "", ""); len(got) != 0 {
		t.Fatalf("不存在 id 应无命中: %v", got)
	}
	_ = os.Remove(filepath.Join(dir, "a.go")) // 保持 os import 使用
}

// TestCallersLangFilter 同名跨语言调用方可按 lang 收窄。
func TestCallersLangFilter(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": "package main\nfunc Run() {}\nfunc GoMain() { Run() }\n",
		"b.js": "function Run() {}\nfunction JsMain() { Run() }\n",
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
		out, err := toolCallers(context.Background(), args)
		if err != nil {
			t.Fatalf("callers: %v", err)
		}
		return out.(map[string]any)
	}
	all := call(map[string]any{"workdir": dir, "name": "Run", "limit": 500})
	if all["total"].(int) < 2 {
		t.Fatalf("跨语言应至少 2 个调用方: %v", all)
	}
	goOnly := call(map[string]any{"workdir": dir, "name": "Run", "lang": "go", "limit": 500})
	if goOnly["total"] != 1 || goOnly["lang"] != "go" {
		t.Fatalf("lang=go 应只留 Go 调用方: %v", goOnly)
	}
	if c := goOnly["callers"].([]Symbol)[0]; c.Lang != "go" {
		t.Fatalf("语言过滤错误: %+v", c)
	}
	if _, err := toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run", "lang": "cobol"}); err == nil {
		t.Fatal("非法 lang 应报错")
	}
}
