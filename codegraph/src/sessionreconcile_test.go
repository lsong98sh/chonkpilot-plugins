package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	vfts "github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/vfts/server"
)

// ─── 测试桩：会话数据面 + 引擎 ──────────────────────────────────────

// fakeSessionPlane 是会话数据面（plugin/v1/session.*）的内存实现。
type fakeSessionPlane struct {
	mu       sync.Mutex
	sessions []map[string]any            // data-session-list 行
	history  map[string][]map[string]any // session_id → 消息视图行
	calls    map[string]int
}

func (f *fakeSessionPlane) call(_ context.Context, method string, payload map[string]any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	switch method {
	case subjectSessionList:
		return json.Marshal(map[string]any{"list": f.sessions})
	case subjectSessionHistory:
		sid, _ := payload["session_id"].(string)
		msgs := f.history[sid]
		turns := make([]map[string]any, 0, len(msgs))
		seen := map[string]bool{}
		for _, m := range msgs {
			tid, _ := m["turn_id"].(string)
			if tid == "" || seen[tid] {
				continue
			}
			seen[tid] = true
			turns = append(turns, map[string]any{"turn_id": tid})
		}
		return json.Marshal(map[string]any{"messages": map[string]any{
			"turns": turns, "messages": msgs, "has_more": false,
		}})
	case subjectSessionGet:
		return json.Marshal(map[string]any{"data": map[string]any{"parent_id": ""}})
	}
	return nil, fmt.Errorf("未预期的能力调用 %s", method)
}

// fakeSessionEngine 记录引擎管理工具调用（vfts_session_*）。
type fakeSessionEngine struct {
	mu    sync.Mutex
	names []string
	args  []map[string]any
	state string // vfts_session_status 应答
}

func (f *fakeSessionEngine) call(_ context.Context, name string, a map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names = append(f.names, name)
	f.args = append(f.args, a)
	if name == "vfts_session_status" {
		return map[string]any{"state": f.state}, nil
	}
	return map[string]any{"ok": true}, nil
}

func (f *fakeSessionEngine) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.names {
		if c == name {
			n++
		}
	}
	return n
}

// arg 返回某工具第 i 次调用的参数（不存在 → nil）。
func (f *fakeSessionEngine) arg(name string, i int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for idx, c := range f.names {
		if c != name {
			continue
		}
		if n == i {
			return f.args[idx]
		}
		n++
	}
	return nil
}

// newReconcileHarness 构造只依赖桩的对账环境。
func newReconcileHarness(t *testing.T, cursors map[string]vfts.SessionCursor) (*sessionIndexer, *fakeSessionPlane, *fakeSessionEngine, *[]map[string]string) {
	t.Helper()
	store := newFakeStore(map[string]string{KeyVftsSessions: "true", KeyVftsEnabled: "true"})
	m := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)
	idx := newSessionIndexer(m)
	m.sessions = idx
	eng := &fakeSessionEngine{state: "ready"}
	m.engine = eng.call
	plane := &fakeSessionPlane{history: map[string][]map[string]any{}}
	idx.dataCall = plane.call
	idx.cursorsFn = func(string) (map[string]vfts.SessionCursor, error) { return cursors, nil }
	recorded := []map[string]string{}
	idx.recordFn = func(_ string, fps map[string]string) error {
		cp := make(map[string]string, len(fps))
		for k, v := range fps {
			cp[k] = v
		}
		recorded = append(recorded, cp)
		return nil
	}
	return idx, plane, eng, &recorded
}

// sessionMsg 造一条可入索引的消息视图行。
func sessionMsg(id, turn, role, text string) map[string]any {
	return map[string]any{
		"message_id": id, "turn_id": turn, "role": role, "type": "text",
		"content": text, "created_at": "2026-11-04T10:00:00Z",
	}
}

// liveSession 造一条主会话行。
func liveSession(sid, dialog string) map[string]any {
	return map[string]any{"session_id": sid, "dialog_id": dialog, "parent_id": ""}
}

// ─── 对账用例 ────────────────────────────────────────────────────

