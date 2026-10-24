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

// reply 模拟 persist 的应答：向请求同主题 Emit {req_id, ok, result}。
func reply(t *testing.T, bus mq.Bus, subject string, result func(req map[string]any) map[string]any) {
	t.Helper()
	_, err := bus.On(subject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req struct {
			ReqID string         `json:"req_id"`
			OK    *bool          `json:"ok"`
			Data  map[string]any `json:"data"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		if req.ReqID == "" || req.OK != nil {
			return nil // 非请求载荷 或 应答回环 忽略
		}
		body, _ := json.Marshal(map[string]any{
			"req_id": req.ReqID, "ok": true, "result": result(req.Data),
		})
		_ = bus.Emit(context.Background(), subject, body)
		return nil
	})
	if err != nil {
		t.Fatalf("stub %s: %v", subject, err)
	}
}

// newTestBusRaw 建测试总线（不加任何默认应答）。
func newTestBusRaw(t *testing.T) mq.Bus {
	t.Helper()
	bus, err := mq.New(mq.Options{Prefix: "chonk."})
	if err != nil {
		t.Fatalf("mq.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	return bus
}

// stubUserConfig 让 `data-user-config-load` 立即应答指定 usr 配置（SL-3：沉淀会读 usr `llm.memory`；
// 无应答要等满 dataTimeout，拖慢用例）。需自定义配置时用 newTestBusRaw + 本函数。
func stubUserConfig(t *testing.T, bus mq.Bus, cfg map[string]any) {
	t.Helper()
	reply(t, bus, userConfigLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": cfg}
	})
}

// newTestBus 建测试总线 + 默认空 usr 配置应答（避免每次沉淀无谓等待）。
func newTestBus(t *testing.T) mq.Bus {
	t.Helper()
	bus := newTestBusRaw(t)
	stubUserConfig(t, bus, map[string]any{})
	return bus
}

// stubSessionGet 桩 data-session-get：parentID 空 = 主会话；非空 = 子会话（OP-07）。
func stubSessionGet(t *testing.T, bus mq.Bus, parentID string) {
	t.Helper()
	reply(t, bus, sessionGetSubject, func(map[string]any) map[string]any {
		row := map[string]any{"session_id": "s"}
		if parentID != "" {
			row["parent_id"] = parentID
		}
		return map[string]any{"data": row}
	})
}

// stubSessionHistory 桩 data-session-history：返回给定轮列表（升序；每项形如
// {"turn_id":"t1"} 或带 "full_tokens"），供跨轮范围/累计门控使用。
func stubSessionHistory(t *testing.T, bus mq.Bus, turns []any) {
	t.Helper()
	reply(t, bus, sessionHistorySubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": map[string]any{"turns": turns, "messages": []any{}, "has_more": false}}
	})
}

// epKey 测试用进度键：<会话>\x00<类别>（对齐专用表 memory_extract 主键）。
func epKey(session, category string) string { return session + "\x00" + category }

// stubProgress 桩 data-memory-extract-load（提取进度专用表）：按 (session, category) 返回
// vals[epKey(session,category)]（缺省空串 = 无进度）。Category 空 → 返回该会话全部类别。
func stubProgress(t *testing.T, bus mq.Bus, vals map[string]string) {
	t.Helper()
	reply(t, bus, memoryExtractLoadSubject, func(data map[string]any) map[string]any {
		sid := strval(data["session_id"])
		cat := strval(data["category"])
		list := []any{}
		add := func(s, c, v string) {
			if v != "" {
				list = append(list, map[string]any{"session_id": s, "category": c, "last_turn_id": v})
			}
		}
		if cat != "" {
			add(sid, cat, vals[epKey(sid, cat)])
		} else {
			for k, v := range vals {
				if i := strings.IndexByte(k, '\x00'); i > 0 && k[:i] == sid {
					add(sid, k[i+1:], v)
				}
			}
		}
		return map[string]any{"list": list}
	})
}

// stubProgressSave 桩 data-memory-extract-save（进度写）：可选 sink 记录 (key,value)，
// key = epKey(session, category)。
func stubProgressSave(t *testing.T, bus mq.Bus, sink func(key, value string)) {
	t.Helper()
	reply(t, bus, memoryExtractSaveSubject, func(data map[string]any) map[string]any {
		if sink != nil {
			sink(epKey(strval(data["session_id"]), strval(data["category"])), strval(data["last_turn_id"]))
		}
		return map[string]any{"ok": true}
	})
}

// stubExtractFaces 桩齐 extract 新增依赖面（OP-05/07）：主会话 + 给定轮列表 + 空进度。
func stubExtractFaces(t *testing.T, bus mq.Bus, turns []any) {
	t.Helper()
	stubSessionGet(t, bus, "")
	stubSessionHistory(t, bus, turns)
	stubProgress(t, bus, map[string]string{})
	stubProgressSave(t, bus, nil)
}

// TestThresholdGateSkipsLLM：累计新增 token 低于阈值（寒暄类）→ 不调 LLM、也不读写记忆。
func TestThresholdGateSkipsLLM(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true"}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
			map[string]any{"role": "assistant", "content": "你能做什么"},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	var llmCalls, readCalls, saveCalls int32
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&llmCalls, 1)
		v.Result = map[string]any{"text": "x"}
		return nil
	})
	_, _ = bus.On(memoryReadSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&readCalls, 1)
		return nil
	})
	_, _ = bus.On(memorySaveSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&saveCalls, 1)
		return nil
	})

	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if n := atomic.LoadInt32(&llmCalls); n != 0 {
		t.Fatalf("低于阈值不应调 LLM，实际调用 %d 次", n)
	}
	if n := atomic.LoadInt32(&readCalls); n != 0 {
		t.Fatalf("低于阈值不应读记忆全文，实际调用 %d 次", n)
	}
	if n := atomic.LoadInt32(&saveCalls); n != 0 {
		t.Fatalf("低于阈值不应保存记忆，实际调用 %d 次", n)
	}
}

// TestExtractRewritesEnabledCategories：过阈值 → 仅按启用类别并行调 LLM 重写；
// 显式关闭的类别跳过；用户偏好未配置关闭 → 默认启用（同此口径）。
func TestExtractRewritesEnabledCategories(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{
			memoryEnabledKey:              "true",
			memoryMinTokensKey:            "1",
			memoryCategoryPrefix + "开发规范": "false",
		}}
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
			map[string]any{"category": "用户偏好", "level": "user", "prompt": "提示词-用户偏好"},
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
	var llmCalls int32
	var calledMu sync.Mutex
	called := map[string]bool{}
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&llmCalls, 1)
		var req struct {
			Prompt     string `json:"prompt"`
			InstanceID string `json:"instance_id"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		// 61-消息一览 §0：llm-simple 请求须带实例 id（透传触发事件 session-complete 的 instance_id）。
		if req.InstanceID != "ins-1" {
			t.Errorf("llm-simple 请求缺 instance_id：%q", req.InstanceID)
		}
		calledMu.Lock()
		if strings.Contains(req.Prompt, "【类别】项目概要") {
			called["项目概要"] = true
		}
		if strings.Contains(req.Prompt, "【类别】开发规范") {
			called["开发规范"] = true
		}
		if strings.Contains(req.Prompt, "【类别】用户偏好") {
			called["用户偏好"] = true
		}
		calledMu.Unlock()
		if !strings.Contains(req.Prompt, "旧全文") {
			t.Errorf("prompt 应含旧全文：%s", req.Prompt)
		}
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})

	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus}
	done := make(chan struct{})
	go func() {
		p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("extract 超时")
	}

	if n := atomic.LoadInt32(&llmCalls); n != 2 {
		t.Fatalf("应仅对 2 个启用类别调 LLM，实际 %d（called=%v）", n, called)
	}
	calledMu.Lock()
	has项目概要, has开发规范, has用户偏好 := called["项目概要"], called["开发规范"], called["用户偏好"]
	calledMu.Unlock()
	if !has项目概要 || !has用户偏好 {
		t.Fatalf("启用类别未全部重写：%v", called)
	}
	if has开发规范 {
		t.Fatalf("已关闭类别不应重写：%v", called)
	}
	if n := atomic.LoadInt32(&saved); n != 2 {
		t.Fatalf("应保存 2 个类别，实际 %d", n)
	}
}

