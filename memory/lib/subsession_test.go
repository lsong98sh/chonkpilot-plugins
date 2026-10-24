// 子会话沉淀门控（DSL-4，42 §2 (253)「提取子会话记忆」，默认关闭）：
//   - 判定 = 会话行 parent_id != ""（经 data-session-get.parent_id，与 OP-07 同一读取）；
//   - 子会话且开关未开 → 跳过自动沉淀（不调 LLM、不写记忆）；主会话恒不跳过。
package memory

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// stubSubsessionDistill 桩齐沉淀回路最小应答集（parentID 决定主/子会话），返回保存计数。
func stubSubsessionDistill(t *testing.T, bus mq.Bus, prjList map[string]any, parentID string) *int32 {
	t.Helper()
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": prjList}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 300)},
		}}
	})
	stubSessionGet(t, bus, parentID)
	reply(t, bus, sessionHistorySubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": map[string]any{"turns": []any{map[string]any{"turn_id": "t1"}}, "messages": []any{}, "has_more": false}}
	})
	stubProgress(t, bus, map[string]string{})
	stubProgressSave(t, bus, nil)
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "P"},
		}}
	})
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	var saved int32
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any {
		atomic.AddInt32(&saved, 1)
		return map[string]any{"ok": true}
	})
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
	return &saved
}

// TestSubsessionGateSkipsAutoDistill：子会话 + 开关未开（默认）→ 跳过（不写记忆）；
// 子会话 + 开关开启 → 照常沉淀；主会话（开关未开）→ 照常沉淀。
func TestSubsessionGateSkipsAutoDistill(t *testing.T) {
	// ① 子会话 + 开关缺失（默认关）→ 跳过
	bus := newTestBus(t)
	saved := stubSubsessionDistill(t, bus,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}, "s-main")
	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	res := p.process(turnEvent{InstanceID: "ins", WorkDir: `C:\ws`, Session: "s-sub", LastTurn: "t1"}, false)
	if res["subsession_skipped"] != true {
		t.Fatalf("子会话默认应跳过沉淀：res=%+v", res)
	}
	if n := atomic.LoadInt32(saved); n != 0 {
		t.Fatalf("子会话默认不应写记忆：保存次数 = %d，want 0", n)
	}

	// ② 子会话 + 开关开启 → 不跳过（照常沉淀）
	bus2 := newTestBus(t)
	saved2 := stubSubsessionDistill(t, bus2,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1", memorySubsessionKey: "true"}, "s-main")
	p2 := New(DefaultOptions())
	p2.deps = plugin.Deps{Bus: bus2}
	if res := p2.process(turnEvent{InstanceID: "ins", WorkDir: `C:\ws`, Session: "s-sub", LastTurn: "t1"}, false); res["subsession_skipped"] == true {
		t.Fatalf("子会话开关开启不应跳过：res=%+v", res)
	}
	if n := atomic.LoadInt32(saved2); n != 1 {
		t.Fatalf("子会话开关开启应沉淀：保存次数 = %d，want 1", n)
	}

	// ③ 主会话（开关未开）→ 不跳过（照常沉淀）
	bus3 := newTestBus(t)
	saved3 := stubSubsessionDistill(t, bus3,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}, "")
	p3 := New(DefaultOptions())
	p3.deps = plugin.Deps{Bus: bus3}
	if res := p3.process(turnEvent{InstanceID: "ins", WorkDir: `C:\ws`, Session: "s-main", LastTurn: "t1"}, false); res["subsession_skipped"] == true {
		t.Fatalf("主会话不应跳过：res=%+v", res)
	}
	if n := atomic.LoadInt32(saved3); n != 1 {
		t.Fatalf("主会话应沉淀：保存次数 = %d，want 1", n)
	}
}

// TestWorkerSkipsSubsessionNotice：worker 路径（真订阅 `session-complete`）：子会话 + 开关未开 →
// **不广播 `memory-*` 进度**（子会话不入沉淀队列状态展示，见 I-128）、不写记忆。
func TestWorkerSkipsSubsessionNotice(t *testing.T) {
	bus := newTestBus(t)
	saved := stubSubsessionDistill(t, bus,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}, "s-main")
	var notices int32
	_, _ = bus.On("tool-notify", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil {
			return nil
		}
		if m["plugin"] == "memory" {
			atomic.AddInt32(&notices, 1)
		}
		return nil
	})

	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"instance_id": "ins", "work_dir": `C:\ws`, "session": "s-sub", "turn": "t1",
	})
	bus.Emit(context.Background(), turnEndSubject, body).Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		done := len(p.pending) == 0 && !p.running["ins"]
		p.mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := atomic.LoadInt32(saved); n != 0 {
		t.Fatalf("子会话 + 开关未开不应写记忆：保存次数 = %d，want 0", n)
	}
	if n := atomic.LoadInt32(&notices); n != 0 {
		t.Fatalf("子会话跳过不应广播 memory-* 进度：条数 = %d，want 0", n)
	}
}