// TestReconcileIndexesMissingSession：数据面有、引擎无 → 对账后补上并记指纹。
func TestReconcileIndexesMissingSession(t *testing.T) {
	idx, plane, eng, recorded := newReconcileHarness(t, map[string]vfts.SessionCursor{})
	msgs := []map[string]any{
		sessionMsg("m1", "t1", "user", "如何优化全文索引的分词"),
		sessionMsg("m2", "t1", "assistant", "换成 jieba 并重建索引"),
	}
	plane.sessions = []map[string]any{liveSession("s1", "d1")}
	plane.history["s1"] = msgs

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Missing != 1 || st.Rebuilt != 0 || st.Removed != 0 || st.Unchanged != 0 {
		t.Fatalf("对账计数不符：%+v", st)
	}
	if n := eng.count("vfts_session_clear"); n != 1 {
		t.Fatalf("应先清该会话旧 doc（clear 次数=%d）", n)
	}
	if n := eng.count("vfts_session_index"); n != 1 {
		t.Fatalf("应写入该会话索引（index 次数=%d）", n)
	}
	docs, _ := eng.arg("vfts_session_index", 0)["docs"].([]vfts.SessionDoc)
	if len(docs) != 2 {
		t.Fatalf("写入 doc 数 = %d，期望 2", len(docs))
	}
	want := sessionFingerprint(sessionDocsOf("s1", "d1", msgs))
	if got := (*recorded)[len(*recorded)-1]["s1"]; got != want {
		t.Fatalf("指纹未记录：got %q want %q", got, want)
	}
}

// TestReconcileRemovesDeletedSession：引擎有、数据面已删除 → 对账后索引里消失。
func TestReconcileRemovesDeletedSession(t *testing.T) {
	idx, plane, eng, _ := newReconcileHarness(t, map[string]vfts.SessionCursor{
		"s2": {Fingerprint: "deadbeef", IndexedAt: 1},
	})
	plane.sessions = []map[string]any{}

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Removed != 1 || st.Missing != 0 || st.Rebuilt != 0 {
		t.Fatalf("对账计数不符：%+v", st)
	}
	if n := eng.count("vfts_session_clear"); n != 1 {
		t.Fatalf("应清理已删除会话（clear 次数=%d）", n)
	}
	if got := eng.arg("vfts_session_clear", 0)["session_id"]; got != "s2" {
		t.Fatalf("清理的会话 = %v，期望 s2", got)
	}
	if n := eng.count("vfts_session_index"); n != 0 {
		t.Fatalf("无存活会话时不应写索引（index 次数=%d）", n)
	}
}

// TestReconcileRebuildsChangedSession：正文变了（指纹不一致）→ 重建该会话并更新指纹。
func TestReconcileRebuildsChangedSession(t *testing.T) {
	idx, plane, eng, recorded := newReconcileHarness(t, map[string]vfts.SessionCursor{
		"s1": {Fingerprint: "stale-fingerprint", IndexedUntilTurn: "t1"},
	})
	msgs := []map[string]any{sessionMsg("m1", "t1", "user", "改过的正文")}
	plane.sessions = []map[string]any{liveSession("s1", "d1")}
	plane.history["s1"] = msgs

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Rebuilt != 1 || st.Missing != 0 || st.Removed != 0 {
		t.Fatalf("对账计数不符：%+v", st)
	}
	if eng.count("vfts_session_clear") != 1 || eng.count("vfts_session_index") != 1 {
		t.Fatalf("变更会话应先删后插：clear=%d index=%d",
			eng.count("vfts_session_clear"), eng.count("vfts_session_index"))
	}
	want := sessionFingerprint(sessionDocsOf("s1", "d1", msgs))
	if got := (*recorded)[len(*recorded)-1]["s1"]; got != want {
		t.Fatalf("指纹未更新：got %q want %q", got, want)
	}
}

// TestReconcileNoopWhenFingerprintMatches：指纹一致 → 不重复重建（幂等），只刷新 indexedAt。
func TestReconcileNoopWhenFingerprintMatches(t *testing.T) {
	msgs := []map[string]any{sessionMsg("m1", "t1", "user", "稳定的正文")}
	fp := sessionFingerprint(sessionDocsOf("s1", "d1", msgs))
	idx, plane, eng, recorded := newReconcileHarness(t, map[string]vfts.SessionCursor{
		"s1": {Fingerprint: fp, IndexedUntilTurn: "t1", IndexedAt: 1},
	})
	plane.sessions = []map[string]any{liveSession("s1", "d1")}
	plane.history["s1"] = msgs

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Unchanged != 1 || st.Missing != 0 || st.Rebuilt != 0 || st.Removed != 0 {
		t.Fatalf("对账计数不符：%+v", st)
	}
	if n := eng.count("vfts_session_clear"); n != 0 {
		t.Fatalf("指纹一致不应清理（clear 次数=%d）", n)
	}
	if n := eng.count("vfts_session_index"); n != 0 {
		t.Fatalf("指纹一致不应重建（index 次数=%d）", n)
	}
	got := (*recorded)
	if len(got) != 1 || got[0]["s1"] != fp {
		t.Fatalf("应只刷新一次 indexedAt 且指纹不变：%+v", got)
	}
}