// TestUserPrefCategoryToggle：用户偏好（唯一用户级）可经 memory.category.用户偏好 显式关闭。
func TestUserPrefCategoryToggle(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{
			memoryEnabledKey:              "true",
			memoryMinTokensKey:            "1",
			memoryCategoryPrefix + "用户偏好": "false",
		}}
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
			map[string]any{"category": "用户偏好", "level": "user", "prompt": "提示词-用户偏好"},
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
	var llmCalls int32
	called := map[string]bool{}
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&llmCalls, 1)
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		if strings.Contains(req.Prompt, "【类别】项目概要") {
			called["项目概要"] = true
		}
		if strings.Contains(req.Prompt, "【类别】用户偏好") {
			called["用户偏好"] = true
		}
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})

	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if n := atomic.LoadInt32(&llmCalls); n != 1 || !called["项目概要"] {
		t.Fatalf("关闭用户偏好后应仅重写项目级类别，实际 %d（called=%v）", n, called)
	}
	if called["用户偏好"] {
		t.Fatalf("已显式关闭的用户偏好不应重写：%v", called)
	}
	if n := atomic.LoadInt32(&saved); n != 1 {
		t.Fatalf("应保存 1 个类别，实际 %d", n)
	}
}

// TestDisabledNoop：memory.enabled 缺失/非 true → 完全不动作（默认关闭）。
func TestDisabledNoop(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{}}
	})
	var calls int32
	_, _ = bus.On(sessionLoadSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		atomic.AddInt32(&calls, 1)
		return nil
	})
	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("默认关闭时不应读本轮消息，实际 %d 次", n)
	}
}

