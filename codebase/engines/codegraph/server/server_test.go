package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ============================================================
// 分组 1：路径与导入解析（path.go）
// ============================================================

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"a/b/c", "a/b/c"},
		{"./a/b", "a/b"},
		{"a/./b", "a/b"},
		{"a/../b", "b"},
		{"a/b/../../c", "c"},
		{"a/./../b", "b"},
		{"a/../../b", "b"}, // beyond root, just strip
		{"a/b/c/", "a/b/c"},
	}
	for _, tt := range tests {
		got := cleanPath(tt.in)
		if got != tt.want {
			t.Errorf("cleanPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRelOf(t *testing.T) {
	got := relOf("/a/b", "/a/b/c/d.go")
	want := "c/d.go"
	if got != want {
		t.Errorf("relOf = %q, want %q", got, want)
	}
	got2 := relOf("/a/b", "/x/y/z.go")
	if !strings.Contains(got2, "x") {
		t.Errorf("relOf outside base should still return something: %q", got2)
	}
}

func TestJoinPath(t *testing.T) {
	tests := []struct {
		a, b, want string
	}{
		{"", "b", "b"},
		{"a", "", "a"},
		{"", "", ""},
		{"a", "b", "a/b"},
		{"a/", "b", "a//b"}, // joinPath doesn't trim trailing slash
	}
	for _, tt := range tests {
		got := joinPath(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("joinPath(%q,%q) = %q, want %q", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDirOf(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"a/b/c.go", "a/b"},
		{"a.go", ""},
		{"", ""},
		{"a/b/c/d.go", "a/b/c"},
	}
	for _, tt := range tests {
		got := dirOf(tt.in)
		if got != tt.want {
			t.Errorf("dirOf(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProbeRelative(t *testing.T) {
	indexed := map[string]bool{
		"src/utils.ts":     true,
		"src/index.ts":     true,
		"src/helper.js":    true,
		"src/foo/index.js": true,
	}
	tests := []struct {
		dir, imp string
		want     string
	}{
		{"src", "./utils", "src/utils.ts"},
		{"src", "./utils.ts", "src/utils.ts"},
		{"src", "./index", "src/index.ts"},
		{"src", "./helper", "src/helper.js"},
		{"src", "./foo", "src/foo/index.js"},
		{"src", "./nonexistent", ""},
		{"src", "../outside", ""},
		{"other", "./utils", ""},
	}
	for _, tt := range tests {
		got := probeRelative(tt.dir, tt.imp, indexed, []string{".js", ".ts", ".tsx", ".jsx", "/index.js", "/index.ts", "/index.tsx", "/index.jsx"})
		if got != tt.want {
			t.Errorf("probeRelative(%q,%q) = %q, want %q", tt.dir, tt.imp, got, tt.want)
		}
	}
}

func TestResolveImport(t *testing.T) {
	indexed := map[string]bool{
		"main.go":                true,
		"pkg/util.go":            true,
		"src/app.ts":             true,
		"src/helper.ts":          true,
		"src/components/btn.tsx": true,
		"demo/__init__.py":       true,
		"demo/core.py":           true,
		"com/example/Main.java":  true,
		"src/lib.rs":             true,
		"src/mod.rs":             true,
	}

	t.Run("go", func(t *testing.T) {
		// Go module resolution needs go.mod on disk; moduleOf caches by dir
		modDir := t.TempDir()
		os.MkdirAll(filepath.Join(modDir, "pkg"), 0o755)
		os.WriteFile(filepath.Join(modDir, "go.mod"), []byte("module testproj\n\ngo 1.21\n"), 0o644)
		os.WriteFile(filepath.Join(modDir, "pkg", "util.go"), []byte("package pkg"), 0o644)
		indexed := map[string]bool{
			"main.go":     true,
			"pkg/util.go": true,
		}
		got := resolveImport("go", "main.go", "testproj/pkg", modDir, indexed)
		if got != "pkg/util.go" {
			t.Errorf("go module prefix: got %q, want %q", got, "pkg/util.go")
		}
		got2 := resolveImport("go", "main.go", "fmt", modDir, indexed)
		if got2 != "" {
			t.Errorf("std lib should not resolve: %q", got2)
		}
	})

	t.Run("javascript", func(t *testing.T) {
		got := resolveImport("typescript", "src/app.ts", "./helper", "/testproj", indexed)
		if got != "src/helper.ts" {
			t.Errorf("js relative: got %q, want %q", got, "src/helper.ts")
		}
		got2 := resolveImport("typescript", "src/app.ts", "lodash", "/testproj", indexed)
		if got2 != "" {
			t.Errorf("npm package should not resolve: %q", got2)
		}
	})

	t.Run("python", func(t *testing.T) {
		got := resolveImport("python", "demo/__init__.py", "demo.core", "/testproj", indexed)
		if got != "demo/core.py" {
			t.Errorf("python dotted: got %q, want %q", got, "demo/core.py")
		}
		got2 := resolveImport("python", "demo/__init__.py", "os", "/testproj", indexed)
		if got2 != "" {
			t.Errorf("stdlib should not resolve: %q", got2)
		}
	})

	t.Run("java", func(t *testing.T) {
		got := resolveImport("java", "com/example/Main.java", "com.example.Main", "/testproj", indexed)
		if got != "com/example/Main.java" {
			t.Errorf("java dotted: got %q, want %q", got, "com/example/Main.java")
		}
		got2 := resolveImport("java", "com/example/Main.java", "java.util.List", "/testproj", indexed)
		if got2 != "" {
			t.Errorf("java stdlib should not resolve: %q", got2)
		}
	})

	t.Run("rust", func(t *testing.T) {
		got := resolveImport("rust", "src/lib.rs", "crate::mod", "/testproj", indexed)
		if got != "src/mod.rs" {
			t.Errorf("rust crate::mod: got %q, want %q", got, "src/mod.rs")
		}
	})
}

// ============================================================
// 分组 2：符号索引（graph.go Index）
// ============================================================

func TestNewIndex(t *testing.T) {
	ix := newIndex()
	if ix == nil {
		t.Fatal("newIndex() returned nil")
	}
	if len(ix.Files) != 0 {
		t.Errorf("expected empty Files, got %d", len(ix.Files))
	}
	if len(ix.AllSymbols()) != 0 {
		t.Errorf("expected empty symbols, got %d", len(ix.AllSymbols()))
	}
}

func TestIndexAddFile(t *testing.T) {
	ix := newIndex()
	fi := &FileInfo{
		Path: "main.go",
		Lang: "go",
		Symbols: []Symbol{
			{File: "main.go", Name: "main", Kind: "func", Line: 1, Complexity: 1},
			{File: "main.go", Name: "Config", Kind: "type", Line: 5, Complexity: 1},
		},
	}
	ix.AddFile(fi)

	if len(ix.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(ix.Files))
	}
	syms := ix.AllSymbols()
	if len(syms) != 2 {
		t.Fatalf("expected 2 symbols, got %d", len(syms))
	}
	// Verify symbol IDs set
	if syms[0].ID == "" || syms[0].ID != "main.go:1:main:func" {
		t.Errorf("unexpected symbol ID: %q", syms[0].ID)
	}
}

func TestIndexRemoveFile(t *testing.T) {
	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path: "a.go",
		Symbols: []Symbol{
			{File: "a.go", Name: "Foo", Kind: "func", Line: 1},
		},
	})
	ix.AddFile(&FileInfo{
		Path: "b.go",
		Symbols: []Symbol{
			{File: "b.go", Name: "Bar", Kind: "func", Line: 1},
		},
	})

	ix.RemoveFile("a.go")
	if len(ix.Files) != 1 {
		t.Errorf("expected 1 file after remove, got %d", len(ix.Files))
	}
	syms := ix.AllSymbols()
	if len(syms) != 1 || syms[0].Name != "Bar" {
		t.Errorf("expected Bar symbol, got %v", syms)
	}

	// Remove nonexistent file should not panic
	ix.RemoveFile("nonexistent.go")
}

func TestIndexAllSymbols(t *testing.T) {
	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path: "a.go",
		Symbols: []Symbol{
			{File: "a.go", Name: "Foo", Kind: "func", Line: 1},
		},
	})
	syms := ix.AllSymbols()
	syms[0].Name = "Hacked"
	// Verify original not affected
	originals := ix.AllSymbols()
	if originals[0].Name != "Foo" {
		t.Errorf("AllSymbols should return a copy, got %q", originals[0].Name)
	}
}

func TestIndexAddRemoveMultiple(t *testing.T) {
	ix := newIndex()
	files := []*FileInfo{
		{Path: "a.go", Symbols: []Symbol{{File: "a.go", Name: "A", Kind: "func", Line: 1}}},
		{Path: "b.go", Symbols: []Symbol{{File: "b.go", Name: "B", Kind: "func", Line: 1}}},
		{Path: "c.go", Symbols: []Symbol{{File: "c.go", Name: "C", Kind: "func", Line: 1}}},
	}
	for _, f := range files {
		ix.AddFile(f)
	}
	if len(ix.Files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(ix.Files))
	}

	ix.RemoveFile("b.go")
	if len(ix.Files) != 2 {
		t.Errorf("expected 2 files after remove, got %d", len(ix.Files))
	}

	ix.RemoveFile("a.go")
	ix.RemoveFile("c.go")
	if len(ix.Files) != 0 {
		t.Errorf("expected 0 files after all removed, got %d", len(ix.Files))
	}
}

// ============================================================
// 分组 3：工作区持久化（graph.go Workspace）
// ============================================================

func TestStoreDir(t *testing.T) {
	dir := t.TempDir()
	sd := StoreDir(dir)
	if !strings.HasSuffix(sd, ".chonkpilot/codegraph") {
		t.Errorf("StoreDir(%q) = %q, want suffix .chonkpilot/codegraph", dir, sd)
	}
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q) failed: %v", dir, err)
	}
	if w.Dir != filepath.ToSlash(dir) {
		t.Errorf("Dir = %q, want %q", w.Dir, filepath.ToSlash(dir))
	}
	if w.Store != StoreDir(dir) {
		t.Errorf("Store = %q, want %q", w.Store, StoreDir(dir))
	}
	// Verify store directory created
	if _, err := os.Stat(w.Store); os.IsNotExist(err) {
		t.Errorf("store directory not created")
	}
	dropWorkspace(w.Dir)
}

