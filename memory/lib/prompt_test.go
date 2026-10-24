// 类别沉淀提示词来源（OP-04，2026-10-06）：提示词**已文件化**并**由后端下发** ——
// 插件不再持内置常量、不再读 prj `memory.prompt.<类别名>` / usr `memory_prompts` 键载体，
// 而是取 `data-memory-list` 每项的 `prompt` 字段（数据层按文件读序
// 项目级 → 用户级 → 系统级磁盘 → embed 内置解析）作 llm-simple 的 `system`；
// **无提示词文件的类别 → 该类不提取**（不调 LLM / 不保存），并上报一次用户可见提示。
package memory

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// llmSystemByCategory 在 llm-simple 请求里按 prompt 的「【类别】X」行取该类的 system 提示词。
// 返回收集器（category → system）；handler 结束后由调用方读取。
type systemCollector struct {
	mu  sync.Mutex
	got map[string]string
}

func (s *systemCollector) add(prompt, system string) {
	const mark = "【类别】"
	i := strings.Index(prompt, mark)
	if i < 0 {
		return
	}
	rest := prompt[i+len(mark):]
	if j := strings.IndexByte(rest, '\n'); j >= 0 {
		rest = rest[:j]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got[strings.TrimSpace(rest)] = system
}

func (s *systemCollector) system(category string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[category]
}

// stubDistillReplies 让沉淀回路跑通所需的最小应答集（prj 配置 / 类别清单 / 旧全文 / 保存），
// 并返回 llm-simple 的 system 收集器。cats 由调用方给出（每项须带 `prompt` 才参与提取）。
func stubDistillReplies(t *testing.T, bus mq.Bus, prjList map[string]any, cats []any) *systemCollector {
	t.Helper()
	reply(t, bus, prjConfigListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": prjList}
	})
	reply(t, bus, sessionLoadSubject, func(map[string]any) map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": strings.Repeat("甲", 200)},
		}}
	})
	stubExtractFaces(t, bus, []any{map[string]any{"turn_id": "t1"}})
	reply(t, bus, memoryListSubject, func(map[string]any) map[string]any {
		return map[string]any{"list": cats}
	})
	reply(t, bus, memoryReadSubject, func(map[string]any) map[string]any {
		return map[string]any{"data": map[string]any{"content": "旧全文"}}
	})
	reply(t, bus, memorySaveSubject, func(map[string]any) map[string]any { return map[string]any{"ok": true} })

	sc := &systemCollector{got: map[string]string{}}
	_, _ = bus.On(llmSimpleSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req struct {
			Prompt string `json:"prompt"`
			System string `json:"system"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		sc.add(req.Prompt, req.System)
		v.Result = map[string]any{"text": "重写后的全文"}
		return nil
	})
	return sc
}

// TestDistillUsesCategoryPrompt：类别沉淀提示词取自 data-memory-list 的 `prompt` 字段
// （数据层文件读序解析结果）→ 原样作 llm-simple 的 `system`（含项目级与用户级类别）。
func TestDistillUsesCategoryPrompt(t *testing.T) {
	bus := newTestBus(t)
	const custom = "自定义提示词-项目概要"
	const userPref = "自定义提示词-用户偏好"
	sc := stubDistillReplies(t, bus,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"},
		[]any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": custom},
			map[string]any{"category": "用户偏好", "level": "user", "prompt": userPref},
		})

	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if got := sc.system("项目概要"); got != custom {
		t.Fatalf("项目概要未用清单下发的提示词：got=%q want=%q", got, custom)
	}
	if got := sc.system("用户偏好"); got != userPref {
		t.Fatalf("用户偏好未用清单下发的提示词：got=%q want=%q", got, userPref)
	}
}

// TestDistillSkipsCategoryWithoutPrompt：类别清单项 `prompt` 为空（= 无任何提示词文件，如新增
// 自定义类别未建 `capability/system/memory/<类别名>.md`）→ 该类**不提取**（不调 LLM / 不保存），
// 且上报一次用户可见提示（Kind=prompt）。
func TestDistillSkipsCategoryWithoutPrompt(t *testing.T) {
	bus := newTestBus(t)
	sc := stubDistillReplies(t, bus,
		map[string]any{memoryEnabledKey: "true", memoryMinTokensKey: "1"},
		[]any{
			map[string]any{"category": "项目概要", "level": "project", "prompt": "P"},
			map[string]any{"category": "新增自定义类", "level": "project"}, // 无提示词文件
		})
	spy := &noticeSpy{}
	p := New(Options{MinTurnTokens: 10})
	p.deps = plugin.Deps{Bus: bus, Notify: spy.fn}
	p.extract(turnEvent{InstanceID: "ins-1", WorkDir: `C:\ws`, Session: "s1", LastTurn: "t1"})

	if got := sc.system("项目概要"); got != "P" {
		t.Fatalf("有提示词的类别应照常提取：got=%q", got)
	}
	if got := sc.system("新增自定义类"); got != "" {
		t.Fatalf("无提示词文件的类别不应调 LLM：got=%q", got)
	}
	got := spy.snapshot()
	found := false
	for _, n := range got {
		if n.Kind == "prompt" && strings.Contains(n.Reason, "新增自定义类") {
			found = true
		}
	}
	if !found {
		t.Fatalf("无提示词文件的类别应上报可见提示（Kind=prompt）：%+v", got)
	}
}
