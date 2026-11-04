// 会话内容索引（SESSION-FTS）冒烟：真实执行 zvec C-API。
//
// 覆盖：写入（先删后插幂等）/ 会话过滤 / role 过滤 / ts 升序 / snippet 正文片段 /
// 按 message_id 删除 / 会话级与整集合清理 / **文件索引全量重建不影响会话索引**。
//
// 运行需 CGO 环境 + `zvec_c_api.dll`（与 incremental_test.go 同口径）。
package server

import (
	"errors"
	"strings"
	"testing"
)

// seedSessionDocs 造两条会话的少量消息（时间戳递增）。
func seedSessionDocs() []SessionDoc {
	return []SessionDoc{
		{SessionID: "s1", DialogID: "d1", TurnID: "t1", MessageID: "m1", Role: "user", TS: 1000, Text: "如何优化全文索引的分词性能"},
		{SessionID: "s1", DialogID: "d1", TurnID: "t1", MessageID: "m2", Role: "assistant", TS: 2000, Text: "可以把分词器换成 jieba 并重建索引"},
		{SessionID: "s2", DialogID: "d2", TurnID: "t9", MessageID: "m9", Role: "user", TS: 3000, Text: "无关会话里的全文索引说明"},
	}
}

// TestSessionIndexQueryAndClear：写入 → 查询（会话/角色过滤、ts 升序、正文 snippet）→ 删除/清理。
func TestSessionIndexQueryAndClear(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()

	res, err := w.IndexSessions(seedSessionDocs(), nil)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if res.Added != 3 || res.Total != 3 {
		t.Fatalf("index 计数 = %+v，期望 added=3 total=3", res)
	}

	// 幂等：同内容重写 → 总数不变
	res, err = w.IndexSessions(seedSessionDocs(), nil)
	if err != nil {
		t.Fatalf("re-index: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("重写后 total = %d，期望 3", res.Total)
	}

	// 会话过滤 + ts 升序
	hits, err := w.QuerySessions("全文索引", "", 20, "s1", "")
	if err != nil {
		t.Fatalf("query session s1: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("s1 无命中")
	}
	for _, h := range hits {
		if h.SessionID != "s1" {
			t.Fatalf("会话过滤失效：%+v", h)
		}
		if h.MessageID == "" || h.TurnID == "" || h.DialogID != "d1" {
			t.Fatalf("命中字段缺失：%+v", h)
		}
		if !strings.Contains(h.Snippet, "索引") {
			t.Fatalf("snippet 应为命中处的正文片段：%q", h.Snippet)
		}
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].TS > hits[i].TS {
			t.Fatalf("命中未按 ts 升序：%d > %d", hits[i-1].TS, hits[i].TS)
		}
	}

	// 角色过滤
	roleHits, err := w.QuerySessions("jieba", "", 20, "s1", "assistant")
	if err != nil {
		t.Fatalf("query role: %v", err)
	}
	if len(roleHits) != 1 || roleHits[0].MessageID != "m2" {
		t.Fatalf("role 过滤不符：%+v", roleHits)
	}
	if got, err := w.QuerySessions("jieba", "", 20, "s1", "user"); err != nil || len(got) != 0 {
		t.Fatalf("user 过滤应无命中：%+v err=%v", got, err)
	}

	// 不传 session_id → 全部会话
	all, err := w.QuerySessions("全文索引", "", 20, "", "")
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("全量命中 = %d，期望 3", len(all))
	}

	// 按 message_id 删除（先删后插的反向）
	if _, err := w.IndexSessions(nil, []SessionRemove{{SessionID: "s1", MessageID: "m2"}}); err != nil {
		t.Fatalf("remove doc: %v", err)
	}
	if got, err := w.QuerySessions("jieba", "", 20, "s1", ""); err != nil || len(got) != 0 {
		t.Fatalf("删除后仍命中：%+v err=%v", got, err)
	}

	// 会话级清理：只删 s1，s2 保留
	if removed, err := w.ClearSessions("s1"); err != nil || removed != 1 {
		t.Fatalf("clear s1 = %d, err=%v（期望 1）", removed, err)
	}
	if got, err := w.QuerySessions("全文索引", "", 20, "", ""); err != nil || len(got) != 1 {
		t.Fatalf("清 s1 后应只剩 s2 的 1 条：%+v err=%v", got, err)
	}
	if st := w.SessionStatus(); st.Sessions != 1 || st.Docs != 1 || st.State != "ready" {
		t.Fatalf("清理后状态不符：%+v", st)
	}

	// 整集合清理
	if _, err := w.ClearSessions(""); err != nil {
		t.Fatalf("clear all: %v", err)
	}
	if st := w.SessionStatus(); st.Docs != 0 || st.Sessions != 0 || st.State != "" {
		t.Fatalf("整集合清理后状态不符：%+v", st)
	}
}