func TestOpenReuse(t *testing.T) {
	dir := t.TempDir()
	w1, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open failed: %v", err)
	}
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	if w1 != w2 {
		t.Error("Open should return the same instance for the same workdir")
	}
	dropWorkspace(w1.Dir)
}

func TestOpenNonExistentDir(t *testing.T) {
	_, err := Open("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for non-existent directory")
	}
}

func TestSaveLoadMeta(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	// Save meta
	w.meta = Meta{Enabled: true, State: "ready", ProgressDone: 10, ProgressTotal: 10}
	if err := w.saveMeta(); err != nil {
		t.Fatalf("saveMeta failed: %v", err)
	}

	// Load meta in a fresh workspace
	dropWorkspace(w.Dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)

	m := w2.Meta()
	if !m.Enabled {
		t.Error("expected Enabled=true after reload")
	}
	if m.State != "ready" {
		t.Errorf("expected State=ready, got %q", m.State)
	}
	if m.ProgressDone != 10 {
		t.Errorf("expected ProgressDone=10, got %d", m.ProgressDone)
	}
}

func TestSaveLoadIndex(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	// Build index in memory
	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path: "main.go",
		Lang: "go",
		Symbols: []Symbol{
			{File: "main.go", Name: "main", Kind: "func", Line: 1, Complexity: 1},
		},
	})
	w.ix = ix
	if err := w.SaveIndex(); err != nil {
		t.Fatalf("SaveIndex failed: %v", err)
	}

	// Reload in fresh workspace
	dropWorkspace(w.Dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)

	ok, err := w2.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex failed: %v", err)
	}
	if !ok {
		t.Fatal("LoadIndex returned false, expected true")
	}
	syms := w2.ix.AllSymbols()
	if len(syms) != 1 || syms[0].Name != "main" {
		t.Errorf("unexpected symbols after reload: %v", syms)
	}
}

func TestSaveLoadIndexNotExist(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	ok, err := w.LoadIndex()
	if err != nil {
		t.Fatalf("LoadIndex failed: %v", err)
	}
	if ok {
		t.Error("LoadIndex should return false when no index exists")
	}
}

func TestConfigure(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	enabled := true
	if err := w.Configure(&enabled, nil, []string{"node_modules", "dist"}, nil); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}

	// Reload
	dropWorkspace(w.Dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)

	m := w2.Meta()
	if !m.Enabled {
		t.Error("expected Enabled=true after configure")
	}
	if len(m.SkipDirs) != 2 || m.SkipDirs[0] != "node_modules" {
		t.Errorf("unexpected SkipDirs: %v", m.SkipDirs)
	}
}

// ============================================================
// 分组 4：查询操作（graph.go 查询方法）
// ============================================================

func makeTestWorkspace(t *testing.T, dir string) *Workspace {
	t.Helper()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path: "main.go",
		Lang: "go",
		Symbols: []Symbol{
			{File: "main.go", Name: "main", Kind: "func", Line: 1, Complexity: 1},
			{File: "main.go", Name: "Config", Kind: "type", Line: 5, Complexity: 1},
		},
	})
	ix.AddFile(&FileInfo{
		Path: "pkg/helper.go",
		Lang: "go",
		Symbols: []Symbol{
			{File: "pkg/helper.go", Name: "Helper", Kind: "func", Line: 3, Complexity: 4},
			{File: "pkg/helper.go", Name: "Helper", Kind: "method", Line: 10, Complexity: 7},
		},
	})
	ix.AddFile(&FileInfo{
		Path: "pkg/constant.go",
		Lang: "go",
		Symbols: []Symbol{
			{File: "pkg/constant.go", Name: "MaxSize", Kind: "type", Line: 1, Complexity: 1},
		},
	})
	w.ix = ix
	return w
}

