// 记忆提取的跨轮范围 / 累计门控 / 按级别遍历 / 写并发互斥白盒（OP-05/06/07/08，2026-10-06）。
//
// 覆盖：
//   - OP-05：提取范围 = 各 session 从「上次提取 turn」到本轮的全部轮（**含被阈值跳过的轮**）；
//     `memory.min-turn-tokens` 为**累计门控**（自上次提取以来累计 token 达阈值才提取）；
//   - OP-06：进度 = 专用表 memory_extract（prjusr；经 data-memory-extract-{load,save}），成功后推进；
//   - OP-07：项目级类别含子 session；用户偏好仅主 session；
//   - OP-08：per-(workdir/项目, 类别) 进程内互斥 + 读-改-写临界区。
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

// llmPromptCollector 收集 llm-simple 请求的 prompt（含类别与新信息）。
type llmPromptCollector struct {
	mu      sync.Mutex
	prompts []string
}

func (c *llmPromptCollector) add(p string) {
	c.mu.Lock()
	c.prompts = append(c.prompts, p)
	c.mu.Unlock()
}

func (c *llmPromptCollector) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.prompts, "\n---\n")
}

// stubTurnMessages 桩 data-session-load-messages：按 turn_id 返回内容 "msg-<turn_id>"。
func stubTurnMessages(t *testing.T, bus mq.Bus) {
	t.Helper()
	reply(t, bus, sessionLoadSubject, func(data map[string]any) map[string]any {
		label := strval(data["turn_id"])
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "msg-" + label},
		}}
	})
}

// stubRewriteReplies 桩 llm-simple（回显）+ memory-read/memory-save（调用计数）。
func stubRewriteReplies(t *testing.T, bus mq.Bus) (*llmPromptCollector, *int32) {
	t.Helper()
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	var saved int32
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any {
		atomic.AddInt32(&saved, 1)
		return map[string]any{"ok": true}
	})
	col := &llmPromptCollector{}
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		col.add(req.Prompt)
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
	return col, &saved
}

// TestExtractCrossTurnIncludesSkipped（OP-05）：进度在 t1 → 提取覆盖 t2、t3（t2 曾被阈值跳过）；
// 不重提 t1；成功后进度推进到 t3。
func TestExtractCrossTurnIncludesSkipped(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "10"}}
	})
	stubSessionGet(t, bus, "")
	stubSessionHistory(t, bus, []any{
		map[string]any{"turn_id": "t1", "full_tokens": 100},
		map[string]any{"turn_id": "t2", "full_tokens": 100},
		map[string]any{"turn_id": "t3", "full_tokens": 100},
	})
	stubProgress(t, bus, map[string]string{epKey("s1", "项目概要"): "t1"})
	var progSaveMu sync.Mutex
	var progSaves []string
	stubProgressSave(t, bus, func(key, value string) {
		progSaveMu.Lock()
		progSaves = append(progSaves, key+"="+value)
		progSaveMu.Unlock()
	})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "P"},
		}}
	})
	stubTurnMessages(t, bus)
	col, _ := stubRewriteReplies(t, bus)

	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t3"})

	prompt := col.joined()
	if !strings.Contains(prompt, "msg-t2") || !strings.Contains(prompt, "msg-t3") {
		t.Fatalf("跨轮范围应含 t2（曾被跳过）与 t3：%q", prompt)
	}
	if strings.Contains(prompt, "msg-t1") {
		t.Fatalf("已提取轮 t1 不应重提：%q", prompt)
	}
	progSaveMu.Lock()
	defer progSaveMu.Unlock()
	if len(progSaves) != 1 || progSaves[0] != epKey("s1", "项目概要")+"=t3" {
		t.Fatalf("成功后应推进进度到 t3：%v", progSaves)
	}
}

// TestCumulativeGateAccumulates（OP-05）：单轮均低于阈值 → 不提取；
// 累计（自上次提取以来多轮之和）达阈值 → 一次提取全部未提取轮。
func TestCumulativeGateAccumulates(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "80"}}
	})
	stubSessionGet(t, bus, "")
	stubProgress(t, bus, map[string]string{epKey("s1", "项目概要"): "t1"})
	stubProgressSave(t, bus, nil)
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "P"},
		}}
	})
	stubTurnMessages(t, bus)
	col, _ := stubRewriteReplies(t, bus)

	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}

	// ① t2 单轮 50 token < 80 → 跳过（不调 LLM）
	hist := &dynamicHistory{}
	stubSessionHistoryDynamic(t, bus, hist)
	hist.set([]any{map[string]any{"turn_id": "t1"}, map[string]any{"turn_id": "t2", "full_tokens": 50}})
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t2"})
	if got := col.joined(); got != "" {
		t.Fatalf("单轮 50 < 80，不应提取：%q", got)
	}

	// ② t2+t3 累计 100 ≥ 80 → 提取 t2 与 t3
	hist.set([]any{
		map[string]any{"turn_id": "t1"},
		map[string]any{"turn_id": "t2", "full_tokens": 50},
		map[string]any{"turn_id": "t3", "full_tokens": 50},
	})
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t3"})
	prompt := col.joined()
	if !strings.Contains(prompt, "msg-t2") || !strings.Contains(prompt, "msg-t3") {
		t.Fatalf("累计达阈值应一次提取 t2+t3：%q", prompt)
	}
}