// TestSessionIndexSurvivesFileRebuild：文件索引全量重建（Initialize）不得清空会话索引。
func TestSessionIndexSurvivesFileRebuild(t *testing.T) {
	dir := t.TempDir()
	writeText(t, dir, "a.txt", "hello vfts file index")

	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()

	if _, err := w.IndexSessions(seedSessionDocs(), nil); err != nil {
		t.Fatalf("index sessions: %v", err)
	}
	if _, err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("file initialize: %v", err)
	}
	// 再次全量重建（幂等）后会话索引仍在
	if _, err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("file initialize #2: %v", err)
	}
	hits, err := w.QuerySessions("全文索引", "", 20, "s1", "")
	if err != nil {
		t.Fatalf("query sessions after file rebuild: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("文件索引全量重建后会话索引被清空")
	}
	if st := w.SessionStatus(); st.Docs != 3 || st.State != "ready" {
		t.Fatalf("会话索引状态被文件重建影响：%+v", st)
	}
}

// TestSessionQueryNotInitialized：未建会话索引 → 查询返回 ErrSessionNotInitialized（结构化应答）。
func TestSessionQueryNotInitialized(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()
	if _, err := w.QuerySessions("任意", "", 20, "", ""); !errors.Is(err, ErrSessionNotInitialized) {
		t.Fatalf("未初始化应返回 ErrSessionNotInitialized，实际 err=%v", err)
	}
}

// TestRecordSessionFingerprints：对账指纹落 state.json（保留既有游标），写入索引不清指纹。
func TestRecordSessionFingerprints(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { Drop(dir); CloseAll() }()

	if _, err := w.IndexSessions(seedSessionDocs(), nil); err != nil {
		t.Fatalf("index: %v", err)
	}
	if err := RecordSessionFingerprints(dir, map[string]string{"s1": "fp-1"}); err != nil {
		t.Fatalf("record fingerprint: %v", err)
	}
	curs, err := SessionCursors(dir)
	if err != nil {
		t.Fatalf("cursors: %v", err)
	}
	if curs["s1"].Fingerprint != "fp-1" || curs["s1"].IndexedAt == 0 {
		t.Fatalf("指纹未落盘：%+v", curs["s1"])
	}
	if curs["s1"].IndexedUntilTurn != "t1" {
		t.Fatalf("既有游标被覆盖：%+v", curs["s1"])
	}
	if curs["s2"].Fingerprint != "" {
		t.Fatalf("未记录的会话不应有指纹：%+v", curs["s2"])
	}

	// 增量/重建继续写索引 → 指纹保留（对账不会因此反复重建）
	if _, err := w.IndexSessions(seedSessionDocs(), nil); err != nil {
		t.Fatalf("re-index: %v", err)
	}
	curs2, err := SessionCursors(dir)
	if err != nil {
		t.Fatalf("cursors #2: %v", err)
	}
	if curs2["s1"].Fingerprint != "fp-1" {
		t.Fatalf("重写索引不应清指纹：%+v", curs2["s1"])
	}
}
