package main

import (
	"encoding/json"
	"testing"
)

// TestSessionDocOfFilters：只索引 role ∈ {user,assistant} 且 type=text（缺省）的非空正文。
func TestSessionDocOfFilters(t *testing.T) {
	base := map[string]any{
		"role": "user", "type": "text", "message_id": "m1", "turn_id": "t1",
		"created_at": "2026-11-01T10:00:00.000000001Z", "content": "全文索引怎么做",
	}
	d, ok := sessionDocOf("s1", "d1", base)
	if !ok {
		t.Fatal("user/text 消息应入索引")
	}
	if d.SessionID != "s1" || d.DialogID != "d1" || d.MessageID != "m1" || d.TurnID != "t1" {
		t.Fatalf("字段映射不符：%+v", d)
	}
	if d.TS <= 0 {
		t.Fatalf("created_at 应解析为 UnixMilli：%d", d.TS)
	}

	cases := []map[string]any{
		{"role": "tool", "message_id": "m2", "content": "x"},                           // 工具结果
		{"role": "assistant", "type": "tool_pair", "message_id": "m3", "content": "x"}, // 工具卡片
		{"role": "assistant", "type": "reasoning", "message_id": "m4", "content": "x"}, // 思维链
		{"role": "user", "type": "text", "message_id": "m5", "content": "   "},         // 空正文
		{"role": "user", "type": "text", "content": "无 id"},                            // 缺 message_id
	}
	for i, m := range cases {
		if _, ok := sessionDocOf("s1", "d1", m); ok {
			t.Fatalf("case %d 不应入索引：%+v", i, m)
		}
	}

	// type 缺省 = 视作 text（消息面普通消息不带 type）
	noType := map[string]any{"role": "assistant", "message_id": "m9", "content": "结论"}
	if _, ok := sessionDocOf("s1", "d1", noType); !ok {
		t.Fatal("type 缺省应按 text 入索引")
	}
}

// TestParseHistoryPage：解析 data-session-history 信封（turns / messages / has_more）。
func TestParseHistoryPage(t *testing.T) {
	raw := []byte(`{"messages":{"turns":[{"turn_id":"t1"},{"turn_id":"t2"}],
		"messages":[{"message_id":"m1","role":"user","turn_id":"t1","content":"hi"}],
		"has_more":true}}`)
	page, err := parseHistoryPage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(page.Turns) != 2 || page.Turns[0] != "t1" || !page.HasMore {
		t.Fatalf("turns/has_more 解析不符：%+v", page)
	}
	if len(page.Messages) != 1 || page.Messages[0]["message_id"] != "m1" {
		t.Fatalf("messages 解析不符：%+v", page.Messages)
	}
}

// TestWorkdirFromMeta：work_dir → instance 登记 → 唯一工作区 → 明确报错。
func TestWorkdirFromMeta(t *testing.T) {
	store := newFakeStore(map[string]string{})
	mgr := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)

	if wd, err := mgr.workdirFromMeta(map[string]any{"work_dir": `E:\proj`}); err != nil || wd != `E:\proj` {
		t.Fatalf("work_dir 直取失败：%q err=%v", wd, err)
	}
	if wd, err := mgr.workdirFromMeta(map[string]any{"instance_id": "inst-1"}); err != nil || wd != `E:\proj` {
		t.Fatalf("instance 回退失败：%q err=%v", wd, err)
	}
	// 无任何上下文 → 唯一实例的工作区
	if wd, err := mgr.workdirFromMeta(nil); err != nil || wd != `E:\proj` {
		t.Fatalf("唯一工作区回退失败：%q err=%v", wd, err)
	}
	// 多实例不同工作区 + 无上下文 → 明确报错（不猜路径）
	mgr.workdirs = map[string]string{"inst-1": `E:\a`, "inst-2": `E:\b`}
	if _, err := mgr.workdirFromMeta(nil); err == nil {
		t.Fatal("多工作区且无上下文应报错")
	}
}

// TestSessionsConfigKey：新配置键只接受 true/false，且 values() 回显缺省 false。
func TestSessionsConfigKey(t *testing.T) {
	if v, err := normalizeValue(KeyVftsSessions, "TRUE"); err != nil || v != "true" {
		t.Fatalf("true 归一不符：%q err=%v", v, err)
	}
	if _, err := normalizeValue(KeyVftsSessions, "yes"); err == nil {
		t.Fatal("非 true/false 应报 config_invalid")
	}
	if got := defaultSettings().values()[KeyVftsSessions]; got != "false" {
		t.Fatalf("缺省应为 false，实际 %q", got)
	}
	s := settingsOf(map[string]string{KeyVftsSessions: "true"})
	if !s.VftsSessions {
		t.Fatal("settingsOf 未读到会话检索开关")
	}
}

// TestOnSessionCompleteIgnoresSubSession：子轮次（parents 非空）不进入待索引队列。
func TestOnSessionCompleteIgnoresSubSession(t *testing.T) {
	store := newFakeStore(map[string]string{})
	mgr := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)
	mgr.sessions = newSessionIndexer(mgr)
	idx := mgr.sessions

	payload, _ := json.Marshal(map[string]any{
		"instance_id": "inst-1", "work_dir": `E:\proj`, "session": "s1", "turn": "t1",
		"dialog_id": "d1", "parents": []string{"s0"},
	})
	idx.onSessionComplete(payload)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if len(idx.pending) != 0 {
		t.Fatalf("子轮次不应入队：%+v", idx.pending)
	}
}

// TestSessionTurnTaskMerge：同会话连续轮末事件去抖合并（只保留一个任务、轮次去重）。
func TestSessionTurnTaskMerge(t *testing.T) {
	store := newFakeStore(map[string]string{})
	mgr := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)
	mgr.sessions = newSessionIndexer(mgr)
	idx := mgr.sessions

	for _, turn := range []string{"t1", "t1", "t2"} {
		payload, _ := json.Marshal(map[string]any{
			"instance_id": "inst-1", "work_dir": `E:\proj`, "session": "s1", "turn": turn,
			"dialog_id": "d1", "parents": []string{},
		})
		idx.onSessionComplete(payload)
	}
	idx.mu.Lock()
	n := len(idx.pending)
	task := idx.pending[normalizeWorkdir(`E:\proj`)+"|s1"]
	idx.mu.Unlock()
	if n != 1 || task == nil || len(task.Turns) != 2 {
		t.Fatalf("去抖合并不符：pending=%d task=%+v", n, task)
	}
	// 清理计时器，避免测试进程悬挂
	idx.mu.Lock()
	for k, tm := range idx.timers {
		tm.Stop()
		delete(idx.timers, k)
	}
	idx.mu.Unlock()
}