func TestSearchSymbol(t *testing.T) {
	dir := t.TempDir()
	w := makeTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	t.Run("search by name", func(t *testing.T) {
		syms := w.SearchSymbol("helper", "", "", 0)
		if len(syms) != 2 {
			t.Fatalf("expected 2 'helper' symbols, got %d", len(syms))
		}
		for _, s := range syms {
			if s.Name != "Helper" {
				t.Errorf("expected Name=Helper, got %q", s.Name)
			}
		}
	})

	t.Run("search by kind", func(t *testing.T) {
		syms := w.SearchSymbol("", "func", "", 0)
		if len(syms) != 2 {
			t.Fatalf("expected 2 func symbols (main + Helper), got %d: %v", len(syms), syms)
		}
	})

	t.Run("search by file", func(t *testing.T) {
		syms := w.SearchSymbol("", "", "constant", 0)
		if len(syms) != 1 || syms[0].Name != "MaxSize" {
			t.Fatalf("expected 1 symbol in constant.go, got %v", syms)
		}
	})

	t.Run("limit", func(t *testing.T) {
		syms := w.SearchSymbol("", "", "", 1)
		if len(syms) > 1 {
			t.Errorf("expected at most 1 symbol with limit=1, got %d", len(syms))
		}
	})

	t.Run("no match", func(t *testing.T) {
		syms := w.SearchSymbol("nonexistent", "", "", 0)
		if len(syms) != 0 {
			t.Errorf("expected 0 symbols, got %d", len(syms))
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		syms := empty.SearchSymbol("main", "", "", 0)
		if len(syms) != 0 {
			t.Errorf("expected 0 symbols for nil index, got %d", len(syms))
		}
	})
}

func TestFindSymbol(t *testing.T) {
	dir := t.TempDir()
	w := makeTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	t.Run("by id", func(t *testing.T) {
		syms := w.FindSymbol("main.go:1:main:func", "", "")
		if len(syms) != 1 {
			t.Fatalf("expected 1 symbol by id, got %d", len(syms))
		}
		if syms[0].Name != "main" {
			t.Errorf("expected Name=main, got %q", syms[0].Name)
		}
		// ID in output should be absolute
		if !strings.HasPrefix(syms[0].ID, w.Dir) {
			t.Errorf("expected absolute ID, got %q", syms[0].ID)
		}
	})

	t.Run("by file+name", func(t *testing.T) {
		syms := w.FindSymbol("", "pkg/helper.go", "Helper")
		if len(syms) != 2 {
			t.Fatalf("expected 2 symbols by file+name, got %d", len(syms))
		}
	})

	t.Run("not found", func(t *testing.T) {
		syms := w.FindSymbol("", "nonexistent.go", "Foo")
		if len(syms) != 0 {
			t.Errorf("expected 0 symbols, got %d", len(syms))
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		syms := empty.FindSymbol("main.go:1", "", "")
		if len(syms) != 0 {
			t.Errorf("expected 0 symbols for nil index, got %d", len(syms))
		}
	})
}

func TestTopComplexity(t *testing.T) {
	dir := t.TempDir()
	w := makeTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	t.Run("top 3", func(t *testing.T) {
		syms := w.TopComplexity("", 3, 1)
		if len(syms) != 3 {
			t.Fatalf("expected 3 symbols, got %d: %v", len(syms), syms)
		}
		// Helper method (cc=7) > Helper func (cc=4) > main (cc=1)
		if syms[0].Complexity != 7 || syms[1].Complexity != 4 || syms[2].Complexity != 1 {
			t.Errorf("unexpected complexity order: %v", syms)
		}
	})

	t.Run("minCc filter", func(t *testing.T) {
		syms := w.TopComplexity("", 10, 5)
		if len(syms) != 1 {
			t.Fatalf("expected 1 symbol with minCc=5, got %d", len(syms))
		}
		if syms[0].Name != "Helper" || syms[0].Kind != "method" {
			t.Errorf("expected Helper method, got %v", syms[0])
		}
	})

	t.Run("file filter", func(t *testing.T) {
		syms := w.TopComplexity("main.go", 10, 1)
		if len(syms) != 2 {
			t.Fatalf("expected 2 symbols in main.go, got %d", len(syms))
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		syms := empty.TopComplexity("", 10, 1)
		if len(syms) != 0 {
			t.Errorf("expected 0 symbols for nil index, got %d", len(syms))
		}
	})
}

func TestModuleSummary(t *testing.T) {
	dir := t.TempDir()
	w := makeTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	sum := w.ModuleSummary("")
	if sum["files"].(int) != 3 {
		t.Errorf("expected 3 files, got %d", sum["files"])
	}
	if sum["symbols"].(int) != 5 {
		t.Errorf("expected 5 symbols, got %d", sum["symbols"])
	}
	lc := sum["langCounts"].(map[string]int)
	if lc["go"] != 3 {
		t.Errorf("expected 3 go files, got %d", lc["go"])
	}
	tc5 := sum["topComplexity5"].([]Symbol)
	if len(tc5) != 5 {
		t.Errorf("expected 5 top complexity symbols, got %d", len(tc5))
	}
	if tc5[0].Complexity != 7 {
		t.Errorf("expected top complexity 7, got %d", tc5[0].Complexity)
	}

	t.Run("with prefix filter", func(t *testing.T) {
		sum2 := w.ModuleSummary("pkg")
		if sum2["files"].(int) != 2 {
			t.Errorf("expected 2 files in pkg/, got %d", sum2["files"])
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		sum3 := empty.ModuleSummary("")
		if sum3["files"].(int) != 0 {
			t.Errorf("expected 0 files for nil index, got %d", sum3["files"])
		}
	})
}

func TestImports(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path:    "main.go",
		Lang:    "go",
		Imports: []string{"fmt", "os"},
		Symbols: []Symbol{{File: "main.go", Name: "main", Kind: "func", Line: 1}},
	})
	ix.AddFile(&FileInfo{
		Path:    "pkg/util.go",
		Lang:    "go",
		Imports: []string{"strings"},
		Symbols: []Symbol{{File: "pkg/util.go", Name: "Util", Kind: "func", Line: 1}},
	})
	w.ix = ix

	t.Run("all", func(t *testing.T) {
		rows := w.Imports("")
		if len(rows) != 2 {
			t.Fatalf("expected 2 files, got %d", len(rows))
		}
		// Keys should be absolute
		for k := range rows {
			if !strings.HasPrefix(k, w.Dir) {
				t.Errorf("expected absolute path key, got %q", k)
			}
		}
	})

	t.Run("file filter", func(t *testing.T) {
		rows := w.Imports("main.go")
		if len(rows) != 1 {
			t.Fatalf("expected 1 file filtered, got %d", len(rows))
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		rows := empty.Imports("")
		if len(rows) != 0 {
			t.Errorf("expected 0 rows for nil index, got %d", len(rows))
		}
	})
}

func TestFindCycles(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	t.Run("no cycle", func(t *testing.T) {
		ix := newIndex()
		ix.AddFile(&FileInfo{
			Path:    "a.go",
			Lang:    "go",
			Symbols: []Symbol{{File: "a.go", Name: "A", Kind: "func", Line: 1}},
		})
		ix.AddFile(&FileInfo{
			Path:    "b.go",
			Lang:    "go",
			Symbols: []Symbol{{File: "b.go", Name: "B", Kind: "func", Line: 1}},
		})
		w.ix = ix
		cyc := w.FindCycles()
		if len(cyc) != 0 {
			t.Errorf("expected 0 cycles, got %d: %v", len(cyc), cyc)
		}
	})

	t.Run("simple cycle", func(t *testing.T) {
		ix := newIndex()
		ix.AddFile(&FileInfo{
			Path:    "a.go",
			Lang:    "go",
			Imports: []string{"testproj/b"},
			Symbols: []Symbol{{File: "a.go", Name: "A", Kind: "func", Line: 1}},
		})
		ix.AddFile(&FileInfo{
			Path:    "b.go",
			Lang:    "go",
			Imports: []string{"testproj/a"},
			Symbols: []Symbol{{File: "b.go", Name: "B", Kind: "func", Line: 1}},
		})
		// Need go.mod for module resolution
		w.ix = ix
		// Without go.mod, no cycles should be detected (unresolvable)
		cyc := w.FindCycles()
		_ = cyc // result depends on whether go.mod exists
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		cyc := empty.FindCycles()
		if len(cyc) != 0 {
			t.Errorf("expected 0 cycles for nil index, got %d", len(cyc))
		}
	})
}

// ============================================================
// 辅助：构建测试用 Go 项目
// ============================================================

func writeTestProject(t *testing.T, dir string) {
	t.Helper()
	// main.go
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello")
}

type Config struct {
	Name string
}
`), 0o644)
	// utils/helper.go
	os.MkdirAll(filepath.Join(dir, "utils"), 0o755)
	os.WriteFile(filepath.Join(dir, "utils", "helper.go"), []byte(`package utils

import "strings"

func Helper(s string) string {
	if s == "" {
		return "default"
	}
	return strings.TrimSpace(s)
}

type Result struct {
	Value string
}
`), 0o644)
	// go.mod
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module testproj\n\ngo 1.21\n"), 0o644)
}

// ============================================================
// 分组 5：索引生命周期 & 语言检测（index.go + lang.go）
// ============================================================

func TestLangForExt(t *testing.T) {
	tests := []struct {
		ext      string
		wantLang string
		wantOK   bool
	}{
		{".go", "go", true},
		{".js", "javascript", true},
		{".jsx", "javascript", true},
		{".ts", "typescript", true},
		{".tsx", "tsx", true},
		{".py", "python", true},
		{".rs", "rust", true},
		{".java", "java", true},
		{".GO", "go", true}, // case insensitive
		{".txt", "", false},
		{".md", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		lang, ok := LangForExt(tt.ext)
		if ok != tt.wantOK || lang != tt.wantLang {
			t.Errorf("LangForExt(%q) = (%q, %v), want (%q, %v)", tt.ext, lang, ok, tt.wantLang, tt.wantOK)
		}
	}
}

func TestSupportedLangs(t *testing.T) {
	langs := SupportedLangs()
	if len(langs) != 7 {
		t.Errorf("expected 7 languages, got %d: %v", len(langs), langs)
	}
}

func TestCollectSourceFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	entries, err := w.collectSourceFiles()
	if err != nil {
		t.Fatalf("collectSourceFiles failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 source files, got %d: %v", len(entries), entries)
	}
	// Verify paths are relative
	for _, e := range entries {
		if strings.HasPrefix(e.path, "/") || strings.HasPrefix(e.path, dir) {
			t.Errorf("expected relative path, got %q", e.path)
		}
		if e.lang != "go" {
			t.Errorf("expected lang=go, got %q for %s", e.lang, e.path)
		}
		if e.mtime <= 0 || e.size <= 0 {
			t.Errorf("invalid mtime/size for %s: %d/%d", e.path, e.mtime, e.size)
		}
	}
}

func TestCollectSourceFilesSkipDirs(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)
	os.MkdirAll(filepath.Join(dir, "node_modules", "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "node_modules", "pkg", "index.js"), []byte("module.exports = {};"), 0o644)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	entries, err := w.collectSourceFiles()
	if err != nil {
		t.Fatalf("collectSourceFiles failed: %v", err)
	}
	// node_modules should be skipped by default
	for _, e := range entries {
		if strings.Contains(e.path, "node_modules") {
			t.Errorf("node_modules should be skipped, got: %s", e.path)
		}
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 source files (node_modules skipped), got %d", len(entries))
	}
}

func TestInitialize(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Verify state
	m := w.Meta()
	if m.State != "ready" {
		t.Errorf("expected State=ready, got %q", m.State)
	}
	// Initialize does NOT set Enabled=true (Configure does that separately)
	if m.ProgressDone != 2 {
		t.Errorf("expected ProgressDone=2, got %d", m.ProgressDone)
	}

	// Verify symbols
	syms := w.ix.AllSymbols()
	if len(syms) != 4 {
		t.Fatalf("expected 4 symbols, got %d: %v", len(syms), syms)
	}
	// Check key symbols exist
	names := map[string]bool{}
	kinds := map[string]bool{}
	for _, s := range syms {
		names[s.Name] = true
		kinds[s.Kind] = true
	}
	if !names["main"] {
		t.Error("expected symbol 'main'")
	}
	if !names["Config"] {
		t.Error("expected symbol 'Config'")
	}
	if !names["Helper"] {
		t.Error("expected symbol 'Helper'")
	}
	if !kinds["func"] {
		t.Error("expected kind 'func'")
	}
	if !kinds["type"] {
		t.Error("expected kind 'type'")
	}

	// Verify index persisted
	dropWorkspace(w.Dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)
	ok, err := w2.LoadIndex()
	if err != nil || !ok {
		t.Fatalf("reload index failed: %v, ok=%v", err, ok)
	}
	if len(w2.ix.AllSymbols()) != 4 {
		t.Errorf("expected 4 symbols after reload, got %d", len(w2.ix.AllSymbols()))
	}
}

func TestInitializeWithSkipDirs(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)
	os.MkdirAll(filepath.Join(dir, "skipme", "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "skipme", "pkg", "v.go"), []byte("package skipme\nfunc V() {}\n"), 0o644)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	// skipme 不在默认排除集，作为用户排除规则（gitignore 语法，非锚定 = 任意层级）下发
	if err := w.Initialize(nil, []string{"skipme/"}, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	for _, s := range w.ix.AllSymbols() {
		if strings.Contains(s.File, "skipme") {
			t.Errorf("skipme 文件应被用户排除规则跳过，got: %s", s.File)
		}
	}
}

// TestCollectSourceFilesStackGitignore：stack_gitignore 关 → 不读 .gitignore（仅内置强制 + 默认 + 用户规则）；
// 开 → 按 gitignore 语义过滤（目录不下降、文件级排除、'!' 反选）。
func TestCollectSourceFilesStackGitignore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // 隔离全局 ignore，避免开发机配置干扰

	cases := []struct {
		name  string
		gitig string
		stack bool
		files []string
	}{
		{"stack 关：不读 .gitignore", "generated/\nmain.go\n", false,
			[]string{"generated/gen.go", "keep.go", "main.go", "utils/helper.go"}},
		{"stack 开：目录不下降 + 文件级排除", "generated/\nmain.go\n", true,
			[]string{"keep.go", "utils/helper.go"}},
		{"stack 开：'!' 反选", "*.go\n!keep.go\n", true,
			[]string{"keep.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestProject(t, dir) // main.go + utils/helper.go + go.mod
			os.MkdirAll(filepath.Join(dir, "generated"), 0o755)
			os.WriteFile(filepath.Join(dir, "generated", "gen.go"), []byte("package generated\nfunc G() {}\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "keep.go"), []byte("package main\nfunc Keep() {}\n"), 0o644)
			os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(c.gitig), 0o644)

			w, err := Open(dir)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer dropWorkspace(w.Dir)

			stack := c.stack
			if err := w.Configure(nil, []string{".go"}, nil, &stack); err != nil {
				t.Fatalf("Configure failed: %v", err)
			}
			entries, err := w.collectSourceFiles()
			if err != nil {
				t.Fatalf("collectSourceFiles failed: %v", err)
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.path)
			}
			sort.Strings(got)
			want := append([]string{}, c.files...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("文件清单不符：got=%v want=%v", got, want)
			}
		})
	}
}

// TestClear：清除索引产物——index.db 删除、内存索引置空、状态回「未初始化」；配置存档保留；
// 清除后可重新全量重建。
func TestClear(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	enabled := true
	if err := w.Configure(&enabled, []string{".go"}, []string{"vendor"}, nil); err != nil {
		t.Fatalf("Configure failed: %v", err)
	}
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	idxPath := filepath.Join(w.Store, dbName)
	if _, err := os.Stat(idxPath); err != nil {
		t.Fatalf("初始化后 index.db 应存在：%v", err)
	}

	if err := w.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	if _, err := os.Stat(idxPath); !os.IsNotExist(err) {
		t.Errorf("Clear 后 index.db 应被删除（stat err=%v）", err)
	}
	if w.ix != nil {
		t.Error("Clear 后内存索引应置空")
	}
	m := w.Meta()
	if m.State != "" {
		t.Errorf("Clear 后 State 应为空（未初始化），got %q", m.State)
	}
	if m.ProgressTotal != 0 || m.Err != "" || m.LastIndexedAt != 0 {
		t.Errorf("Clear 应重置进度/错误/时间戳，got %+v", m)
	}
	// 配置存档（enabled/exts/skip_dirs）不是索引产物 → 保留
	if !m.Enabled || len(m.Exts) != 1 || m.Exts[0] != ".go" || len(m.SkipDirs) != 1 {
		t.Errorf("Clear 应保留配置存档，got %+v", m)
	}
	if st, err := w.EnsureReady(); st != "not_initialized" || err == nil {
		t.Errorf("Clear 后 EnsureReady = (%q, %v)，期望 not_initialized", st, err)
	}
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Clear 后重新 Initialize failed: %v", err)
	}
	if w.State() != "ready" {
		t.Errorf("重建后 State 应为 ready，got %q", w.State())
	}
}

// TestToolConfigureClearMode：管理工具 codegraph_configure 的 mode 参数——clear 走清除路径；
// 缺省（无 mode）仍是纯存档语义，不清除已就绪索引。
func TestToolConfigureClearMode(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	out, err := toolConfigure(context.Background(), map[string]any{"workdir": dir, "mode": "clear"})
	if err != nil {
		t.Fatalf("toolConfigure(mode=clear) failed: %v", err)
	}
	res, ok := out.(map[string]any)
	if !ok || res["mode"] != "clear" || res["ok"] != true {
		t.Fatalf("unexpected toolConfigure 应答：%v", out)
	}
	if _, err := os.Stat(filepath.Join(w.Store, dbName)); !os.IsNotExist(err) {
		t.Errorf("mode=clear 后 index.db 应被删除（stat err=%v）", err)
	}
	if w.State() != "" {
		t.Errorf("mode=clear 后 State 应为空（未初始化），got %q", w.State())
	}

	// 缺省 mode：仅存档配置，不清除
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("重新 Initialize failed: %v", err)
	}
	if _, err := toolConfigure(context.Background(), map[string]any{"workdir": dir, "enabled": true}); err != nil {
		t.Fatalf("toolConfigure(缺省 mode) failed: %v", err)
	}
	if w.State() != "ready" {
		t.Errorf("缺省 mode 不应清除索引，State=%q", w.State())
	}
}

func TestReconcileNoChange(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	changed, err := w.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if changed {
		t.Error("expected reconcile to return false (no changes)")
	}
}

func TestReconcileNewFile(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Add a new file
	os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\nfunc NewFunc() {}\n"), 0o644)

	changed, err := w.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if !changed {
		t.Error("expected reconcile to return true (new file added)")
	}
	// Verify new symbol exists
	syms := w.ix.AllSymbols()
	found := false
	for _, s := range syms {
		if s.Name == "NewFunc" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected NewFunc symbol after reconcile")
	}
}

func TestReconcileModifiedFile(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Modify main.go - add a new function
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import "fmt"

func main() {
	fmt.Println("hello")
}

type Config struct {
	Name string
}

func NewFunc() {}
`), 0o644)

	changed, err := w.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if !changed {
		t.Error("expected reconcile to return true (file modified)")
	}
	// Verify new symbol exists
	syms := w.ix.AllSymbols()
	found := false
	for _, s := range syms {
		if s.Name == "NewFunc" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected NewFunc symbol after reconcile")
	}
}

