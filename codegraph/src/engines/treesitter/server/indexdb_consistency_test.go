package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// countDBFiles 统计 index.db 的 files bucket 条数（落盘集合大小）。
// 库不存在 / 不可用 → 0（与「未初始化」同口径）。
func countDBFiles(t *testing.T, w *Workspace) int {
	t.Helper()
	if !w.dbUsable() {
		return 0
	}
	db, err := w.openDB(true)
	if err != nil {
		t.Fatalf("open index.db: %v", err)
	}
	defer func() { _ = db.Close() }()
	n := 0
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketFiles))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, _ []byte) error {
			n++
			return nil
		})
	}); err != nil {
		t.Fatalf("scan files bucket: %v", err)
	}
	return n
}

// CODEGRAPH-IDXDB-1 ①/③：`status.indexedFiles` 与落盘 `index.db` 的 files bucket
// **双向一致**（同一条索引快照派生），且 Clear 后 status 不保留陈旧条数。
func TestStatusMatchesDiskIndex(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, "skipme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skipme", "c.go"),
		[]byte("package skipme\n\nfunc Skipped() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	// skip-dirs 生效：只索引 main.go + utils/helper.go
	if err := w.Initialize(nil, []string{"skipme/"}, nil); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	st := w.Status()
	if st.IndexedFiles != 2 {
		t.Fatalf("status.indexedFiles = %d，期望 2", st.IndexedFiles)
	}
	if got := countDBFiles(t, w); got != st.IndexedFiles {
		t.Fatalf("落盘 files=%d ≠ status.indexedFiles=%d（同快照必须相等）", got, st.IndexedFiles)
	}

	// 重新打开（进程内缓存清空）→ 由落盘库派生 same 快照；status 不得因未载入而报 0。
	dropWorkspace(w.Dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer dropWorkspace(w2.Dir)
	if ok, err := w2.LoadIndex(); err != nil || !ok {
		t.Fatalf("LoadIndex = (%v, %v)", ok, err)
	}
	st2 := w2.Status()
	if st2.IndexedFiles != 2 || !st2.Loaded {
		t.Fatalf("重开后 status = %+v，期望 indexedFiles=2 loaded=true", st2)
	}
	if got := countDBFiles(t, w2); got != st2.IndexedFiles {
		t.Fatalf("重开后 落盘 files=%d ≠ status=%d", got, st2.IndexedFiles)
	}

	// Clear：落盘库删除 + status 归零（不得残留旧快照条数）
	if err := w2.Clear(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	st3 := w2.Status()
	if st3.IndexedFiles != 0 || st3.Loaded || st3.State != "" {
		t.Fatalf("Clear 后 status = %+v，期望 indexedFiles=0 loaded=false state=\"\"", st3)
	}
	if got := countDBFiles(t, w2); got != 0 {
		t.Fatalf("Clear 后落盘 files=%d，期望 0", got)
	}
	if _, err := os.Stat(w2.dbPath()); !os.IsNotExist(err) {
		t.Fatalf("Clear 后 index.db 应被删除（stat err=%v）", err)
	}
}

// CODEGRAPH-IDXDB-1 ①（双实例归因）：同一 workdir 以不同大小写再次 Open 必须复用**同一个**
// Workspace（Windows 路径大小写不敏感）；否则两套内存索引共写一个 index.db → status 与落盘不一致。
func TestOpenCaseInsensitiveReuse(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows 路径语义专用")
	}
	dir := t.TempDir()
	writeTestProject(t, dir)

	w1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w1.Dir)

	// 大小写翻转 + 正斜杠（等价路径）
	alt := strings.ToUpper(filepath.ToSlash(dir))
	if alt == w1.Dir {
		t.Skip("大小写翻转后路径未变化（目录名非 ASCII / 无可翻转字符）")
	}
	w2, err := Open(alt)
	if err != nil {
		t.Fatalf("Open(alt) failed: %v", err)
	}
	if w2 != w1 {
		t.Fatalf("同一 workdir 不同大小写应复用同一 Workspace（%p ≠ %p）", w2, w1)
	}
}
