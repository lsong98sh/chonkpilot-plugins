package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeEvent(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		in    map[string]any
		typ   string
		data  map[string]any
	}{
		{
			name:  "text delta",
			topic: "session-receive",
			in: map[string]any{"type": "text", "payload": map[string]any{"text": "hi"},
				"session": "s1", "turn": "t1"},
			typ:  "message.delta",
			data: map[string]any{"session_id": "s1", "turn_id": "t1", "text": "hi"},
		},
		{
			name:  "reasoning",
			topic: "session-receive",
			in: map[string]any{"type": "reason", "payload": map[string]any{"text": "think"},
				"session": "s1", "turn": "t1"},
			typ:  "message.reasoning",
			data: map[string]any{"session_id": "s1", "turn_id": "t1", "text": "think"},
		},
		{
			name:  "tool call",
			topic: "session-receive",
			in: map[string]any{"type": "tool-call", "payload": map[string]any{"tool": "task_run"},
				"session": "s1", "turn": "t1"},
			typ:  "message.tool",
			data: map[string]any{"session_id": "s1", "turn_id": "t1", "name": "task_run", "status": "running"},
		},
		{
			name:  "turn start",
			topic: "session-turn-start",
			in:    map[string]any{"session": "s1", "turn": "t1"},
			typ:   "turn.start",
			data:  map[string]any{"session_id": "s1", "turn_id": "t1"},
		},
		{
			name:  "turn complete",
			topic: "session-complete",
			in: map[string]any{"session": "s1", "turn": "t1", "status": "error",
				"finish_reason": "error", "message": "boom"},
			typ: "turn.complete",
			data: map[string]any{"session_id": "s1", "turn_id": "t1", "status": "error",
				"finish_reason": "error", "message": "boom"},
		},
		{
			name:  "ask",
			topic: "session-ask",
			in: map[string]any{"ask_id": "a1", "dialog_id": "d1", "session": "s1", "turn": "t1",
				"question": "选哪个", "options": []any{map[string]any{"value": "x", "label": "X"}},
				"multi": true},
			typ: "ask",
			data: map[string]any{"ask_id": "a1", "session_id": "s1", "turn_id": "t1",
				"question": "选哪个", "options": []any{map[string]any{"value": "x", "label": "X"}}, "multi": true},
		},
		{
			name:  "task update",
			topic: "task-done",
			in: map[string]any{"task_id": "tk1", "session": "s1", "turn": "t1",
				"state": "done", "tool": "task_run", "kind": "tool"},
			typ: "task.update",
			data: map[string]any{"task_id": "tk1", "session_id": "s1", "turn_id": "t1",
				"state": "done", "tool": "task_run", "kind": "tool"},
		},
		{
			name:  "notice",
			topic: "tool-notify",
			in:    map[string]any{"message": "完成", "plugin": "memory", "kind": "queued"},
			typ:   "notice",
			data:  map[string]any{"message": "完成", "plugin": "memory", "kind": "queued"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, data, ok := normalizeEvent(tc.topic, tc.in)
			if !ok {
				t.Fatalf("应可归一：%s", tc.topic)
			}
			if typ != tc.typ {
				t.Fatalf("type = %q，期望 %q", typ, tc.typ)
			}
			if !reflect.DeepEqual(data, tc.data) {
				t.Fatalf("data 不符：\n得到 %#v\n期望 %#v", data, tc.data)
			}
		})
	}
}

func TestNormalizeEventUnsupported(t *testing.T) {
	if _, _, ok := normalizeEvent("session-receive", map[string]any{"type": "unknown"}); ok {
		t.Fatal("未知 session-receive 子类型不应归一")
	}
	if _, _, ok := normalizeEvent("unknown-topic", map[string]any{}); ok {
		t.Fatal("未知主题不应归一")
	}
}

func TestEventFrameShape(t *testing.T) {
	f := eventFrame("ws1", "message.delta", map[string]any{"text": "hi"})
	if f["workspace_id"] != "ws1" || f["type"] != "message.delta" {
		t.Fatalf("事件帧应为 {workspace_id,type,data}：%#v", f)
	}
	if data := asMap(f["data"]); data["text"] != "hi" {
		t.Fatalf("data 不符：%#v", f["data"])
	}
	if _, err := json.Marshal(f); err != nil {
		t.Fatalf("事件帧不可序列化：%v", err)
	}
}

func TestRunningSetAndAskCache(t *testing.T) {
	r := newRunningSet()
	r.Add("s1")
	if !r.Has("s1") || r.Has("") {
		t.Fatal("runningSet 维护不符")
	}
	r.Remove("s1")
	if r.Has("s1") {
		t.Fatal("session-complete 后应移出运行集")
	}

	a := newAskCache()
	a.Put("ask1", "dlg1")
	if got := a.Get("ask1"); got != "dlg1" {
		t.Fatalf("ask 缓存 = %q，期望 dlg1", got)
	}
	if got := a.Get("missing"); got != "" {
		t.Fatalf("未命中应为空串，得到 %q", got)
	}
}

func TestOnBusEventRoutesToWorkspace(t *testing.T) {
	h := newFakeHost()
	c := newTestClient(t, h, nil)
	c.ws.Register("i1", "/repo/proj", "", "")
	payload, _ := json.Marshal(map[string]any{
		"instance_id": "i1", "session": "s1", "turn": "t1",
		"type": "text", "payload": map[string]any{"text": "hi"},
	})
	c.onBusEvent("session-receive", payload)
	if c.running.Has("s1") {
		t.Fatal("session-receive 不应加入运行集")
	}
	// 运行集只由 turn-start / complete 维护。
	c.onBusEvent("session-turn-start", mustJSON(t, map[string]any{"instance_id": "i1", "session": "s1", "turn": "t1"}))
	if !c.running.Has("s1") {
		t.Fatal("session-turn-start 应加入运行集")
	}
	c.onBusEvent("session-complete", mustJSON(t, map[string]any{
		"instance_id": "i1", "session": "s1", "turn": "t1", "status": "complete",
	}))
	if c.running.Has("s1") {
		t.Fatal("session-complete 应移出运行集")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