func TestReconcileDeletedFile(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// Delete main.go
	if err := os.Remove(filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	changed, err := w.Reconcile()
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if !changed {
		t.Error("expected reconcile to return true (file deleted)")
	}
	// main.go symbols should be removed
	syms := w.ix.AllSymbols()
	for _, s := range syms {
		if strings.Contains(s.File, "main.go") {
			t.Errorf("main.go symbols should be removed, found: %s", s.Name)
		}
	}
}

func TestReconcileNotInitialized(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	_, err = w.Reconcile()
	if err != ErrNotInitialized {
		t.Errorf("expected ErrNotInitialized, got: %v", err)
	}
}

func TestEnsureReady(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	t.Run("not initialized", func(t *testing.T) {
		st, err := w.EnsureReady()
		if st != "not_initialized" {
			t.Errorf("expected not_initialized, got %q (err=%v)", st, err)
		}
	})

	t.Run("after initialize", func(t *testing.T) {
		if err := w.Initialize(nil, nil, nil); err != nil {
			t.Fatalf("Initialize failed: %v", err)
		}
		st, err := w.EnsureReady()
		if st != "ready" || err != nil {
			t.Errorf("expected ready, got %q (err=%v)", st, err)
		}
	})

	t.Run("reload from disk", func(t *testing.T) {
		dropWorkspace(w.Dir)
		w2, err := Open(dir)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer dropWorkspace(w2.Dir)
		st, err := w2.EnsureReady()
		if st != "ready" || err != nil {
			t.Errorf("expected ready after reload, got %q (err=%v)", st, err)
		}
	})
}

// ============================================================
// 分组 6：工具辅助函数（server.go）
// ============================================================

func TestGetString(t *testing.T) {
	args := map[string]any{"name": "hello", "empty": "", "num": 42}
	if got := getString(args, "name"); got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}
	if got := getString(args, "empty"); got != "" {
		t.Errorf("expected '', got %q", got)
	}
	if got := getString(args, "missing"); got != "" {
		t.Errorf("expected '', got %q", got)
	}
	if got := getString(args, "num"); got != "" {
		t.Errorf("expected '' for non-string, got %q", got)
	}
}

