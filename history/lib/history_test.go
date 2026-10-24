// history 插件门控 L1：`history.enabled` 默认判定（42 §2 (125)）——**只有显式 "true" 才开启**；
// 缺失 / "" / "false" / 非法 / 读失败 一律关闭（默认不开启，避免在用户仓库产生未预期的 git 提交）。
//
// 读通道两式（均覆盖）：
//   - 异步回读：instance-register → `data-prj-config-load`（测试以桩应答 {result:{data:值}}）；
//   - 实时同步：`data-prj-config-refresh` 广播（载荷 {id, op, list}）。
package history

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// TestGateFromValue 值 → 开关判定：**仅 "true" 为真**（缺失/""/false/大小写/数字 一律假）。
func TestGateFromValue(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"", false},
		{"false", false},
		{"True", false},
		{"TRUE", false},
		{"1", false},
		{"yes", false},
		{" true", false},
	} {
		if got := gateFromValue(tc.in); got != tc.want {
			t.Fatalf("gateFromValue(%q) = %v，期望 %v", tc.in, got, tc.want)
		}
	}
}

// stubLoad 在总线上桩应答 `data-prj-config-load`（可切换返回值），模拟 persist 读键。
// 实现要点：只在请求（无 ok 字段）时回应，且**异步**发应答（避免同一 Emit 内重入把
// 请求-响应订阅者的单缓冲 channel 填两次）。
func stubLoad(t *testing.T, bus mq.Bus, val *string, mu *sync.Mutex) {
	t.Helper()
	_, err := bus.On("data-prj-config-load", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil {
			return nil
		}
		if _, isReply := m["ok"]; isReply {
			return nil // 应答不回应答
		}
		reqID, _ := m["req_id"].(string)
		mu.Lock()
		cur := *val
		mu.Unlock()
		b, _ := json.Marshal(map[string]any{
			"req_id": reqID, "ok": true,
			"result": map[string]any{"data": cur},
		})
		go func() { bus.Emit(context.Background(), "data-prj-config-load", b) }()
		return nil
	})
	if err != nil {
		t.Fatalf("桩应答订阅失败: %v", err)
	}
}

// newHistory 建总线 + history 插件 + 桩读键，并登记实例（触发异步回读）。
func newHistory(t *testing.T, val *string) (mq.Bus, *History) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	bus, err := mq.New(mq.Options{Prefix: "chonk."})
	if err != nil {
		t.Fatalf("mq.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	stubLoad(t, bus, val, &sync.Mutex{})
	h := New()
	if err := h.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("history Start: %v", err)
	}
	pub := func(subject string, v any) {
		b, _ := json.Marshal(v)
		bus.Emit(context.Background(), subject, b)
	}
	pub("instance-register", map[string]any{
		"instance_id": "ins-gate", "client_type": "unittest", "work_dir": t.TempDir(),
	})
	return bus, h
}

// waitGate 等异步回读落定（gate 出现）并返回其值。
func waitGate(t *testing.T, h *History, id string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.gateMu.Lock()
		v, ok := h.gate[id]
		h.gateMu.Unlock()
		if ok {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等 history.enabled 回读超时")
	return false
}

// TestGateDefaultOff：无该键（读回 ""）→ 回读落定后门控为关闭；未回读时亦为关闭（默认不开启）。
func TestGateDefaultOff(t *testing.T) {
	val := ""
	_, h := newHistory(t, &val)
	// 未回读（gate 缺失）→ 关闭
	h.gateMu.Lock()
	_, read := h.gate["ins-gate"]
	h.gateMu.Unlock()
	if read {
		t.Fatalf("回读不应已完成（前置断言）")
	}
	if h.gateEnabled("ins-gate") {
		t.Fatalf("未回读时应默认关闭")
	}
	// 回读落定（键缺失 → ""）→ 仍关闭
	if waitGate(t, h, "ins-gate") {
		t.Fatalf("键缺失（读回 \"\"）应判定关闭")
	}
}

// TestGateExplicitValues：显式 "true" 开 / "false" 关 / 非法值关（经异步回读通道）。
func TestGateExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"banana", false},
	} {
		val := tc.val
		_, h := newHistory(t, &val)
		if got := waitGate(t, h, "ins-gate"); got != tc.want {
			t.Fatalf("history.enabled=%q → 门控 %v，期望 %v", tc.val, got, tc.want)
		}
	}
}

