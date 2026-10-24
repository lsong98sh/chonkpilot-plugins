// 记忆沉淀**队列 + 合并**白盒（2026-10-06，用户拍板）：
//
// 覆盖：
//   - 每 instance 一个串行 worker；订阅回调只置待处理标记（key = (instance, session)）；
//   - 同一 (instance, session) 多次事件**覆盖合并**（待处理表恒 ≤1 条，实跑 ≤2 次）；
//   - **无新轮 → 跳过**（不算失败；各类进度均已在最新轮 → 不调 LLM、不写回）；
//   - 进度**仅在保存成功后**推进（保存失败 → 不推进，下次重提该段）。
package memory

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// blockingHistory 桩 data-session-history：**首次**读阻塞至 release（制造在飞处理，
// 便于观察后续同会话事件的合并）。
type blockingHistory struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (b *blockingHistory) stub(t *testing.T, bus mq.Bus, turns []any) {
	t.Helper()
	reply(t, bus, sessionHistorySubject, func(map[string]any) map[string]any {
		b.mu.Lock()
		b.calls++
		n := b.calls
		b.mu.Unlock()
		if n == 1 {
			close(b.started)
			<-b.release
		}
		return map[string]any{"messages": map[string]any{"turns": turns, "messages": []any{}, "has_more": false}}
	})
}

func (b *blockingHistory) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// TestQueueMergesSameSession：同会话在飞期间多次事件 → 待处理表恒 1 条，实跑 2 次（首个 + 合并）。
func TestQueueMergesSameSession(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}}
	})
	stubSessionGet(t, bus, "")
	hist := &blockingHistory{started: make(chan struct{}), release: make(chan struct{})}
	hist.stub(t, bus, []any{map[string]any{"turn_id": "t1", "full_tokens": 100}})
	stubProgress(t, bus, map[string]string{})
	stubProgressSave(t, bus, nil)
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "P"}}}
	})
	stubTurnMessages(t, bus)
	stubRewriteReplies(t, bus)

	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	emit := func() {
		payload, _ := json.Marshal(map[string]any{
			"instance_id": "ins-1", "work_dir": `C:\ws`, "session": "s1", "turn": "t1",
		})
		_ = bus.Emit(context.Background(), turnEndSubject, payload)
	}
	emit()
	select {
	case <-hist.started: // 首个处理在飞（阻塞）
	case <-time.After(3 * time.Second):
		t.Fatal("首个处理未开始（worker 未拉起？）")
	}
	for i := 0; i < 5; i++ {
		emit() // 同会话多次事件 → 覆盖合并
	}
	// 合并断言：待处理表同 key 恰 1 条。
	key := sessionTaskKey("ins-1", "s1")
	p.mu.Lock()
	_, ok := p.pending[key]
	n := len(p.pending)
	p.mu.Unlock()
	if !ok || n != 1 {
		t.Fatalf("同会话事件应合并为 1 条待处理：hit=%v len=%d", ok, n)
	}
	close(hist.release)
	// 等 worker 排空。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		done := len(p.pending) == 0 && !p.running["ins-1"]
		p.mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := hist.count(); got != 2 {
		t.Fatalf("同会话 6 次事件应只处理 2 次（首个 + 合并的 1 次），实得 %d", got)
	}
}

// TestNoNewTurnSkips：各类进度均已在最新轮 → 无新轮 → 跳过（不算失败；不调 LLM、不写回）。
func TestNoNewTurnSkips(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}}
	})
	stubSessionGet(t, bus, "")
	stubSessionHistory(t, bus, []any{
		map[string]any{"turn_id": "t1", "full_tokens": 100},
		map[string]any{"turn_id": "t2", "full_tokens": 100},
	})
	stubProgress(t, bus, map[string]string{epKey("s1", "项目概要"): "t2"}) // 已在最新轮
	stubProgressSave(t, bus, nil)
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "P"}}}
	})
	var llmCalls, saves int32
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&llmCalls, 1)
		v.Result = map[string]any{"text": "x"}
		return nil
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any {
		atomic.AddInt32(&saves, 1)
		return map[string]any{"ok": true}
	})

	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	res := p.process(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t2"}, true)

	if res["ok"] != true {
		t.Fatalf("无新轮应成功跳过（ok:true）：%+v", res)
	}
	if saved, _ := res["saved"].([]string); len(saved) != 0 {
		t.Fatalf("无新轮不应有写入：%+v", res)
	}
	if n := atomic.LoadInt32(&llmCalls); n != 0 {
		t.Fatalf("无新轮不应调 LLM，实际 %d", n)
	}
	if n := atomic.LoadInt32(&saves); n != 0 {
		t.Fatalf("无新轮不应保存，实际 %d", n)
	}
}

// TestProgressNotAdvancedOnSaveFailure：保存失败 → 不推进进度（下次重提该段）。
func TestProgressNotAdvancedOnSaveFailure(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}}
	})
	stubSessionGet(t, bus, "")
	stubSessionHistory(t, bus, []any{map[string]any{"turn_id": "t1", "full_tokens": 100}})
	stubProgress(t, bus, map[string]string{})
	var progSaves int32
	stubProgressSave(t, bus, func(string, string) { atomic.AddInt32(&progSaves, 1) })
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "P"}}}
	})
	stubTurnMessages(t, bus)
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
	failReply(t, bus, memorySaveSubject) // 保存失败

	p := New(Options{MinTurnTokens: 1})
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if n := atomic.LoadInt32(&progSaves); n != 0 {
		t.Fatalf("保存失败不应推进进度，实际写进度 %d 次", n)
	}
}
