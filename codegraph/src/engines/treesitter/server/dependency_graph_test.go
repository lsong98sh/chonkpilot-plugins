package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDependencyGraphExtraction(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": `package main

import (
	"fmt"
	"os"
)

import "strings"

func main() { fmt.Println(os.Args, strings.TrimSpace("")) }
`,
		"b.rs": `pub use foo::bar;
pub(crate) use baz::qux;
use std::io;
mod keep;
`,
		"c.js": `import x from "pkg";
export { y } from "other";
const legacy = require("legacy");
const dyn = import("dyn");
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
	deps, total, totalEdges, matched := w.DependencyGraphPage("", 500, 0, true)
	if !matched {
		t.Fatalf("empty filter 应 matched")
	}
	if total != 3 || totalEdges != 8 {
		t.Fatalf("total=%d totalEdges=%d deps=%v", total, totalEdges, deps)
	}
	byRel := map[string][]string{}
	for k, v := range deps {
		byRel[w.norm(k)] = v
	}
	for _, want := range []string{"fmt", "os", "strings"} {
		if !containsStr(byRel["a.go"], want) {
			t.Errorf("go 分组/单行 import 缺 %q: %v", want, byRel["a.go"])
		}
	}
	for _, want := range []string{"foo::bar", "baz::qux", "std::io"} {
		if !containsStr(byRel["b.rs"], want) {
			t.Errorf("rust import 缺 %q: %v", want, byRel["b.rs"])
		}
	}
	if !containsStr(byRel["c.js"], "pkg") || !containsStr(byRel["c.js"], "other") {
		t.Errorf("js 静态 import 缺: %v", byRel["c.js"])
	}
	if containsStr(byRel["c.js"], "legacy") || containsStr(byRel["c.js"], "dyn") {
		t.Errorf("require()/动态 import() 不应提取: %v", byRel["c.js"])
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestToolDependencyGraphPageAndFilters(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go":        "package main\nimport (\n\"fmt\"\n\"os\"\n)\n",
		"b.go":        "package main\nimport \"strings\"\n",
		"c.go":        "package main\n",
		"sub/a.go":    "package sub\nimport \"extra\"\n",
		"sub/util.go": "package sub\nimport \"util/test\"\n",
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
		out, err := toolDependencyGraph(context.Background(), args)
		if err != nil {
			t.Fatalf("dependency graph: %v", err)
		}
		return out.(map[string]any)
	}

	// 默认：跳过无 import 的 c.go；4 个文件 5 条边。
	m := call(map[string]any{"workdir": dir})
	if m["total"] != 4 || m["returned"] != 4 || m["files"] != 4 || m["edges"] != 5 || m["totalEdges"] != 5 || m["truncated"] != false {
		t.Fatalf("default=%v", m)
	}
	if len(m["deps"].(map[string][]string)) != 4 {
		t.Fatalf("deps=%v", m["deps"])
	}

	// include_empty=true：包含 c.go（值为空数组）。
	m = call(map[string]any{"workdir": dir, "include_empty": true})
	if m["total"] != 5 || m["returned"] != 5 || m["edges"] != 5 || m["totalEdges"] != 5 {
		t.Fatalf("include_empty=%v", m)
	}

	// 分页：total 真实、truncated 正确。
	m = call(map[string]any{"workdir": dir, "limit": 1})
	if m["returned"] != 1 || m["truncated"] != true {
		t.Fatalf("limit1=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "limit": 1, "offset": 3})
	if m["returned"] != 1 || m["truncated"] != false {
		t.Fatalf("offset3=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "offset": 99})
	if m["returned"] != 0 || m["note"] != "offset_beyond_total" {
		t.Fatalf("offset99=%v", m)
	}

	// file 过滤：大小写不敏感 + 路径段后缀；重名时 ambiguous。
	m = call(map[string]any{"workdir": dir, "file": "A.GO"})
	if m["total"] != 2 || m["file_matched"] != true || m["ambiguous"] != true {
		t.Fatalf("case-insensitive/ambiguous=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "file": "sub/a.go"})
	if m["total"] != 1 || m["ambiguous"] != nil {
		t.Fatalf("exact=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "file": filepath.Join(dir, "b.go")})
	if m["total"] != 1 || m["file_matched"] != true {
		t.Fatalf("absolute=%v", m)
	}

	// 未命中 / 命中但无 import。
	m = call(map[string]any{"workdir": dir, "file": "nope.go"})
	if m["total"] != 0 || m["file_matched"] != false || m["note"] != "file_not_indexed" {
		t.Fatalf("not_found=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "file": "c.go"})
	if m["total"] != 0 || m["file_matched"] != true || m["note"] != "no_dependencies" {
		t.Fatalf("empty file=%v", m)
	}

	// 严格参数。
	for name, args := range map[string]map[string]any{
		"未知参数":       {"workdir": dir, "foo": 1},
		"file 类型":    {"workdir": dir, "file": 1},
		"limit 0":    {"workdir": dir, "limit": 0},
		"offset 负":   {"workdir": dir, "offset": -1},
		"include 类型": {"workdir": dir, "include_empty": "yes"},
	} {
		if _, err := toolDependencyGraph(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestToolDependencyGraphEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	out, err := toolDependencyGraph(context.Background(), map[string]any{"workdir": dir, "include_empty": true})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["total"] != 0 || m["note"] != "no_indexed_files" {
		t.Fatalf("empty index=%v", m)
	}
	out, err = toolDependencyGraph(context.Background(), map[string]any{"workdir": dir})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["note"] != "no_dependencies" {
		t.Fatalf("empty index default=%v", m)
	}
}