// TestDistillCarriesSubsystemLLM（SL-3 / SL-C2）：沉淀读 usr `llm.memory`（经既有
// data-user-config-load 面）→ 随 llm-simple 请求带 `llm`（provider name）。
func TestDistillCarriesSubsystemLLM(t *testing.T) {
	bus := newTestBusRaw(t)
	stubUserConfig(t, bus, map[string]any{
		"llm.memory": "prov-m",
		"llms":       []any{map[string]any{"name": "prov-m"}},
	})
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
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any { return map[string]any{"ok": true} })

	got := make(chan map[string]any, 1)
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req map[string]any
		_ = json.Unmarshal(v.Payload, &req)
		select {
		case got <- req:
		default:
		}
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})

	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	select {
	case req := <-got:
		if req["llm"] != "prov-m" {
			t.Fatalf("llm-simple 请求应带 llm=prov-m：%+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到 llm-simple 请求")
	}
}

// TestDistillWithoutSubsystemLLMByteEquivalent（SL-C7 兼容硬要求）：空 usr 配置（无 llms /
// 无 llm.memory → defaultLLM 无可用）→ 不传 `llm`，llm-simple 载荷与改前**逐字节等价**。
func TestDistillWithoutSubsystemLLMByteEquivalent(t *testing.T) {
	bus := newTestBus(t) // 默认空 usr 配置
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
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any { return map[string]any{"ok": true} })

	var raw []byte
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		raw = append([]byte(nil), v.Payload...)
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})

	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if len(raw) == 0 {
		t.Fatal("未收到 llm-simple 请求")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("载荷非 JSON: %v", err)
	}
	want, _ := json.Marshal(map[string]any{
		"prompt": m["prompt"], "system": "提示词-项目概要", "instance_id": "ins-1",
	})
	if string(raw) != string(want) {
		t.Fatalf("未指定子系统 LLM 时载荷须逐字节等价：\n got=%s\nwant=%s", raw, want)
	}
}

// TestNewReqIDConcurrentUnique：并发领取 req_id 必须全唯一（I-78 回归）。
//
// 旧实现 `memory-<UnixNano>` 在 Windows（时钟刻度 ≈1ms）下，同 tick 并发请求会同号
// → 应答按 req_id 等值过滤失效 → 8 类记忆并行读取串味。本测试在旧实现下必失败。
func TestNewReqIDConcurrentUnique(t *testing.T) {
	const goroutines, perG = 8, 1000
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*perG)
	dups := 0
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, 0, perG)
			for i := 0; i < perG; i++ {
				local = append(local, newReqID())
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range local {
				if _, ok := seen[id]; ok {
					dups++
					continue
				}
				seen[id] = struct{}{}
			}
		}()
	}
	wg.Wait()

	if dups != 0 {
		t.Fatalf("req_id 撞号 %d 次（%d goroutine × %d 次应全唯一）", dups, goroutines, perG)
	}
	if len(seen) != goroutines*perG {
		t.Fatalf("唯一 id 数 %d ≠ 期望 %d", len(seen), goroutines*perG)
	}
	for id := range seen {
		if !strings.HasPrefix(id, "memory-") {
			t.Fatalf("req_id 应保留 memory- 前缀（日志辨识）：%q", id)
		}
		if len(id) <= len("memory-") {
			t.Fatalf("req_id 随机段为空：%q", id)
		}
		break
	}
}

// TestUnixNanoSameTickCollides 复现 I-78 的根因：同 tick 连续取 UnixNano 会撞号。
//
// 仅作证据留存：若宿主时钟刻度足够细（连续取样不撞号），跳过而非失败。
func TestUnixNanoSameTickCollides(t *testing.T) {
	const n = 200
	seen := make(map[int64]int, n)
	dups := 0
	for i := 0; i < n; i++ {
		ns := time.Now().UnixNano()
		if seen[ns] > 0 {
			dups++
		}
		seen[ns]++
	}
	if dups == 0 {
		t.Skip("宿主时钟刻度足够细，本机无法复现 I-78（不影响 newReqID 唯一性结论）")
	}
	t.Logf("复现 I-78：%d 次连续取样出现 %d 次同 tick 撞号（UnixNano 刻度粗糙，仅 %d 个不同值）",
		n, dups, len(seen))
}