// dynamicHistory 可变历史桩（累计门控两步用例）。
type dynamicHistory struct {
	mu    sync.Mutex
	turns []any
}

func (d *dynamicHistory) set(turns []any) {
	d.mu.Lock()
	d.turns = turns
	d.mu.Unlock()
}

func (d *dynamicHistory) get() []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]any(nil), d.turns...)
}

// stubSessionHistoryDynamic 用可变切片桩 data-session-history（同一总线上可多次读取）。
func stubSessionHistoryDynamic(t *testing.T, bus mq.Bus, d *dynamicHistory) {
	t.Helper()
	reply(t, bus, sessionHistorySubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": map[string]any{"turns": d.get(), "messages": []any{}, "has_more": false}}
	})
}

// TestProjectCategoriesSubSessionUserPrefMainOnly（OP-07 + DSL-4 42 §2 (253)）：子会话是否沉淀
// 先受「提取子会话记忆」（`memory.subsession`，默认关）门控；开启后**仅提取项目级**（用户偏好仍仅主会话）；
// 主会话恒两者都提取。
func TestProjectCategoriesSubSessionUserPrefMainOnly(t *testing.T) {
	cats := []any{
		map[string]any{"category": "项目概要", "level": "project", "prompt": "P-项目"},
		map[string]any{"category": "用户偏好", "level": "user", "prompt": "P-偏好"},
	}
	newBus := func(t *testing.T, parentID string, subsessionOn bool) (*llmPromptCollector, *Plugin) {
		t.Helper()
		bus := newTestBus(t)
		reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
			list := map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "10"}
			if subsessionOn {
				list[memorySubsessionKey] = "true" // DSL-4「提取子会话记忆」开启
			}
			return map[string]any{"list": list}
		})
		stubSessionGet(t, bus, parentID)
		stubSessionHistory(t, bus, []any{map[string]any{"turn_id": "t1", "full_tokens": 100}})
		stubProgress(t, bus, map[string]string{})
		stubProgressSave(t, bus, nil)
		reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
			return map[string]any{"list": cats}
		})
		stubTurnMessages(t, bus)
		col, _ := stubRewriteReplies(t, bus)
		p := New(DefaultOptions())
		p.deps = plugin.Deps{Bus: bus}
		return col, p
	}

	// ① 子会话 + 开关开启：仅项目级
	colSub, pSub := newBus(t, "main-s", true)
	pSub.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "child-s", LastTurn: "t1"})
	sub := colSub.joined()
	if !strings.Contains(sub, "【类别】项目概要") {
		t.Fatalf("子会话（开关开启）应提取项目级类别：%q", sub)
	}
	if strings.Contains(sub, "【类别】用户偏好") {
		t.Fatalf("子会话不应提取用户偏好：%q", sub)
	}

	// ② 子会话 + 开关未开（默认）：跳过（不提取任何类别）
	colOff, pOff := newBus(t, "main-s", false)
	pOff.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "child-s", LastTurn: "t1"})
	if off := colOff.joined(); off != "" {
		t.Fatalf("子会话 + 开关未开应跳过沉淀（不调 LLM）：%q", off)
	}

	// ③ 主会话：两者都提取
	colMain, pMain := newBus(t, "", false)
	pMain.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})
	main := colMain.joined()
	if !strings.Contains(main, "【类别】项目概要") || !strings.Contains(main, "【类别】用户偏好") {
		t.Fatalf("主会话应同时提取项目级与用户偏好：%q", main)
	}
}

// TestConcurrentRewriteSerializedPerCategory（OP-08）：同进程多会话并发写同一类别 →
// LLM 重写阶段不并发（整段读-改-写临界区 per-(workdir, 类别) 互斥）。
func TestConcurrentRewriteSerializedPerCategory(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "10"}}
	})
	stubSessionGet(t, bus, "")
	stubSessionHistory(t, bus, []any{map[string]any{"turn_id": "t1", "full_tokens": 100}})
	stubProgress(t, bus, map[string]string{})
	stubProgressSave(t, bus, nil)
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{map[string]any{"category": "项目概要", "level": "project", "prompt": "P"}}}
	})
	stubTurnMessages(t, bus)
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any { return map[string]any{"ok": true} })

	var inflight, maxInflight int32
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		n := atomic.AddInt32(&inflight, 1)
		for {
			cur := atomic.LoadInt32(&maxInflight)
			if n <= cur || atomic.CompareAndSwapInt32(&maxInflight, cur, n) {
				break
			}
		}
		time.Sleep(60 * time.Millisecond) // 放大并发窗口
		atomic.AddInt32(&inflight, -1)
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})

	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}

	var wg sync.WaitGroup
	for _, s := range []string{"s1", "s2", "s3", "s4"} {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: session, LastTurn: "t1"})
		}(s)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&maxInflight); got != 1 {
		t.Fatalf("同一类别的 LLM 重写不得并发（观测最大并发 = %d，应为 1）", got)
	}
}
