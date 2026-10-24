// 手动沉淀（memory.flush）白盒：用户显式触发一次沉淀，**投递即回**（I-128）。
//
// 覆盖：
//   - 回执：立即返回 `{ok, queued, session}`（不再同步等各类别 LLM 跑完）；
//   - 异步处理：按启用类别**各沉淀一次**（可观测：data-memory-save / llm-simple 次数）；
//   - 进度：处理经通知面 tool-notify 广播 `memory-start` / `memory-done`；
//   - 不受 memory.min-turn-tokens 门控（同一配置下自动沉淀仍被阈值挡住 → 差异只属手动）；
//   - 作用域 = payload 指定的 instance/session（取该会话**最近一轮** data-session-history）；
//   - 前置不满足（缺 session）→ `{ok:false, reason}` 明确作答。
package memory

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// flushStub 记录一次手动沉淀链路里各数据面/LLM 的调用次数与载荷。
type flushStub struct {
	saves    int32
	llmCalls int32
	mu       sync.Mutex
	prompts  map[string]string // 类别 → 沉淀 prompt（含旧全文 + 本轮新信息）
	history  []map[string]any  // data-session-history 收到的 data 载荷
}

// wire 装配最小桩：配置 / 最近轮次 / 本轮消息 / 类别清单 / 读写 / LLM。
func (s *flushStub) wire(t *testing.T, bus mq.Bus, cfg map[string]any, turns []any) {
	t.Helper()
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": cfg}
	})
	reply(t, bus, sessionHistorySubject, func(data map[string]any) map[string]any {
		s.mu.Lock()
		s.history = append(s.history, data)
		s.mu.Unlock()
		return map[string]any{"messages": map[string]any{"turns": turns, "messages": []any{}, "has_more": false}}
	})
	stubSessionGet(t, bus, "") // 主会话（自动沉淀反证路径需要）
	stubProgress(t, bus, map[string]string{})
	stubProgressSave(t, bus, nil)
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "甲甲甲"},
			map[string]any{"role": "assistant", "content": "乙乙乙"},
		}}
	})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "提示词-项目概要"},
			map[string]any{"category": "开发规范", "level": "project", "prompt": "提示词-开发规范"},
			map[string]any{"category": "用户偏好", "level": "user", "prompt": "提示词-用户偏好"},
		}}
	})
	reply(t, bus, memoryReadSubject, func(data map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文-" + strval(data["category"])}}
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any {
		atomic.AddInt32(&s.saves, 1)
		return map[string]any{"ok": true}
	})
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&s.llmCalls, 1)
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		s.mu.Lock()
		for _, cat := range []string{"项目概要", "开发规范", "用户偏好"} {
			if strings.Contains(req.Prompt, "【类别】"+cat) {
				s.prompts[cat] = req.Prompt
			}
		}
		s.mu.Unlock()
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
}

// noticeLog 收集 tool-notify 的 notice 取值（进度断言用）。
type noticeLog struct {
	mu   sync.Mutex
	list []string
	ch   chan string
}

func watchNotices(t *testing.T, bus mq.Bus) *noticeLog {
	t.Helper()
	n := &noticeLog{ch: make(chan string, 64)}
	if _, err := bus.On("tool-notify", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil {
			return nil
		}
		s := strval(m["notice"])
		n.mu.Lock()
		n.list = append(n.list, s)
		n.mu.Unlock()
		select {
		case n.ch <- s:
		default:
		}
		return nil
	}); err != nil {
		t.Fatalf("sub tool-notify: %v", err)
	}
	return n
}

// waitNotice 等到指定 notice（或超时）。
func (n *noticeLog) waitNotice(t *testing.T, want string) bool {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-n.ch:
			if s == want {
				return true
			}
		case <-deadline:
			n.mu.Lock()
			defer n.mu.Unlock()
			t.Fatalf("等 notice=%s 超时（已见 %v）", want, n.list)
			return false
		}
	}
}

