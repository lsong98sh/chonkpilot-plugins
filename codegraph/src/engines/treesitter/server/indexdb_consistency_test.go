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

// Initialize 的落盘顺序 = 先写新快照、成功后才换内存。保存失败（此处用另一 bbolt 句柄
// 独占锁住 index.db → 写库超时）时必须：
//   - 返回错误、状态回 error；
//   - **不换**内存索引（新增文件只出现在 next、不进入 w.ix）；
//   - 磁盘旧索引保持原样（未被清空/重写），重开仍是旧快照。
func TestInitializeSaveFailureKeepsMemoryAndDisk(t *testing.T) {
	dir := t.TempDir()
	writeTestProject(t, dir)
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("首次 Initialize failed: %v", err)
	}
	before := w.Status()
	if before.IndexedFiles == 0 {
		t.Fatalf("前置：首次索引应有文件，status=%+v", before)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("前置：落盘 files=%d ≠ status=%d", got, before.IndexedFiles)
	}

	// 新增可索引文件：若保存失败后仍替换内存索引，status.indexedFiles 会变成 before+1。
	if err := os.WriteFile(filepath.Join(dir, "extra.go"),
		[]byte("package extra\n\nfunc Extra() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 独占锁住索引库（第二个写者 open 会等待 dbOpenTimeout 后返回 ErrTimeout）。
	held, err := bolt.Open(w.dbPath(), 0o644, &bolt.Options{})
	if err != nil {
		t.Fatalf("hold index.db: %v", err)
	}

	err = w.Initialize(nil, nil, nil)
	closeErr := held.Close()
	if err == nil {
		t.Fatal("index.db 被独占时 Initialize 应保存失败并返回错误")
	}
	if closeErr != nil {
		t.Fatalf("release index.db: %v", closeErr)
	}

	if st := w.State(); st != "error" {
		t.Fatalf("保存失败后 state 应为 error：got=%q", st)
	}
	if got := w.Status().IndexedFiles; got != before.IndexedFiles {
		t.Fatalf("保存失败后内存索引被替换：indexedFiles=%d，期望保持 %d（extra.go 不得进入内存）",
			got, before.IndexedFiles)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("保存失败后磁盘索引被改写：files=%d，期望保持 %d", got, before.IndexedFiles)
	}
}

// saveIndexSnapshot(nil) 必须安全：写入仅含 schema 的空索引（0 文件），可被 LoadIndex 识别。
func TestSaveIndexSnapshotNilSafe(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer dropWorkspace(w.Dir)

	if err := w.saveIndexSnapshot(nil); err != nil {
		t.Fatalf("saveIndexSnapshot(nil): %v", err)
	}
	if got := countDBFiles(t, w); got != 0 {
		t.Fatalf("空快照落盘 files=%d，期望 0", got)
	}
	ok, err := w.LoadIndex()
	if err != nil || !ok {
		t.Fatalf("空快照应可识别为已初始化空索引：LoadIndex=(%v,%v)", ok, err)
	}
	if got := w.Status().IndexedFiles; got != 0 {
		t.Fatalf("空快照载入后 indexedFiles=%d，期望 0", got)
	}
}

// IndexPaths 的落盘顺序同样是「先写新快照、成功后才换内存」。用独占 bbolt 句柄模拟保存失败：
// 返回错误、状态 error、内存/磁盘均为旧快照（新增符号不得进入内存，磁盘条数不变）。
func TestIndexPathsSaveFailureKeepsMemoryAndDisk(t *testing.T) {
	w := newIndexedWorkspace(t)
	before := w.Status()
	if before.IndexedFiles == 0 {
		t.Fatalf("前置：应有索引文件，status=%+v", before)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("前置：落盘 files=%d ≠ status=%d", got, before.IndexedFiles)
	}

	p := writeSrc(t, w.Dir, "pkg/new.go", "package pkg\n\nfunc NewlyAdded() {}\n")

	held, err := bolt.Open(w.dbPath(), 0o644, &bolt.Options{})
	if err != nil {
		t.Fatalf("hold index.db: %v", err)
	}
	_, _, _, err = w.IndexPaths([]string{p}, nil)
	closeErr := held.Close()
	if err == nil {
		t.Fatal("index.db 被独占时 IndexPaths 应保存失败并返回错误")
	}
	if closeErr != nil {
		t.Fatalf("release index.db: %v", closeErr)
	}

	if st := w.State(); st != "error" {
		t.Fatalf("保存失败后 state 应为 error：got=%q", st)
	}
	if got := w.Status().IndexedFiles; got != before.IndexedFiles {
		t.Fatalf("保存失败后内存索引被替换：indexedFiles=%d，期望保持 %d", got, before.IndexedFiles)
	}
	if got := len(w.SearchSymbol("NewlyAdded", "", "", 0)); got != 0 {
		t.Fatalf("保存失败后新符号进入了内存索引：命中 %d 次", got)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("保存失败后磁盘索引被改写：files=%d，期望保持 %d", got, before.IndexedFiles)
	}
}

// Reconcile 的落盘顺序：先 saveIndexDelta（只依赖 upserts/removed），成功后才换内存。
// 独占 bbolt 句柄模拟保存失败：内存/磁盘均为旧快照，新符号不进入内存。
func TestReconcileSaveFailureKeepsMemoryAndDisk(t *testing.T) {
	w := newIndexedWorkspace(t)
	before := w.Status()
	if before.IndexedFiles == 0 {
		t.Fatalf("前置：应有索引文件，status=%+v", before)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("前置：落盘 files=%d ≠ status=%d", got, before.IndexedFiles)
	}

	// 新增文件（mtime/size 与旧条目不同 → reconcile 会产生 upsert）。
	writeSrc(t, w.Dir, "pkg/reconciled.go", "package pkg\n\nfunc ReconcileOnly() {}\n")

	held, err := bolt.Open(w.dbPath(), 0o644, &bolt.Options{})
	if err != nil {
		t.Fatalf("hold index.db: %v", err)
	}
	changed, err := w.Reconcile()
	closeErr := held.Close()
	if err == nil {
		t.Fatal("index.db 被独占时 Reconcile 应保存失败并返回错误")
	}
	if closeErr != nil {
		t.Fatalf("release index.db: %v", closeErr)
	}
	if !changed {
		t.Fatal("发生过新增时 changed 应为 true")
	}

	if st := w.State(); st != "error" {
		t.Fatalf("保存失败后 state 应为 error：got=%q", st)
	}
	if got := w.Status().IndexedFiles; got != before.IndexedFiles {
		t.Fatalf("保存失败后内存索引被替换：indexedFiles=%d，期望保持 %d", got, before.IndexedFiles)
	}
	if got := len(w.SearchSymbol("ReconcileOnly", "", "", 0)); got != 0 {
		t.Fatalf("保存失败后新符号进入了内存索引：命中 %d 次", got)
	}
	if got := countDBFiles(t, w); got != before.IndexedFiles {
		t.Fatalf("保存失败后磁盘索引被改写：files=%d，期望保持 %d（旧快照）", got, before.IndexedFiles)
	}
}

// 正常路径：Initialize → IndexPaths → Reconcile 之后，内存 status 与落盘 files bucket 始终一致。
func TestDiskMemoryConsistentAcrossUpdatePaths(t *testing.T) {
	w := newIndexedWorkspace(t)
	check := func(stage string) {
		t.Helper()
		st := w.Status()
		if got := countDBFiles(t, w); got != st.IndexedFiles {
			t.Fatalf("%s：落盘 files=%d ≠ status.indexedFiles=%d", stage, got, st.IndexedFiles)
		}
	}
	check("Initialize")

	p := writeSrc(t, w.Dir, "pkg/added.go", "package pkg\n\nfunc AddedOne() {}\n")
	if _, _, _, err := w.IndexPaths([]string{p}, nil); err != nil {
		t.Fatalf("IndexPaths: %v", err)
	}
	check("IndexPaths")
	if got := len(w.SearchSymbol("AddedOne", "", "", 0)); got != 1 {
		t.Fatalf("IndexPaths 后 AddedOne 命中 %d 次，期望 1", got)
	}

	// 改内容（长度不同确保 mtime/size 快检命中差异）→ Reconcile 走 delta。
	writeSrc(t, w.Dir, "a.go", "package a\n\nfunc RebuiltGamma() {}\n")
	if changed, err := w.Reconcile(); err != nil || !changed {
		t.Fatalf("Reconcile = (%v, %v)，期望 (true, nil)", changed, err)
	}
	check("Reconcile")
	if got := len(w.SearchSymbol("RebuiltGamma", "", "", 0)); got != 1 {
		t.Fatalf("Reconcile 后 RebuiltGamma 命中 %d 次，期望 1", got)
	}
	if got := len(w.SearchSymbol("Alpha", "", "", 0)); got != 0 {
		t.Fatalf("Reconcile 后旧符号 Alpha 仍命中 %d 次，期望 0", got)
	}
}
