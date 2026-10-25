package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeSrc 在 dir 下写一个源码文件（目录自动创建）。
func writeSrc(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// newIndexedWorkspace 建临时仓库并全量索引，返回工作区。
func newIndexedWorkspace(t *testing.T) *Workspace {
	t.Helper()
	dir := t.TempDir()
	writeSrc(t, dir, "a.go", "package a\n\nfunc Alpha() {}\n")
	writeSrc(t, dir, "b.ts", "export function Beta() {}\n")
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { dropWorkspace(w.Dir) })
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return w
}

func TestIndexPathsUpdate(t *testing.T) {
	w := newIndexedWorkspace(t)
	if got := len(w.SearchSymbol("Alpha", "", "", 0)); got != 1 {
		t.Fatalf("Alpha before update = %d, want 1", got)
	}

	p := writeSrc(t, w.Dir, "a.go", "package a\n\nfunc Gamma() {}\n\nfunc Delta() {}\n")
	indexed, removed, skipped, err := w.IndexPaths([]string{p}, nil)
	if err != nil {
		t.Fatalf("IndexPaths: %v", err)
	}
	if indexed != 1 || removed != 0 || skipped != 0 {
		t.Errorf("indexed/removed/skipped = %d/%d/%d, want 1/0/0", indexed, removed, skipped)
	}
	if got := len(w.SearchSymbol("Alpha", "", "", 0)); got != 0 {
		t.Errorf("Alpha after update = %d, want 0", got)
	}
	if got := len(w.SearchSymbol("Gamma", "", "", 0)); got != 1 {
		t.Errorf("Gamma after update = %d, want 1（符号不得重复）", got)
	}
	if got := len(w.SearchSymbol("Delta", "", "", 0)); got != 1 {
		t.Errorf("Delta after update = %d, want 1", got)
	}
	if got := len(w.SearchSymbol("Beta", "", "", 0)); got != 1 {
		t.Errorf("Beta (untouched file) = %d, want 1", got)
	}
	if st := w.Status(); st.State != "ready" {
		t.Errorf("state = %q, want ready", st.State)
	}
}

func TestIndexPathsRemove(t *testing.T) {
	w := newIndexedWorkspace(t)
	p := filepath.Join(w.Dir, "a.go")
	if err := os.Remove(filepath.FromSlash(p)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	indexed, removed, skipped, err := w.IndexPaths(nil, []string{p})
	if err != nil {
		t.Fatalf("IndexPaths: %v", err)
	}
	if indexed != 0 || removed != 1 || skipped != 0 {
		t.Errorf("indexed/removed/skipped = %d/%d/%d, want 0/1/0", indexed, removed, skipped)
	}
	if got := len(w.SearchSymbol("Alpha", "", "", 0)); got != 0 {
		t.Errorf("Alpha after remove = %d, want 0", got)
	}
	for _, f := range w.IndexedFiles() {
		if f == "a.go" {
			t.Error("a.go 仍在索引中")
		}
	}
	if got := len(w.SearchSymbol("Beta", "", "", 0)); got != 1 {
		t.Errorf("Beta after remove = %d, want 1", got)
	}
}

func TestIndexPathsAdd(t *testing.T) {
	w := newIndexedWorkspace(t)
	p := writeSrc(t, w.Dir, "pkg/c.go", "package pkg\n\nfunc Epsilon() {}\n")
	indexed, removed, skipped, err := w.IndexPaths([]string{p}, nil)
	if err != nil {
		t.Fatalf("IndexPaths: %v", err)
	}
	if indexed != 1 || removed != 0 || skipped != 0 {
		t.Errorf("indexed/removed/skipped = %d/%d/%d, want 1/0/0", indexed, removed, skipped)
	}
	if got := len(w.SearchSymbol("Epsilon", "", "", 0)); got != 1 {
		t.Errorf("Epsilon = %d, want 1", got)
	}
}

func TestIndexPathsSkipped(t *testing.T) {
	t.Run("扩展名不在生效集合", func(t *testing.T) {
		w := newIndexedWorkspace(t)
		// 仅索引 .ts → a.go 不再可索引。
		if err := w.Configure(nil, []string{".ts"}, nil, nil); err != nil {
			t.Fatalf("Configure: %v", err)
		}
		indexed, removed, skipped, err := w.IndexPaths([]string{filepath.Join(w.Dir, "a.go")}, nil)
		if err != nil {
			t.Fatalf("IndexPaths: %v", err)
		}
		if indexed != 0 || skipped != 1 {
			t.Errorf("indexed/skipped = %d/%d, want 0/1", indexed, skipped)
		}
		if removed != 0 {
			t.Errorf("removed = %d, want 0（removes 入参为空）", removed)
		}
		for _, f := range w.IndexedFiles() {
			if f == "a.go" {
				t.Error("不可索引文件应从索引删除")
			}
		}
	})

	t.Run("命中 ignore 规则", func(t *testing.T) {
		w := newIndexedWorkspace(t)
		writeSrc(t, w.Dir, "skip/keep.go", "package skip\n\nfunc Zeta() {}\n")
		if _, _, _, err := w.IndexPaths([]string{filepath.Join(w.Dir, "skip/keep.go")}, nil); err != nil {
			t.Fatalf("IndexPaths(add): %v", err)
		}
		if got := len(w.SearchSymbol("Zeta", "", "", 0)); got != 1 {
			t.Fatalf("Zeta = %d, want 1", got)
		}
		if err := w.Configure(nil, nil, []string{"skip/"}, nil); err != nil {
			t.Fatalf("Configure: %v", err)
		}
		indexed, _, skipped, err := w.IndexPaths([]string{filepath.Join(w.Dir, "skip/keep.go")}, nil)
		if err != nil {
			t.Fatalf("IndexPaths: %v", err)
		}
		if indexed != 0 || skipped != 1 {
			t.Errorf("indexed/skipped = %d/%d, want 0/1", indexed, skipped)
		}
		if got := len(w.SearchSymbol("Zeta", "", "", 0)); got != 0 {
			t.Errorf("Zeta after ignore = %d, want 0（应从索引删除）", got)
		}
	})

	t.Run("未初始化", func(t *testing.T) {
		dir := t.TempDir()
		writeSrc(t, dir, "a.go", "package a\n")
		w, err := Open(dir)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { dropWorkspace(w.Dir) })
		if _, _, _, err := w.IndexPaths([]string{filepath.Join(dir, "a.go")}, nil); !errors.Is(err, ErrNotInitialized) {
			t.Errorf("err = %v, want ErrNotInitialized", err)
		}
	})
}