// TestManualFlushQueuedThenDistills：memory.flush **投递即回** → 异步按启用类别各沉淀一次；
// 进度经 tool-notify 广播（memory-start / memory-done）；阈值极大也照沉淀。
func TestManualFlushQueuedThenDistills(t *testing.T) {
	bus := newTestBus(t)
	st := &flushStub{prompts: map[string]string{}}
	// 阈值极大：自动沉淀必被挡住（下面用 extract 反证差异只属手动触发）。
	st.wire(t, bus, map[string]any{
		memoryEnabledKey:              "true",
		memoryMinTokensKey:            "999999",
		memoryCategoryPrefix + "开发规范": "false", // 显式关闭 → 不应写入
	}, []any{
		map[string]any{"turn_id": "t-1"},
		map[string]any{"turn_id": "t-2"}, // 最近一轮
	})

	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	notices := watchNotices(t, bus)

	v := bus.Emit(context.Background(), "memory.flush", map[string]any{
		"instance_id": "ins-1", "session": "s1",
	}).Wait()
	if err := v.Err(); err != nil {
		t.Fatalf("memory.flush 派发错误：%v", err)
	}
	res, _ := v.Result.(map[string]any)
	if res == nil {
		t.Fatalf("memory.flush 应写回结果，实际 %#v", v.Result)
	}
	if ok, _ := res["ok"].(bool); !ok {
		t.Fatalf("手动沉淀应受理：%+v", res)
	}
	// **投递即回**：回执只有 {ok, queued, session}（不含 saved/turn/enabled——异步处理）
	if queued, _ := res["queued"].(bool); !queued {
		t.Fatalf("回执应 queued=true：%+v", res)
	}
	if _, has := res["saved"]; has {
		t.Fatalf("投递即回不应带 saved（异步处理）：%+v", res)
	}
	if res["session"] != "s1" {
		t.Fatalf("回执应带回 session：%+v", res)
	}

	// 进度：等异步处理完成（memory-start → memory-done）
	notices.waitNotice(t, "memory-start")
	notices.waitNotice(t, "memory-done")

	// 可观测效果：一次触发 = 每启用类别恰好一次 save / 一次 llm-simple（不重复触发）
	if n := atomic.LoadInt32(&st.saves); n != 2 {
		t.Fatalf("data-memory-save 次数应为 2（开发规范已关闭），实际 %d", n)
	}
	if n := atomic.LoadInt32(&st.llmCalls); n != 2 {
		t.Fatalf("llm-simple 次数应为 2，实际 %d", n)
	}
	if pr, ok := st.prompts["项目概要"]; !ok || !strings.Contains(pr, "旧全文-项目概要") || !strings.Contains(pr, "甲甲甲") {
		t.Fatalf("沉淀 prompt 应含旧全文与本轮新信息：%q", pr)
	}
	if _, ok := st.prompts["开发规范"]; ok {
		t.Fatalf("已关闭类别不应被写入")
	}
	// 作用域：仅按 payload 的 session 取轮次（一次）
	if len(st.history) != 1 || st.history[0]["session_id"] != "s1" {
		t.Fatalf("data-session-history 请求应恰一次且限定该会话：%+v", st.history)
	}

	// 反证：同一配置下自动沉淀（session-complete 路径）被阈值挡住 →「不受阈值门控」只属手动触发
	before := atomic.LoadInt32(&st.llmCalls)
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t-2"})
	if n := atomic.LoadInt32(&st.llmCalls); n != before {
		t.Fatalf("阈值 999999 下自动沉淀不应调 LLM（%d → %d）", before, n)
	}
}

// TestManualFlushQueuedNotice：实例 worker 忙时再次入队 → 广播 `memory-queued`（排队态）。
func TestManualFlushQueuedNotice(t *testing.T) {
	bus := newTestBus(t)
	notices := watchNotices(t, bus)
	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 模拟该实例 worker 已在运行（占住 running）→ 后续 enqueue 只能排队
	p.mu.Lock()
	p.running = map[string]bool{"ins-1": true}
	p.mu.Unlock()

	p.enqueue(turnEvent{InstanceID: "ins-1", Session: "s1", LastTurn: "t-1"}, false)

	if !notices.waitNotice(t, "memory-queued") {
		t.Fatal("worker 忙时应广播 memory-queued")
	}
}

