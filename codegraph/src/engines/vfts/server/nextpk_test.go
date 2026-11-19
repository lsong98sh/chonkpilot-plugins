// 主键游标（NextPK）崩溃一致性：先持久化预留，再写 chunk。
// 允许主键空洞，但绝不允许 NextPK 落后于已落盘 chunk 的最大主键（否则下次增量会复用主键覆盖旧块）。
//
// 运行前置同 incremental_test.go 文件头（CGO + `zvec_c_api.dll` 在 PATH）。
package server

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// reserveNextPK 持久化语义：抬升后落盘；较小值不回退；重新从磁盘读仍是预留值。
func TestReserveNextPKPersists(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { Drop(dir); CloseAll() })

	if err := w.reserveNextPK(41); err != nil {
		t.Fatalf("reserveNextPK(41): %v", err)
	}
	if err := w.reserveNextPK(17); err != nil {
		t.Fatalf("reserveNextPK(17): %v", err)
	}
	if got := w.Meta().NextPK; got != 41 {
		t.Fatalf("NextPK 不应回退：got=%d want=41", got)
	}

	// 丢弃进程内缓存后重新 Open（从 meta.json 载入）验证持久化。
	Drop(dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { Drop(dir) })
	if got := w2.Meta().NextPK; got != 41 {
		t.Fatalf("重新从磁盘载入的 NextPK=%d，期望 41", got)
	}
}

// ReloadMeta 读到旧磁盘快照时不得把内存 NextPK 拉回（否则随后 saveMeta 会把已预留游标写回旧值）。
func TestReloadMetaDoesNotLowerNextPK(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { Drop(dir); CloseAll() })

	if err := w.reserveNextPK(99); err != nil {
		t.Fatalf("reserveNextPK(99): %v", err)
	}
	// 模拟并发写者/旧进程留下较旧的 meta 快照。
	if err := os.WriteFile(filepath.Join(filepath.FromSlash(w.Store), metaName),
		[]byte(`{"nextPK":7}`), 0o644); err != nil {
		t.Fatalf("write stale meta: %v", err)
	}
	if err := w.ReloadMeta(); err != nil {
		t.Fatalf("ReloadMeta: %v", err)
	}
	if got := w.Meta().NextPK; got != 99 {
		t.Fatalf("ReloadMeta 后 NextPK=%d，期望保持 99（单调不回退）", got)
	}
}

// 增量索引后：落盘 meta.NextPK >= 本次写入的最大纯数字主键（下次增量不会复用）。
func TestIncrementalNextPKCoversMaxDocID(t *testing.T) {
	dir := t.TempDir()
	writeText(t, dir, "a.txt", "alpha apple\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { Drop(dir); CloseAll() })

	full, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if len(full.Indexed) != 1 || len(full.Indexed[0].DocIDs) == 0 {
		t.Fatalf("前置：a.txt 应有块：%+v", full)
	}

	writeText(t, dir, "a.txt", "beta banana\n")
	inc, err := w.Incremental([]IncrementalFile{{
		Path: "a.txt", DocIDs: full.Indexed[0].DocIDs,
	}}, nil)
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if len(inc.Indexed) != 1 || len(inc.Indexed[0].DocIDs) == 0 {
		t.Fatalf("增量应重建 a.txt 并写入块：%+v", inc)
	}

	var maxID int64
	for _, id := range inc.Indexed[0].DocIDs {
		n, perr := strconv.ParseInt(id, 10, 64)
		if perr != nil {
			t.Fatalf("主键应为纯数字串：id=%q err=%v", id, perr)
		}
		if n > maxID {
			maxID = n
		}
	}
	if maxID == 0 {
		t.Fatal("增量未分配任何主键")
	}
	if got := w.Meta().NextPK; got < maxID {
		t.Fatalf("内存 NextPK=%d < 最大 doc_id=%d", got, maxID)
	}

	// 从磁盘读 meta：重新 Open（进程内缓存丢弃）后 NextPK 必须仍 >= 最大主键。
	Drop(dir)
	w2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { Drop(dir) })
	if got := w2.Meta().NextPK; got < maxID {
		t.Fatalf("落盘 NextPK=%d < 最大 doc_id=%d（崩溃后将复用主键）", got, maxID)
	}
}

// 预留落盘失败 → Incremental 返回错误且**不写任何 chunk**（宁可主键空洞，不可复用主键）。
func TestIncrementalReserveFailureDoesNotWriteChunks(t *testing.T) {
	dir := t.TempDir()
	writeText(t, dir, "a.txt", "alpha apple\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { Drop(dir); CloseAll() })

	orig := reserveNextPKSave
	reserveNextPKSave = func(*Workspace) error { return errors.New("injected reserve save failure") }
	defer func() { reserveNextPKSave = orig }()

	_, err = w.Incremental([]IncrementalFile{{Path: "a.txt"}}, nil)
	if err == nil {
		t.Fatal("预留落盘失败时 Incremental 应返回错误")
	}
	if got := w.Meta().State; got != "error" {
		t.Fatalf("预留失败后 state 应为 error：got=%q", got)
	}
	// 集合已打开但未写入任何块 → 检索无命中。
	hits, qerr := w.Query("alpha", "", 10, "")
	if qerr != nil {
		t.Fatalf("query: %v", qerr)
	}
	if len(hits) != 0 {
		t.Fatalf("预留失败后不应写入 chunk：hits=%+v", hits)
	}
}