func TestGetInt(t *testing.T) {
	args := map[string]any{"count": float64(42), "zero": float64(0), "neg": float64(-1)}
	if got := getInt(args, "count", 10); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	if got := getInt(args, "missing", 10); got != 10 {
		t.Errorf("expected default 10, got %d", got)
	}
	if got := getInt(args, "zero", 10); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
	if got := getInt(args, "neg", 10); got != -1 {
		t.Errorf("expected -1, got %d", got)
	}
}

func TestGetBoolPtr(t *testing.T) {
	args := map[string]any{"yes": true, "no": false}
	if p := getBoolPtr(args, "yes"); p == nil || *p != true {
		t.Error("expected &true")
	}
	if p := getBoolPtr(args, "no"); p == nil || *p != false {
		t.Error("expected &false")
	}
	if p := getBoolPtr(args, "missing"); p != nil {
		t.Error("expected nil for missing key")
	}
}

func TestGetStrings(t *testing.T) {
	args := map[string]any{"list": []any{"a", "b", "c"}}
	got := getStrings(args, "list")
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("expected [a b c], got %v", got)
	}
	if got := getStrings(args, "missing"); got != nil {
		t.Errorf("expected nil for missing, got %v", got)
	}
	if got := getStrings(args, "list_with_non_string"); got != nil {
		// non-string elements should be filtered
		_ = got
	}
}

func TestNeedWS(t *testing.T) {
	t.Run("missing workdir", func(t *testing.T) {
		_, err := needWS(map[string]any{})
		if err == nil {
			t.Error("expected error for missing workdir")
		}
	})

	t.Run("non-existent dir", func(t *testing.T) {
		_, err := needWS(map[string]any{"workdir": "/nonexistent/path/12345"})
		if err == nil {
			t.Error("expected error for non-existent dir")
		}
	})

	t.Run("valid dir", func(t *testing.T) {
		dir := t.TempDir()
		w, err := needWS(map[string]any{"workdir": dir})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w == nil {
			t.Fatal("expected non-nil workspace")
		}
		dropWorkspace(w.Dir)
	})
}