// TestCodegraphIndexPathsTool 经 MCP 工具面调用 codegraph_index_paths（契约回归）。
func TestCodegraphIndexPathsTool(t *testing.T) {
	w := newIndexedWorkspace(t)
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-codegraph", Version: "0.0.1"}, nil)
	if err := RegisterTools(srv); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	p := writeSrc(t, w.Dir, "tool.go", "package tool\n\nfunc Iota() {}\n")
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "codegraph_index_paths", Arguments: map[string]any{
		"workdir": w.Dir, "files": []string{p},
	}})
	if err != nil || res.IsError {
		t.Fatalf("call index_paths: err=%v res=%+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out struct {
		OK      bool `json:"ok"`
		Indexed int  `json:"indexed"`
		Removed int  `json:"removed"`
		Skipped int  `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", text, err)
	}
	if !out.OK || out.Indexed != 1 || out.Removed != 0 || out.Skipped != 0 {
		t.Errorf("resp = %s, want ok indexed=1 removed=0 skipped=0", text)
	}
	if got := len(w.SearchSymbol("Iota", "", "", 0)); got != 1 {
		t.Errorf("Iota = %d, want 1", got)
	}

	if err := os.Remove(filepath.FromSlash(p)); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "codegraph_index_paths", Arguments: map[string]any{
		"workdir": w.Dir, "remove": []string{p},
	}})
	if err != nil || res.IsError {
		t.Fatalf("call index_paths(remove): err=%v res=%+v", err, res)
	}
	if got := len(w.SearchSymbol("Iota", "", "", 0)); got != 0 {
		t.Errorf("Iota after remove = %d, want 0", got)
	}
}

func TestGateIndexStatus(t *testing.T) {
	oldWait, oldPoll := indexReadyWait, indexReadyPoll
	indexReadyWait, indexReadyPoll = 250*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { indexReadyWait, indexReadyPoll = oldWait, oldPoll })

	w := newIndexedWorkspace(t)

	t.Run("indexing 超时仍 pending", func(t *testing.T) {
		if err := WriteIndexStatus(w.Dir, IndexStatus{State: "indexing", Pending: 3}); err != nil {
			t.Fatalf("WriteIndexStatus: %v", err)
		}
		resp, blocked := gateIndexStatus(w.Dir)
		if !blocked {
			t.Fatal("indexing 状态应拦截查询")
		}
		m, ok := resp.(map[string]any)
		if !ok || m["status"] != "pending" || m["state"] != "indexing" || m["pending"] != 3 {
			t.Errorf("resp = %#v, want status=pending state=indexing pending=3", resp)
		}
	})

	t.Run("等待期间转 ready", func(t *testing.T) {
		if err := WriteIndexStatus(w.Dir, IndexStatus{State: "indexing", Pending: 1}); err != nil {
			t.Fatalf("WriteIndexStatus: %v", err)
		}
		go func() {
			time.Sleep(60 * time.Millisecond)
			_ = WriteIndexStatus(w.Dir, IndexStatus{State: "ready", Pending: 0})
		}()
		if resp, blocked := gateIndexStatus(w.Dir); blocked {
			t.Errorf("就绪后不应拦截，resp = %#v", resp)
		}
	})

	t.Run("error 拦截", func(t *testing.T) {
		if err := WriteIndexStatus(w.Dir, IndexStatus{State: "error", Message: "boom"}); err != nil {
			t.Fatalf("WriteIndexStatus: %v", err)
		}
		resp, blocked := gateIndexStatus(w.Dir)
		if !blocked {
			t.Fatal("error 状态应拦截查询")
		}
		m, ok := resp.(map[string]any)
		if !ok || m["status"] != "error" || m["message"] != "boom" {
			t.Errorf("resp = %#v, want status=error message=boom", resp)
		}
	})

	t.Run("ready 放行", func(t *testing.T) {
		if err := WriteIndexStatus(w.Dir, IndexStatus{State: "ready", Pending: 0}); err != nil {
			t.Fatalf("WriteIndexStatus: %v", err)
		}
		resp, ws, ok := readyGate(map[string]any{"workdir": w.Dir})
		if !ok {
			t.Fatalf("ready 应放行，resp = %#v", resp)
		}
		if ws == nil {
			t.Fatal("workspace 为空")
		}
		if got := len(ws.SearchSymbol("Alpha", "", "", 0)); got != 1 {
			t.Errorf("Alpha = %d, want 1", got)
		}
	})

	t.Run("状态文件不存在放行", func(t *testing.T) {
		if err := os.Remove(IndexStatusPath(w.Dir)); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove status: %v", err)
		}
		if resp, blocked := gateIndexStatus(w.Dir); blocked {
			t.Errorf("文件不存在应放行，resp = %#v", resp)
		}
	})
}
