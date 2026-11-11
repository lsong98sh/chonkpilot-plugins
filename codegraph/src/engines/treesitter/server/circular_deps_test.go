package server

import (
	"context"
	"reflect"
	"testing"
)

func TestToolCircularDepsPage(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.js": `import "./b";`,
		"b.js": `import "./a";
import "./c";`,
		"c.js": `import "./b";`,
		"d.js": `const x = 1;`,
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
			t.Fatalf("circular deps: %v", err)
		}
		return out.(map[string]any)
	}

	m := call(map[string]any{"workdir": dir, "limit": 500})
	if m["cycleCount"] != 2 || m["returned"] != 2 || m["truncated"] != false {
		t.Fatalf("cycles=%v", m)
	}
	want := [][]string{{"a.js", "b.js", "a.js"}, {"b.js", "c.js", "b.js"}}
	if !reflect.DeepEqual(m["cycles"], want) {
		t.Fatalf("cycles=%v want=%v", m["cycles"], want)
	}
	for _, cyc := range m["cycles"].([][]string) {
		for _, p := range cyc {
			if len(p) > 1 && (p[1] == ':' || p[0] == '/') {
				t.Fatalf("应为相对路径: %v", cyc)
			}
		}
	}

	// 确定性：重复调用结果一致。
	if again := call(map[string]any{"workdir": dir, "limit": 500}); !reflect.DeepEqual(again["cycles"], m["cycles"]) {
		t.Fatalf("结果不稳定: first=%v second=%v", m["cycles"], again["cycles"])
	}

	// 分页。
	m = call(map[string]any{"workdir": dir, "limit": 1})
	if m["returned"] != 1 || m["truncated"] != true {
		t.Fatalf("limit1=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "limit": 1, "offset": 1})
	if m["returned"] != 1 || m["truncated"] != false || !reflect.DeepEqual(m["cycles"], [][]string{{"b.js", "c.js", "b.js"}}) {
		t.Fatalf("offset1=%v", m)
	}
	m = call(map[string]any{"workdir": dir, "offset": 99})
	if m["returned"] != 0 || m["note"] != "offset_beyond_total" {
		t.Fatalf("offset99=%v", m)
	}

	// 严格参数。
	for name, args := range map[string]map[string]any{
		"未知参数":       {"workdir": dir, "foo": 1},
		"limit 0":    {"workdir": dir, "limit": 0},
		"offset 负":   {"workdir": dir, "offset": -1},
		"include 类型": {"workdir": dir, "include_self": "yes"},
	} {
		if _, err := toolCircularDeps(context.Background(), args); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestToolCircularDepsSelfAndNoCycles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"self.js": `import "./self";`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	out, err := toolCircularDeps(context.Background(), map[string]any{"workdir": dir})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["cycleCount"] != 1 || !reflect.DeepEqual(m["cycles"], [][]string{{"self.js", "self.js"}}) {
		t.Fatalf("默认应包含自环: %v", m)
	}

	out, err = toolCircularDeps(context.Background(), map[string]any{"workdir": dir, "include_self": false})
	if err != nil {
		t.Fatal(err)
	}
	m = out.(map[string]any)
	if m["cycleCount"] != 0 || m["note"] != "no_cycles" || !reflect.DeepEqual(m["cycles"], [][]string{}) {
		t.Fatalf("include_self=false: %v", m)
	}
}