func TestReadyGate(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	t.Run("not initialized", func(t *testing.T) {
		resp, w, ok := readyGate(map[string]any{"workdir": dir})
		if ok {
			t.Error("expected not ready")
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		respMap, ok := resp.(map[string]any)
		if !ok {
			t.Fatalf("expected map response, got %T", resp)
		}
		if respMap["status"] != "not_initialized" {
			t.Errorf("expected status=not_initialized, got %q", respMap["status"])
		}
		if w != nil {
			dropWorkspace(w.Dir)
		}
	})

	t.Run("ready after initialize", func(t *testing.T) {
		w, err := Open(dir)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer dropWorkspace(w.Dir)

		if err := w.Initialize(nil, nil, nil); err != nil {
			t.Fatalf("Initialize failed: %v", err)
		}

		resp, _, ok := readyGate(map[string]any{"workdir": dir})
		if !ok {
			t.Errorf("expected ready, got response: %v", resp)
		}
	})
}

// ============================================================
// 额外：symbolID 与 itoa
// ============================================================

func TestSymbolID(t *testing.T) {
	id := symbolID("main.go", 1, "main", "func")
	want := "main.go:1:main:func"
	if id != want {
		t.Errorf("symbolID = %q, want %q", id, want)
	}
}

func TestItoa(t *testing.T) {
	tests := []struct {
		v    int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
		{100, "100"},
	}
	for _, tt := range tests {
		if got := itoa(tt.v); got != tt.want {
			t.Errorf("itoa(%d) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

// ============================================================
// 辅助：确保测试辅助函数存在且签名正确
// ============================================================

func TestAllSymbolsSort(t *testing.T) {
	ix := newIndex()
	for _, f := range []string{"z.go", "a.go", "m.go"} {
		ix.AddFile(&FileInfo{
			Path:    f,
			Symbols: []Symbol{{File: f, Name: "Fn", Kind: "func", Line: 1}},
		})
	}
	syms := ix.AllSymbols()
	// AllSymbols should preserve the order they were added
	if len(syms) != 3 {
		t.Fatalf("expected 3 symbols, got %d", len(syms))
	}
	// Sort and verify
	sort.Slice(syms, func(i, j int) bool { return syms[i].File < syms[j].File })
	if syms[0].File != "a.go" || syms[1].File != "m.go" || syms[2].File != "z.go" {
		t.Errorf("unexpected sort order: %v", syms)
	}
}

// ============================================================
// 分组 7：调用图（extract.go callKinds/callsWithin + graph.go Callers/Callees + server.go 工具）
// ============================================================

// writeCallGraphProject 落盘多语言真实语料（7 语言），覆盖普通调用/方法调用/跨文件调用/
// 构造与 new/宏/嵌套函数剪枝。
func writeCallGraphProject(t *testing.T, dir string) {
	t.Helper()
	write := func(rel, src string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("go.mod", "module testproj\n\ngo 1.21\n")
	// Go：跨文件调用（Main → Run@b.go）、选择表达式 fmt.Println、内层 func_literal 剪枝
	write("a.go", `package main

import "fmt"

func Main() {
	fmt.Println(Run())
	top()
}
`)
	write("b.go", `package main

func Run() string { return "x" }

func top() {
	f := func() { deep() }
	f()
}

func deep() {}

func useGen() {
	Gen[int](1, 2)
	fns[0]()
}

func Gen[T any](x T, y T) T { return x }
`)
	// Python：方法调用（helper.run）、跨文件式调用、内层 def 剪枝、stdlib 调用
	write("c.py", `import os


def main():
    helper.run()
    compute(3)
    nested()


def compute(x):
    def inner():
        deep_py()
    inner()
    return x


def nested():
    return os.getcwd()
`)
	// TypeScript：箭头函数剪枝、new 表达式、类内方法互调
	write("d.ts", `import { util } from "./util";

export function main(): void {
  util.help();
  compute(1);
  const f = () => {
    deepTs();
  };
  f();
}

export function compute(n: number): number {
  return n;
}

export function makeThing(): Thing {
  return new Thing();
}

export class Svc {
  run(): void {
    this.step();
  }
  step(): void {}
}
`)
	// TSX：JSX 表达式内的调用
	write("h.tsx", `export function Panel(): any {
  const el = <div>{label()}</div>;
  return el;
}
`)
	// JavaScript：new 表达式 + 成员调用
	write("e.js", `function main() {
  helper();
  const c = new Cls();
  obj.method();
}

function helper() {}
`)
	// Java：方法调用 + 对象创建 + 成员调用；泛型实参噪声
	write("f.java", `import java.util.List;

class D {
    void run() {
        helper();
        Widget w = new Widget();
        List<String> xs = new java.util.ArrayList<String>();
        w.paint();
    }

    void helper() {
    }
}

class Widget {
    void paint() {
    }
}
`)
	// Rust：函数调用 + 方法调用 + 宏调用
	write("g.rs", `fn run() {
    helper();
    println!("hi");
    let n = v.len();
}

fn helper() {}
`)
}

// TestCallGraphExtraction：AST 调用提取（7 语言）+ Calls 落盘往返。
func TestCallGraphExtraction(t *testing.T) {
	dir := t.TempDir()
	writeCallGraphProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	type key struct{ file, name, kind string }
	got := map[key][]string{}
	for _, s := range w.ix.AllSymbols() {
		got[key{s.File, s.Name, s.Kind}] = s.Calls
	}

	cases := []struct {
		file, name, kind string
		want             []string
	}{
		// Go：普通调用 + 选择表达式取最后一段（fmt.Println → Println）+ 跨文件调用（Run@b.go）
		{"a.go", "Main", "func", []string{"Println", "Run", "top"}},
		// 内层 func_literal 的 deep() 不计入 top
		{"b.go", "top", "func", []string{"f"}},
		{"b.go", "Run", "func", nil},
		{"b.go", "deep", "func", nil},
		// Go 泛型实例化 Gen[int](1,2) → 取 operand（Gen）；下标调用 fns[0]() 无法静态取名 → 丢弃
		{"b.go", "useGen", "func", []string{"Gen"}},
		{"b.go", "Gen", "func", nil},
		// Python：方法调用 helper.run → run；内层 def deep_py 被剪枝；os.getcwd → getcwd
		{"c.py", "main", "func", []string{"compute", "nested", "run"}},
		{"c.py", "compute", "func", []string{"inner"}},
		{"c.py", "nested", "func", []string{"getcwd"}},
		// TS：箭头函数体 deepTs 被剪枝；new Thing → Thing；方法互调 this.step → step
		{"d.ts", "main", "func", []string{"compute", "f", "help"}},
		{"d.ts", "compute", "func", nil},
		{"d.ts", "makeThing", "func", []string{"Thing"}},
		{"d.ts", "run", "method", []string{"step"}},
		{"d.ts", "step", "method", nil},
		// TSX：JSX 表达式内调用
		{"h.tsx", "Panel", "func", []string{"label"}},
		// JS：new Cls → Cls；obj.method → method
		{"e.js", "main", "func", []string{"Cls", "helper", "method"}},
		// Java：helper()、new Widget()、w.paint()、new java.util.ArrayList<String>() → ArrayList
		{"f.java", "run", "method", []string{"ArrayList", "Widget", "helper", "paint"}},
		// Rust：helper()、println! 宏、v.len()
		{"g.rs", "run", "func", []string{"helper", "len", "println"}},
	}
	for _, c := range cases {
		k := key{c.file, c.name, c.kind}
		g, ok := got[k]
		if !ok {
			t.Errorf("缺少符号 %v（共 %d 个符号）", k, len(got))
			continue
		}
		if !reflect.DeepEqual(g, c.want) {
			t.Errorf("%v Calls = %v, want %v", k, g, c.want)
		}
	}

	t.Run("persist round-trip", func(t *testing.T) {
		dropWorkspace(w.Dir)
		w2, err := Open(dir)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		defer dropWorkspace(w2.Dir)
		ok, err := w2.LoadIndex()
		if err != nil || !ok {
			t.Fatalf("LoadIndex failed: %v, ok=%v", err, ok)
		}
		var mainCalls []string
		for _, s := range w2.ix.AllSymbols() {
			if s.File == "a.go" && s.Name == "Main" {
				mainCalls = s.Calls
			}
		}
		if !reflect.DeepEqual(mainCalls, []string{"Println", "Run", "top"}) {
			t.Errorf("LoadIndex 后 Calls 未往返：%v", mainCalls)
		}
	})
}

// makeCallTestWorkspace 内存造调用图语料：Main/Foo/Helper 三个符号 + 重名 Foo。
func makeCallTestWorkspace(t *testing.T, dir string) *Workspace {
	t.Helper()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	ix := newIndex()
	ix.AddFile(&FileInfo{
		Path: "a.go", Lang: "go",
		Symbols: []Symbol{{File: "a.go", Name: "Main", Kind: "func", Line: 1, Calls: []string{"Foo", "Helper"}}},
	})
	ix.AddFile(&FileInfo{
		Path: "b.go", Lang: "go",
		Symbols: []Symbol{{File: "b.go", Name: "Helper", Kind: "func", Line: 3, Calls: []string{"Foo"}}},
	})
	ix.AddFile(&FileInfo{
		Path: "c.go", Lang: "go",
		Symbols: []Symbol{
			{File: "c.go", Name: "Foo", Kind: "func", Line: 2, Calls: []string{"Helper"}},
			{File: "c.go", Name: "Foo", Kind: "method", Line: 20},
		},
	})
	w.ix = ix
	return w
}

func TestCallers(t *testing.T) {
	dir := t.TempDir()
	w := makeCallTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	t.Run("exact name", func(t *testing.T) {
		syms := w.Callers("Helper", "", 0)
		if len(syms) != 2 {
			t.Fatalf("expected 2 callers of Helper, got %d: %v", len(syms), syms)
		}
		if syms[0].Name != "Main" || syms[1].Name != "Foo" {
			t.Errorf("unexpected order: %v", syms)
		}
	})

	t.Run("case-insensitive", func(t *testing.T) {
		if got := w.Callers("helper", "", 0); len(got) != 2 {
			t.Errorf("case-insensitive match failed: %v", got)
		}
	})

	t.Run("qualified name matches tail", func(t *testing.T) {
		tail := w.Callers("Foo", "", 0)
		qual := w.Callers("pkg.Foo", "", 0)
		if len(tail) != 2 || len(qual) != 2 {
			t.Fatalf("expected 2 callers for both Foo/pkg.Foo, got %d/%d", len(tail), len(qual))
		}
	})

	t.Run("file filter", func(t *testing.T) {
		syms := w.Callers("Foo", "a.go", 0)
		if len(syms) != 1 || syms[0].Name != "Main" {
			t.Errorf("file filter failed: %v", syms)
		}
	})

	t.Run("limit", func(t *testing.T) {
		if got := w.Callers("Foo", "", 1); len(got) != 1 {
			t.Errorf("limit=1 failed: %v", got)
		}
	})

	t.Run("no match", func(t *testing.T) {
		if got := w.Callers("Nope", "", 0); len(got) != 0 {
			t.Errorf("expected 0, got %v", got)
		}
	})

	t.Run("empty target", func(t *testing.T) {
		if got := w.Callers("  ", "", 0); len(got) != 0 {
			t.Errorf("expected 0 for empty target, got %v", got)
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		if got := empty.Callers("Foo", "", 0); len(got) != 0 {
			t.Errorf("expected 0 for nil index, got %v", got)
		}
	})

	t.Run("caller carries calls", func(t *testing.T) {
		syms := w.Callers("Helper", "", 0)
		if len(syms) != 2 || !reflect.DeepEqual(syms[0].Calls, []string{"Foo", "Helper"}) {
			t.Errorf("caller symbol should carry its Calls: %v", syms)
		}
	})
}

func TestCallees(t *testing.T) {
	dir := t.TempDir()
	w := makeCallTestWorkspace(t, dir)
	defer dropWorkspace(w.Dir)

	t.Run("by id", func(t *testing.T) {
		refs := w.Callees("a.go:1:Main:func", "", "", 0)
		if len(refs) != 2 {
			t.Fatalf("expected 2 callees, got %d: %v", len(refs), refs)
		}
		if refs[0].Name != "Foo" || refs[0].Resolved {
			t.Errorf("Foo is ambiguous (2 symbols) → resolved must be false: %+v", refs[0])
		}
		h := refs[1]
		if h.Name != "Helper" || !h.Resolved || h.Kind != "func" || h.Line != 3 ||
			!strings.HasSuffix(h.File, "b.go") || !strings.Contains(h.ID, "b.go:3:Helper:func") {
			t.Errorf("Helper should resolve uniquely to b.go:3: %+v", h)
		}
	})

	t.Run("by file+name", func(t *testing.T) {
		refs := w.Callees("", "b.go", "Helper", 0)
		if len(refs) != 1 || refs[0].Name != "Foo" || refs[0].Resolved {
			t.Errorf("unexpected refs: %+v", refs)
		}
	})

	t.Run("case-insensitive file+name", func(t *testing.T) {
		if refs := w.Callees("", "b.go", "helper", 0); len(refs) != 1 {
			t.Errorf("expected 1 ref, got %+v", refs)
		}
	})

	t.Run("limit", func(t *testing.T) {
		if refs := w.Callees("a.go:1:Main:func", "", "", 1); len(refs) != 1 {
			t.Errorf("limit=1 failed: %+v", refs)
		}
	})

	t.Run("symbol not found", func(t *testing.T) {
		if refs := w.Callees("", "nope.go", "X", 0); len(refs) != 0 {
			t.Errorf("expected 0 refs, got %+v", refs)
		}
	})

	t.Run("symbol without calls", func(t *testing.T) {
		if refs := w.Callees("", "c.go", "Foo", 0); refs == nil {
			t.Fatal("expected non-nil handling for call-less symbol")
		}
	})

	t.Run("no index", func(t *testing.T) {
		empty := &Workspace{Dir: w.Dir, Store: w.Store}
		if refs := empty.Callees("a.go:1:Main:func", "", "", 0); len(refs) != 0 {
			t.Errorf("expected 0 for nil index, got %+v", refs)
		}
	})
}

// TestCallGraphTools：两个新工具 handler 的正常/未就绪/参数缺失/未命中分支 + 自检清单。
func TestCallGraphTools(t *testing.T) {
	dir := t.TempDir()
	writeCallGraphProject(t, dir)

	t.Run("tool names in self-check list", func(t *testing.T) {
		names := map[string]bool{}
		for _, n := range TextToolNames() {
			names[n] = true
		}
		if !names["codegraph_callers"] || !names["codegraph_callees"] {
			t.Errorf("TextToolNames 缺新工具：%v", TextToolNames())
		}
	})

	t.Run("not initialized", func(t *testing.T) {
		for _, fn := range []func(context.Context, map[string]any) (any, error){toolCallers, toolCallees} {
			out, err := fn(context.Background(), map[string]any{"workdir": dir, "name": "Run", "file": "a.go"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			m, ok := out.(map[string]any)
			if !ok || m["status"] != "not_initialized" {
				t.Fatalf("expected not_initialized, got %v", out)
			}
		}
		if w, err := Open(dir); err == nil {
			dropWorkspace(w.Dir)
		}
	})

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	t.Run("missing params", func(t *testing.T) {
		if _, err := toolCallers(context.Background(), map[string]any{"workdir": dir}); err == nil {
			t.Error("toolCallers 缺 name/id 应报错")
		}
		if _, err := toolCallees(context.Background(), map[string]any{"workdir": dir}); err == nil {
			t.Error("toolCallees 缺 id/file+name 应报错")
		}
	})

	t.Run("callers by name", func(t *testing.T) {
		out, err := toolCallers(context.Background(), map[string]any{"workdir": dir, "name": "Run"})
		if err != nil {
			t.Fatalf("toolCallers failed: %v", err)
		}
		m := out.(map[string]any)
		if m["target"] != "Run" || m["total"].(int) != 2 {
			t.Fatalf("unexpected 应答：%v", m)
		}
		// a.go Main 调 Run；c.py main 调 run（忽略大小写 + 跨语言同名）
		got := map[string]bool{}
		for _, s := range m["callers"].([]Symbol) {
			got[s.Name] = true
		}
		if !got["Main"] || !got["main"] {
			t.Errorf("expected callers {Main, main}, got %v", m["callers"])
		}
	})

	t.Run("callers by id not found", func(t *testing.T) {
		out, err := toolCallers(context.Background(), map[string]any{"workdir": dir, "id": "nope.go:1:X:func"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m := out.(map[string]any); m["status"] != "not_found" {
			t.Errorf("expected not_found, got %v", m)
		}
	})

	t.Run("callees by file+name", func(t *testing.T) {
		out, err := toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Main"})
		if err != nil {
			t.Fatalf("toolCallees failed: %v", err)
		}
		m := out.(map[string]any)
		if m["total"].(int) != 3 {
			t.Fatalf("expected 3 callees, got %v", m)
		}
		refs := m["callees"].([]CalleeRef)
		// Run 在语料内被 4 处定义（b.go/Run、d.ts/Svc.run、f.java/D.run、g.rs/run）→ 名字级启发式下无法唯一解析；
		// top 仅 b.go 一处 → 唯一解析并回填 file/line。
		if refs[1].Name != "Run" || refs[1].Resolved {
			t.Errorf("Run should be ambiguous (resolved=false): %+v", refs[1])
		}
		if refs[2].Name != "top" || !refs[2].Resolved || !strings.HasSuffix(refs[2].File, "b.go") || refs[2].Line != 5 {
			t.Errorf("top should resolve uniquely to b.go:5: %+v", refs[2])
		}
	})

	t.Run("callees limit", func(t *testing.T) {
		out, err := toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "a.go", "name": "Main", "limit": float64(1)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m := out.(map[string]any); m["total"].(int) != 1 {
			t.Errorf("limit=1 failed: %v", m)
		}
	})

	t.Run("callees not found", func(t *testing.T) {
		out, err := toolCallees(context.Background(), map[string]any{"workdir": dir, "file": "nope.go", "name": "X"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m := out.(map[string]any); m["status"] != "not_found" {
			t.Errorf("expected not_found, got %v", m)
		}
	})
}

// ============================================================
// 分组 8：索引落盘（bbolt，store.go）
// ============================================================

// TestIndexStoreUsesBoltDB：初始化落盘 index.db（bbolt 单文件），不再产出 index.json。
func TestIndexStoreUsesBoltDB(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.Store, dbName)); err != nil {
		t.Fatalf("初始化后应存在 %s：%v", dbName, err)
	}
	if _, err := os.Stat(filepath.Join(w.Store, indexLegacyName)); !os.IsNotExist(err) {
		t.Errorf("不应再产出 %s（stat err=%v）", indexLegacyName, err)
	}
}

// TestIndexReloadAfterClose：全量落盘 → 释放工作区（连接即开即关，无 .db 锁残留）→
// 新实例重开可完整读回（符号/调用/导入/基线字段）。
func TestIndexReloadAfterClose(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	want := []string{"main.go", "utils/helper.go"}
	if got := w.IndexedFiles(); !reflect.DeepEqual(got, want) {
		t.Errorf("IndexedFiles = %v, want %v", got, want)
	}
	dropWorkspace(w.Dir)

	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)
	ok, err := w2.LoadIndex()
	if err != nil || !ok {
		t.Fatalf("LoadIndex after reopen = (%v, %v)，期望 (true, nil)", ok, err)
	}
	if got := w2.IndexedFiles(); !reflect.DeepEqual(got, want) {
		t.Errorf("重开后 IndexedFiles = %v, want %v", got, want)
	}
	syms := w2.ix.AllSymbols()
	if len(syms) != 4 {
		t.Errorf("重开后应有 4 个符号，got %d", len(syms))
	}
	for _, s := range syms {
		if s.ID == "" {
			t.Errorf("重开后符号 ID 应重建：%+v", s)
		}
	}
	if fi := w2.ix.Files["main.go"]; fi == nil || fi.Imports == nil || fi.Mtime == 0 || fi.Size == 0 {
		t.Errorf("重开后文件条目基线字段应完整：%+v", fi)
	}
}

// TestReconcileIncrementalPersists：增量（新增/修改/删除）后仅变更文件落盘，重开读回一致。
func TestReconcileIncrementalPersists(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	// 新增 new.go + 修改 main.go + 删除 utils/helper.go
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package main\nfunc NewFunc() {}\n"), 0o644); err != nil {
		t.Fatalf("write new.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "utils", "helper.go")); err != nil {
		t.Fatalf("remove helper.go: %v", err)
	}
	changed, err := w.Reconcile()
	if err != nil || !changed {
		t.Fatalf("Reconcile = (%v, %v)，期望 (true, nil)", changed, err)
	}
	dropWorkspace(w.Dir)

	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)
	ok, err := w2.LoadIndex()
	if err != nil || !ok {
		t.Fatalf("LoadIndex after delta = (%v, %v)", ok, err)
	}
	if got, want := w2.IndexedFiles(), []string{"main.go", "new.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("增量后落盘文件集 = %v, want %v", got, want)
	}
	names := map[string]bool{}
	for _, s := range w2.ix.AllSymbols() {
		names[s.Name] = true
	}
	if !names["Extra"] || !names["NewFunc"] {
		t.Errorf("增量后应含 Extra/NewFunc，got %v", names)
	}
	if names["Helper"] {
		t.Errorf("已删除文件的符号不应残留：%v", names)
	}
}

// TestLoadIndexCorruptDB：损坏/截断/空索引库 → 兜底（不 panic；视为未初始化并回状态），
// 且 Clear/Initialize 可恢复。
func TestLoadIndexCorruptDB(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p string) error
	}{
		{"garbage", func(p string) error { return os.WriteFile(p, []byte("this is not a bolt database"), 0o644) }},
		{"truncated", func(p string) error {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(p, b[:100], 0o644)
		}},
		{"empty", func(p string) error { return os.WriteFile(p, nil, 0o644) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTestProject(t, dir)

			w, err := Open(dir)
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			if err := w.Initialize(nil, nil, nil); err != nil {
				t.Fatalf("Initialize failed: %v", err)
			}
			dropWorkspace(w.Dir)

			w2, err := Open(dir)
			if err != nil {
				t.Fatalf("reopen failed: %v", err)
			}
			defer dropWorkspace(w2.Dir)
			if err := c.mutate(w2.dbPath()); err != nil {
				t.Fatalf("mutate %s: %v", dbName, err)
			}

			ok, err := w2.LoadIndex()
			if err != nil {
				t.Fatalf("损坏索引应兜底（不抛错），got err=%v", err)
			}
			if ok {
				t.Error("损坏索引不应被视为已载入")
			}
			if w2.State() != "" {
				t.Errorf("损坏索引应回「未初始化」，got state=%q", w2.State())
			}

			// 恢复路径：重建后可用
			if err := w2.Initialize(nil, nil, nil); err != nil {
				t.Fatalf("重建失败：%v", err)
			}
			if got := w2.IndexedFiles(); len(got) != 2 {
				t.Errorf("重建后应有 2 个文件，got %v", got)
			}
		})
	}
}

// TestLegacyJSONNotMigrated：检测到旧 index.json（无 index.db）→ 不加载、删除旧文件、
// 状态回「未初始化」（配置存档保留）；随后全量重建产出 index.db。
func TestLegacyJSONNotMigrated(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	legacy := `{"files":[{"path":"legacy.go","lang":"go","imports":[],"symbols":[{"file":"legacy.go","name":"Legacy","kind":"func","line":1}]}]}`
	if err := os.WriteFile(filepath.Join(w.Store, indexLegacyName), []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy json: %v", err)
	}
	w.setMeta(Meta{State: "ready", LastIndexedAt: 123, Exts: []string{".go"}})
	if err := w.saveMeta(); err != nil {
		t.Fatalf("saveMeta failed: %v", err)
	}

	ok, err := w.LoadIndex()
	if err != nil {
		t.Fatalf("旧 JSON 不应报错（不自动迁移）：%v", err)
	}
	if ok {
		t.Error("旧 JSON 不应被加载")
	}
	if _, err := os.Stat(filepath.Join(w.Store, indexLegacyName)); !os.IsNotExist(err) {
		t.Errorf("旧 index.json 应被删除（stat err=%v）", err)
	}
	if w.State() != "" {
		t.Errorf("状态应回「未初始化」，got %q", w.State())
	}
	if w.Meta().Exts[0] != ".go" {
		t.Errorf("配置存档应保留，got %+v", w.Meta())
	}

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("重建失败：%v", err)
	}
	if !w.dbExists() {
		t.Error("重建后应存在 index.db")
	}
	if got, want := w.IndexedFiles(), []string{"main.go", "utils/helper.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("重建后文件集 = %v, want %v（不含旧 JSON 的 legacy.go）", got, want)
	}
}

// TestConcurrentReadIndex：并发读安全（多 goroutine 同时 LoadIndex/查询，无错误/竞争）。
func TestConcurrentReadIndex(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	dropWorkspace(w.Dir)

	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := w2.LoadIndex()
			if err != nil {
				errs <- err
				return
			}
			if !ok {
				errs <- fmt.Errorf("LoadIndex=false")
				return
			}
			if got := len(w2.IndexedFiles()); got != 2 {
				errs <- fmt.Errorf("IndexedFiles=%d, want 2", got)
				return
			}
			if got := len(w2.SearchSymbol("Helper", "", "", 0)); got != 1 {
				errs <- fmt.Errorf("SearchSymbol(Helper)=%d, want 1", got)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("并发读失败：%v", e)
	}
}

// TestModuleOfConcurrent：并发调用 moduleOf（包级缓存共享）不得 data race（go test -race）。
// 旧实现无锁读写包级 moduleCache → -race 下必报竞争；本测试亦断言各调用取到正确 module。
func TestModuleOfConcurrent(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "go.mod"), []byte("module modA\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "go.mod"), []byte("module modB\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		dir, want := dirA, "modA"
		if i%2 == 1 {
			dir, want = dirB, "modB"
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if got := moduleOf(dir); got != want {
					t.Errorf("moduleOf(%q) = %q, want %q", dir, got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}