// TestReconcileYieldsWhenIncrementalPending：该 workdir 有增量待处理 → 本轮整体让行。
func TestReconcileYieldsWhenIncrementalPending(t *testing.T) {
	idx, plane, eng, recorded := newReconcileHarness(t, map[string]vfts.SessionCursor{})
	plane.sessions = []map[string]any{liveSession("s1", "d1")}
	plane.history["s1"] = []map[string]any{sessionMsg("m1", "t1", "user", "x")}
	idx.mu.Lock()
	idx.pending[normalizeWorkdir(`E:\proj`)+"|s1"] = &sessionTurnTask{SessionID: "s1", Turns: []string{"t1"}}
	idx.mu.Unlock()

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Sessions != 0 || st.Missing != 0 || st.Rebuilt != 0 || st.Removed != 0 {
		t.Fatalf("增量待处理时本轮应让行：%+v", st)
	}
	if len(eng.names) != 0 {
		t.Fatalf("让行时不应触达引擎：%v", eng.names)
	}
	if len(*recorded) != 0 {
		t.Fatalf("让行时不应记录指纹：%+v", *recorded)
	}
}

// TestReconcileSkipsWhenEngineNotReady：会话索引未就绪（未初始化 / 建库中）→ 本轮不动。
func TestReconcileSkipsWhenEngineNotReady(t *testing.T) {
	idx, plane, eng, _ := newReconcileHarness(t, map[string]vfts.SessionCursor{})
	eng.state = ""
	plane.sessions = []map[string]any{liveSession("s1", "d1")}
	plane.history["s1"] = []map[string]any{sessionMsg("m1", "t1", "user", "x")}

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Sessions != 0 || st.Missing != 0 {
		t.Fatalf("引擎未就绪时不应索引：%+v", st)
	}
	for _, n := range eng.names {
		if n != "vfts_session_status" {
			t.Fatalf("未就绪时只允许查状态，实际调用 %v", eng.names)
		}
	}
	if len(eng.names) != 1 {
		t.Fatalf("应只查一次状态：%v", eng.names)
	}
}

// TestReconcileIgnoresSubSessions：子会话（parent_id 非空）不参与对账。
func TestReconcileIgnoresSubSessions(t *testing.T) {
	idx, plane, eng, _ := newReconcileHarness(t, map[string]vfts.SessionCursor{})
	plane.sessions = []map[string]any{{"session_id": "sub", "dialog_id": "d1", "parent_id": "s1"}}

	st := idx.reconcileWorkdir(context.Background(), `E:\proj`, "inst-1")
	if st.Sessions != 0 || eng.count("vfts_session_index") != 0 {
		t.Fatalf("子会话不应进索引：%+v calls=%v", st, eng.names)
	}
}

// TestSessionFingerprintStable：指纹与消息顺序无关，正文 / 角色 / id 变化即变。
func TestSessionFingerprintStable(t *testing.T) {
	a := []vfts.SessionDoc{
		{SessionID: "s1", MessageID: "m1", Role: "user", Text: "第一段"},
		{SessionID: "s1", MessageID: "m2", Role: "assistant", Text: "第二段"},
	}
	b := []vfts.SessionDoc{a[1], a[0]} // 顺序反转
	if sessionFingerprint(a) != sessionFingerprint(b) {
		t.Fatal("指纹应与消息顺序无关")
	}
	changedText := []vfts.SessionDoc{a[0], {SessionID: "s1", MessageID: "m2", Role: "assistant", Text: "第二段（改）"}}
	if sessionFingerprint(a) == sessionFingerprint(changedText) {
		t.Fatal("正文变化必须改变指纹")
	}
	changedRole := []vfts.SessionDoc{a[0], {SessionID: "s1", MessageID: "m2", Role: "user", Text: "第二段"}}
	if sessionFingerprint(a) == sessionFingerprint(changedRole) {
		t.Fatal("角色变化必须改变指纹")
	}
	changedID := []vfts.SessionDoc{a[0], {SessionID: "s1", MessageID: "m3", Role: "assistant", Text: "第二段"}}
	if sessionFingerprint(a) == sessionFingerprint(changedID) {
		t.Fatal("消息 id 变化必须改变指纹")
	}
	if sessionFingerprint(nil) == sessionFingerprint(a) {
		t.Fatal("空集合与有内容的指纹必须不同")
	}
}
