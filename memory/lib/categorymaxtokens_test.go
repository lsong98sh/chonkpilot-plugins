// `memory.category-max-tokens`（prj 键）接线白盒（2026-09-19）：
//   - 读取口径：>0 生效（告警阈值），缺失/非正 = 0 = 不限；
//   - 语义：**仅告警、不截断、不阻断沉淀**（与前端标红提示同口径）——超限仍照常保存全文。
package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// TestResolveConfigCategoryMaxTokens：读 prj 键；缺失 → 0（不限）。
func TestResolveConfigCategoryMaxTokens(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{
			memoryEnabledKey:           "true",
			memoryCategoryMaxTokensKey: "1500",
		}}
	})
	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus}
	cfg, err := p.resolveConfig("ins-1")
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.MaxTokens != 1500 {
		t.Fatalf("MaxTokens = %d，want 1500", cfg.MaxTokens)
	}

	// 键缺失 → 0（不限）
	bus2 := newTestBus(t)
	reply(t, bus2, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{memoryEnabledKey: "true"}}
	})
	p2 := New(DefaultOptions())
	p2.deps = plugin.Deps{Bus: bus2}
	cfg2, err := p2.resolveConfig("ins-1")
	if err != nil {
		t.Fatalf("resolveConfig(2): %v", err)
	}
	if cfg2.MaxTokens != 0 {
		t.Fatalf("键缺失应不限（0）: %d", cfg2.MaxTokens)
	}
}

// TestCategoryMaxTokensWarnsNotTruncates：超限（远大于阈值）仍**原样保存全文**（不截断、不阻断）。
func TestCategoryMaxTokensWarnsNotTruncates(t *testing.T) {
	bus := newTestBus(t)
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": map[string]any{
			memoryEnabledKey:           "true",
			memoryMinTokensKey:         "1",
			memoryCategoryMaxTokensKey: "1", // 极小阈值 → 必然超限
		}}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 300)},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": []any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "提示词-项目概要"},
		}}
	})
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	long := strings.Repeat("新记忆内容", 200)
	var savedContent string
	var saved int32
	reply(t, bus, memorySaveSubject, func(data map[string]any) map[string]any {
		atomic.AddInt32(&saved, 1)
		savedContent, _ = data["content"].(string)
		return map[string]any{"ok": true}
	})
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		v.Result = map[string]any{"text": long}
		return nil
	})

	var logs []string
	var mu sync.Mutex
	p := New(DefaultOptions())
	p.deps = plugin.Deps{Bus: bus, Logf: func(format string, args ...any) {
		mu.Lock()
		logs = append(logs, fmt.Sprintf(format, args...))
		mu.Unlock()
	}}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if n := atomic.LoadInt32(&saved); n != 1 {
		t.Fatalf("超告警阈值不应阻断沉淀：保存次数 = %d，want 1", n)
	}
	if savedContent != long {
		t.Fatalf("超告警阈值不应截断：保存内容长度 = %d，want %d", len(savedContent), len(long))
	}
	// 原始证据：后端确实读取该键并按同一口径告警（超限 1 token）
	mu.Lock()
	defer mu.Unlock()
	warned := false
	for _, l := range logs {
		if strings.Contains(l, "超告警阈值") {
			warned = true
			t.Logf("warn log: %s", l)
		}
	}
	if !warned {
		t.Fatalf("超限应记录告警日志，实际日志: %v", logs)
	}
}