// TestManualFlushGuards：前置不满足 → `{ok:false, reason}` 明确作答（不静默、不误报成功）。
func TestManualFlushGuards(t *testing.T) {
	bus := newTestBus(t)
	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	v := bus.Emit(context.Background(), "memory.flush", map[string]any{"instance_id": "ins-1"}).Wait()
	res, _ := v.Result.(map[string]any)
	if res == nil || res["ok"] != false {
		t.Fatalf("缺 session 应回 ok:false，实际 %#v", v.Result)
	}
	if !strings.Contains(strval(res["reason"]), "instance_id/session required") {
		t.Fatalf("原因应含 instance_id/session required，实际 %q", res["reason"])
	}
}

// TestManualFlushDisabledMemoryNoWrites：记忆库未启用 → 仍投递即回（queued），但**不产生任何写入**。
func TestManualFlushDisabledMemoryNoWrites(t *testing.T) {
	bus := newTestBus(t)
	st := &flushStub{prompts: map[string]string{}}
	st.wire(t, bus, map[string]any{}, []any{map[string]any{"turn_id": "t-1"}}) // 未启用
	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	notices := watchNotices(t, bus)

	v := bus.Emit(context.Background(), "memory.flush", map[string]any{
		"instance_id": "ins-1", "session": "s1",
	}).Wait()
	res, _ := v.Result.(map[string]any)
	if res == nil || res["ok"] != true {
		t.Fatalf("投递即回应为 ok:true：%#v", v.Result)
	}
	notices.waitNotice(t, "memory-done")
	if n := atomic.LoadInt32(&st.saves); n != 0 {
		t.Fatalf("未启用不应写入任何类别，实际 save %d 次", n)
	}
	if n := atomic.LoadInt32(&st.llmCalls); n != 0 {
		t.Fatalf("未启用不应调 LLM，实际 %d 次", n)
	}
}

// TestManualFlushLocksOnInstanceWorkDir：手动沉淀（memory.flush）载荷**不含 work_dir**，
// 但锁键须经实例视图解析回 work_dir，与自动沉淀（session-complete 带 work_dir）落在**同一把**
// per-(workdir, 类别) 锁上（OP-08）——否则同一 (项目, 类别) 的读-改-写并发会丢更新。
func TestManualFlushLocksOnInstanceWorkDir(t *testing.T) {
	bus := newTestBus(t)
	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	manual := turnEvent{InstanceID: "ins-1", Session: "s1"}                  // 手动载荷：无 work_dir
	auto := turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1"} // 自动载荷：带 work_dir

	// 未登记实例：手动载荷无 work_dir → 解析为空（与自动路径不同锁 = 缺陷场景）。
	if got := p.resolveWorkDir(manual); got != "" {
		t.Fatalf("未登记实例时手动 work_dir 应为空，实际 %q", got)
	}

	// 宿主登记实例（instance-register）→ 两路径收敛到同一 work_dir（同一把锁）。
	if err := bus.Emit(context.Background(), "instance-register", map[string]any{
		"instance_id": "ins-1", "work_dir": `C:\ws`,
	}).Wait().Err(); err != nil {
		t.Fatalf("emit instance-register: %v", err)
	}
	wa, wm := p.resolveWorkDir(auto), p.resolveWorkDir(manual)
	if wa != `C:\ws` || wm != wa {
		t.Fatalf("登记后两路径应收敛到同一 work_dir：auto=%q manual=%q", wa, wm)
	}

	// 锁表验证：同 (workdir, 类别) 只落一把互斥体。
	unlock := p.lockCategory(wm, "项目概要")
	n := 0
	p.locks.Range(func(_, _ any) bool { n++; return true })
	unlock()
	if n != 1 {
		t.Fatalf("同 (workdir, 类别) 应只有 1 把锁，实际 %d", n)
	}
}