// TestGateRefreshRealtime：`data-prj-config-refresh` 实时同步 —— 只有显式 "true" 开；
// 缺失键 / "false" / op=delete → 关（默认不开启）。
func TestGateRefreshRealtime(t *testing.T) {
	empty := ""
	bus, h := newHistory(t, &empty)
	waitGate(t, h, "ins-gate") // 先等初始异步回读落定（避免其覆盖后续 refresh 结果）
	push := func(list map[string]any) {
		b, _ := json.Marshal(map[string]any{
			"id": "history.enabled", "op": "save", "list": list,
		})
		bus.Emit(context.Background(), "data-prj-config-refresh", b)
	}
	// 显式 "true" → 开
	push(map[string]any{"history.enabled": "true"})
	if !h.gateEnabled("ins-gate") {
		t.Fatalf("显式 true 应开启")
	}
	// 显式 "false" → 关
	push(map[string]any{"history.enabled": "false"})
	if h.gateEnabled("ins-gate") {
		t.Fatalf("显式 false 应关闭")
	}
	// 非法值 → 关
	push(map[string]any{"history.enabled": "banana"})
	if h.gateEnabled("ins-gate") {
		t.Fatalf("非法值应关闭")
	}
	// 键缺失（op=delete 的 list 不含该键）→ 关
	push(map[string]any{})
	if h.gateEnabled("ins-gate") {
		t.Fatalf("键缺失应关闭（默认不开启）")
	}
	// 非本键的刷新 → 不改动
	push(map[string]any{"history.enabled": "true"})
	b, _ := json.Marshal(map[string]any{"id": "other.key", "op": "save", "list": map[string]any{}})
	bus.Emit(context.Background(), "data-prj-config-refresh", b)
	if !h.gateEnabled("ins-gate") {
		t.Fatalf("非 history.enabled 的刷新不应改动门控")
	}
}

// TestRetentionRefreshBatch：**批量写**（单条广播带 `ids` 全组键）→ keep 与 ttl **一并应用**
// （61 §3.1：一次批量写只发 1 条 refresh；消费方按 `ids` 逐键展开，不得只认首键 `id`）。
func TestRetentionRefreshBatch(t *testing.T) {
	empty := ""
	bus, h := newHistory(t, &empty)
	waitGate(t, h, "ins-gate") // 先等初始异步回读落定（避免其覆盖后续 refresh 结果）
	b, _ := json.Marshal(map[string]any{
		"id":  "history.checkpoint_keep",
		"ids": []string{"history.checkpoint_keep", "history.checkpoint_ttl_days"},
		"op":  "save",
		"list": map[string]any{
			"history.checkpoint_keep":     "123",
			"history.checkpoint_ttl_days": "9",
		},
	})
	bus.Emit(context.Background(), "data-prj-config-refresh", b)
	if got := h.keepVal(); got != 123 {
		t.Fatalf("批量广播应应用 keep：got %d，期望 123", got)
	}
	if got := h.ttlVal(); got != 9 {
		t.Fatalf("批量广播应应用 ttl（勿因 id 为首键而漏）：got %d，期望 9", got)
	}
}

// TestInstanceExitClearsMappings：`instance-exit` → 回收该实例的 gate/gateBusy 与 sess 归属
// （否则已退出实例的条目永驻 → 无界增长）；且**不误删**其它实例的会话归属。
func TestInstanceExitClearsMappings(t *testing.T) {
	val := "true"
	bus, h := newHistory(t, &val)
	if !waitGate(t, h, "ins-gate") {
		t.Fatalf("前置：ins-gate 回读应为 true")
	}
	// 登记会话归属：一条属于 ins-gate，一条属于别的实例（不应被误删）。
	h.rememberSession("sess-1", "root-1", "/wd", "ins-gate")
	h.rememberSession("sess-other", "root-2", "/wd", "ins-other")

	b, _ := json.Marshal(map[string]any{"instance_id": "ins-gate"})
	bus.Emit(context.Background(), "instance-exit", b).Wait()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.gateMu.Lock()
		_, gateLeft := h.gate["ins-gate"]
		h.gateMu.Unlock()
		h.sessMu.Lock()
		_, sessLeft := h.sess["sess-1"]
		h.sessMu.Unlock()
		if !gateLeft && !sessLeft {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	h.gateMu.Lock()
	_, gateLeft := h.gate["ins-gate"]
	_, busyLeft := h.gateBusy["ins-gate"]
	h.gateMu.Unlock()
	if gateLeft || busyLeft {
		t.Fatalf("instance-exit 后 gate/gateBusy 应被回收：gate=%v busy=%v", gateLeft, busyLeft)
	}
	h.sessMu.Lock()
	_, sessLeft := h.sess["sess-1"]
	_, otherLeft := h.sess["sess-other"]
	h.sessMu.Unlock()
	if sessLeft {
		t.Fatalf("instance-exit 后该实例的会话归属应被回收")
	}
	if !otherLeft {
		t.Fatalf("其它实例的会话归属不应被误删")
	}
}
