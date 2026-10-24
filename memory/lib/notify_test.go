// 记忆沉淀失败的**用户可见上报**（memory.notify，2026-09-20 B 批）白盒：
// 失败路径 → ① 日志行含失败原因（落 gui.log 的那条）；② 经 Deps.Notify 上报一次
// （插件/类别/实例/会话/轮次/原因齐备）；非失败路径（阈值跳过/未启用）**不上报**；
// 未注入 Notify（缺省）→ 行为同改前（静默，不 panic）。
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// noticeSpy 记录插件上报的用户可见提示。
type noticeSpy struct {
	mu   sync.Mutex
	list []plugin.Notice
}

func (s *noticeSpy) fn(n plugin.Notice) {
	s.mu.Lock()
	s.list = append(s.list, n)
	s.mu.Unlock()
}

func (s *noticeSpy) snapshot() []plugin.Notice {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]plugin.Notice(nil), s.list...)
}

// failReply 模拟 persist 的**失败**应答（ok=false → 调用方按失败处理）。
func failReply(t *testing.T, bus mq.Bus, subject string) {
	t.Helper()
	_, err := bus.On(subject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req struct {
			ReqID string `json:"req_id"`
			OK    *bool  `json:"ok"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		if req.ReqID == "" || req.OK != nil {
			return nil // 非请求载荷 / 应答回环 忽略
		}
		body, _ := json.Marshal(map[string]any{"req_id": req.ReqID, "ok": false})
		_ = bus.Emit(context.Background(), subject, body)
		return nil
	})
	if err != nil {
		t.Fatalf("stub %s: %v", subject, err)
	}
}

// TestNotifyOnSaveFailure：保存失败 → 日志留因 + 上报一次（字段齐备）。
func TestNotifyOnSaveFailure(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{
			memoryEnabledKey:   "true",
			memoryMinTokensKey: "1",
		}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 200)},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "开发规范", "level": "project", "prompt": "提示词-开发规范"}}}
	})
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
	failReply(t, bus, memorySaveSubject) // 保存失败

	var logged []string
	spy := &noticeSpy{}
	p := New(Options{MinTurnTokens: 1})
	p.deps = plugin.Deps{
		Bus:    bus,
		Logf:   func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
		Notify: spy.fn,
	}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	// ① 失败原因进日志（同一字符串同时用于日志与上报）
	var saveLog string
	for _, l := range logged {
		if strings.Contains(l, "保存记忆失败（开发规范）") {
			saveLog = l
		}
	}
	if saveLog == "" {
		t.Fatalf("应记录保存失败日志，实际 %v", logged)
	}
	if !strings.Contains(saveLog, "data-memory-save") {
		t.Fatalf("失败日志应含可诊断原因（主题名），实际 %q", saveLog)
	}

	// ② 上报一次（字段齐备、原因含主题名）
	got := spy.snapshot()
	if len(got) != 1 {
		t.Fatalf("保存失败应上报一次，实际 %d：%+v", len(got), got)
	}
	n := got[0]
	if n.Plugin != "memory" || n.Kind != "save" {
		t.Fatalf("上报字段不符：%+v", n)
	}
	if n.InstanceID != "ins-1" || n.Session != "s1" || n.Turn != "t1" {
		t.Fatalf("上报归属字段不符（判重键）：%+v", n)
	}
	if !strings.Contains(n.Reason, "data-memory-save") || !strings.Contains(n.Reason, "开发规范") {
		t.Fatalf("上报原因应含类别与主题（可理解/可查）：%q", n.Reason)
	}
}

// TestNotifyPerFailedCategory：同轮多类别失败 → 逐类上报（**宿主**按 轮次×插件×类别 去重，
// 见 chonkpilot-llm/server/pluginnotice.go；插件只如实上报，不自行限频）。
func TestNotifyPerFailedCategory(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 200)},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "提示词-项目概要"},
			map[string]any{"category": "开发规范", "level": "project", "prompt": "提示词-开发规范"},
		}}
	})
	failReply(t, bus, memoryReadSubject) // 读记忆失败（两类都失败）

	spy := &noticeSpy{}
	p := New(Options{MinTurnTokens: 1})
	p.deps = plugin.Deps{Bus: bus, Notify: spy.fn}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	got := spy.snapshot()
	if len(got) != 2 {
		t.Fatalf("两个类别失败应各上报一次（宿主去重），实际 %d：%+v", len(got), got)
	}
	for _, n := range got {
		if n.Kind != "read" {
			t.Fatalf("失败类别应为 read（读记忆失败）：%+v", n)
		}
		if !strings.Contains(n.Reason, "data-memory-read") {
			t.Fatalf("原因应含失败主题：%q", n.Reason)
		}
	}
}

// TestNoNotifyOnNonFailure：阈值跳过（非失败）→ 不上报（反打扰：只有失败才提示）。
func TestNoNotifyOnNonFailure(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1000"}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{map[string]any{"role": "user", "content": "你好"}}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "提示词-项目概要"}}}
	})
	spy := &noticeSpy{}
	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus, Notify: spy.fn}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})
	if got := spy.snapshot(); len(got) != 0 {
		t.Fatalf("非失败路径不应上报：%+v", got)
	}
}

// TestNotifyNilSafe：未注入 Notify（缺省）→ 与改前一致（只记日志、不 panic）。
func TestNotifyNilSafe(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 200)},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "提示词-项目概要"}}}
	})
	failReply(t, bus, memoryReadSubject) // 失败路径 ⇒ 会调 notify（未注入 → 必须静默）

	var logged []string
	p := New(Options{MinTurnTokens: 1})
	p.deps = plugin.Deps{
		Bus:  bus,
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "读记忆失败（项目概要）") {
		t.Fatalf("缺省（未注入 Notify）应仍记日志，实际 %v", logged)
	}
}
