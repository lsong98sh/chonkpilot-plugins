package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBuildHistoryArgsOmitsEmpty(t *testing.T) {
	args := buildHistoryArgs("i1", map[string]any{"session_id": "s1"})
	if len(args) != 2 || args["instance_id"] != "i1" || args["session_id"] != "s1" {
		t.Fatalf("只应带 instance_id / session_id：%#v", args)
	}
	full := buildHistoryArgs("i1", map[string]any{
		"session_id": "s1", "dialog_id": "d1", "before_turn_id": "t9", "target_messages": float64(20),
	})
	if full["dialog_id"] != "d1" || full["before_turn_id"] != "t9" || full["target_messages"] != 20 {
		t.Fatalf("参数拼装不符：%#v", full)
	}
}

func TestNormalizeHistoryMessage(t *testing.T) {
	plain := normalizeHistoryMessage(map[string]any{
		"message_id": "m1", "role": "assistant", "content": "hi", "turn_id": "t1", "type": "text",
		"reasoning": "why",
	})
	if plain["id"] != "m1" || plain["text"] != "hi" || plain["reasoning"] != "why" || plain["type"] != "text" {
		t.Fatalf("普通消息归一不符：%#v", plain)
	}
	if _, ok := plain["tool_name"]; ok {
		t.Fatal("普通消息不应带 tool_name")
	}

	toolPair := normalizeHistoryMessage(map[string]any{
		"role": "tool", "type": "tool_pair", "tool": "task_run", "tool_call_status": "done",
		"message_id": "m2", "turn_id": "t1",
	})
	if toolPair["tool_name"] != "task_run" || toolPair["tool_status"] != "done" {
		t.Fatalf("工具卡片归一不符：%#v", toolPair)
	}

	calls := normalizeHistoryMessage(map[string]any{
		"role": "assistant", "message_id": "m3",
		"tool_calls": `[{"id":"c1","function":{"name":"file_read","arguments":"{}"}}]`,
	})
	if calls["tool_name"] != "file_read" {
		t.Fatalf("应从 tool_calls[0].function.name 取工具名：%#v", calls)
	}
}

func TestSessionsListMapsAndMarksRunning(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.list", map[string]any{"list": []any{
		map[string]any{"session_id": "s1", "dialog_id": "d1", "title": "A",
			"last_activity": "2026-01-02", "updated_at": "2026-01-01"},
		map[string]any{"session_id": "s2", "title": "B", "updated_at": "2026-02-02"},
	}})
	c := newTestClient(t, h, nil)
	c.running.Add("s1")

	v, err := c.rpcSessionsList(context.Background(), "i1")
	if err != nil {
		t.Fatalf("rpcSessionsList: %v", err)
	}
	list, ok := v.(map[string]any)["list"].([]map[string]any)
	if !ok || len(list) != 2 {
		t.Fatalf("list 不符：%#v", v)
	}
	if list[0]["updated_at"] != "2026-01-02" || list[0]["running"] != true {
		t.Fatalf("第一条映射不符：%#v", list[0])
	}
	if list[1]["updated_at"] != "2026-02-02" || list[1]["running"] != false {
		t.Fatalf("第二条映射不符：%#v", list[1])
	}
	args := h.callsOf("plugin/v1/session.list")
	if len(args) != 1 || args[0]["instance_id"] != "i1" {
		t.Fatalf("session.list 入参不符：%#v", args)
	}
}

func TestSessionsCreateEmptyTextOnlyStarts(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.start", map[string]any{"accepted": true})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsCreate(context.Background(), "i1", map[string]any{"text": ""})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := v.(map[string]any)
	if !strings.HasPrefix(m["session_id"].(string), "s_") || !strings.HasPrefix(m["turn_id"].(string), "t_") {
		t.Fatalf("session/turn 前缀不符：%#v", m)
	}
	if len(h.callsOf("plugin/v1/session.start")) != 1 {
		t.Fatal("应调用一次 session.start")
	}
	if len(h.callsOf("plugin/v1/session.send")) != 0 {
		t.Fatal("空文本不应调用 session.send")
	}
	args := h.callsOf("plugin/v1/session.start")[0]
	if args["instance_id"] != "i1" || args["session"] != m["session_id"] || args["turn"] != m["turn_id"] {
		t.Fatalf("start 入参不符：%#v", args)
	}
	if args["request_id"] == nil {
		t.Fatal("start 必须带 request_id 以取真实 accepted 应答")
	}
}

func TestSessionsCreateWithTextSends(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.start", map[string]any{"accepted": true})
	h.setResponse("plugin/v1/session.send", map[string]any{})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsCreate(context.Background(), "i1", map[string]any{"text": "你好"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sends := h.callsOf("plugin/v1/session.send")
	if len(sends) != 1 {
		t.Fatalf("应调用一次 session.send，得到 %d", len(sends))
	}
	if sends[0]["type"] != "text-user" || sends[0]["content"] != "你好" {
		t.Fatalf("send 入参不符：%#v", sends[0])
	}
	if sends[0]["turn"] != v.(map[string]any)["turn_id"] {
		t.Fatalf("send 轮次应与返回一致：%#v", sends[0])
	}
}

func TestSessionsCreateBusyReturnsRejection(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.start", map[string]any{"accepted": false, "error": "session busy: other"})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsCreate(context.Background(), "i1", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("accepted:false 应是结果而非 RPC 错误：%v", err)
	}
	m := v.(map[string]any)
	if m["accepted"] != false || !strings.Contains(m["error"].(string), "session busy") {
		t.Fatalf("忙碌应答未原样返回：%#v", m)
	}
	if len(h.callsOf("plugin/v1/session.send")) != 0 {
		t.Fatal("被拒绝后不应再发 session.send")
	}
}

// 手机端发送一律进 inbox（source=mobile）：运行中 → 排队（queued:true），不插话、不拒绝。
func TestSessionsSendRunningQueuesToInbox(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.inbox.add", map[string]any{"ok": true, "id": "in-1", "type": "next_turn"})
	c := newTestClient(t, h, nil)
	c.running.Add("s1")

	v, err := c.rpcSessionsSend(context.Background(), "i1", map[string]any{"session_id": "s1", "text": "继续"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	m := v.(map[string]any)
	if m["accepted"] != true || m["queued"] != true || m["inbox_id"] != "in-1" || m["type"] != "next_turn" {
		t.Fatalf("运行中应入队：%#v", m)
	}
	calls := h.callsOf("plugin/v1/session.inbox.add")
	if len(calls) != 1 {
		t.Fatalf("应调用一次 session.inbox.add，得到 %d", len(calls))
	}
	if calls[0]["source"] != "mobile" || calls[0]["content"] != "继续" || calls[0]["session"] != "s1" ||
		calls[0]["instance_id"] != "i1" {
		t.Fatalf("inbox.add 入参不符：%#v", calls[0])
	}
	if len(h.callsOf("plugin/v1/session.start")) != 0 || len(h.callsOf("plugin/v1/session.send")) != 0 {
		t.Fatal("手机端不得走 session.start / session.send（不插话）")
	}
}

// 空闲同样走 inbox（服务端立即消费起新一轮）→ queued:false，由 turn.start 驱动 UI。
func TestSessionsSendIdleUsesInbox(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.inbox.add", map[string]any{"ok": true, "id": "in-2", "type": "next_turn"})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsSend(context.Background(), "i1", map[string]any{"session_id": "s2", "text": "新话题"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	m := v.(map[string]any)
	if m["accepted"] != true || m["queued"] != false || m["session_id"] != "s2" {
		t.Fatalf("空闲发送应经 inbox 立即消费：%#v", m)
	}
	if len(h.callsOf("plugin/v1/session.inbox.add")) != 1 ||
		len(h.callsOf("plugin/v1/session.start")) != 0 {
		t.Fatal("空闲发送也只应调 inbox.add")
	}
}

func TestSessionsSendInboxFailureRejected(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.inbox.add", map[string]any{"ok": false, "error": "dialog_required: x"})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsSend(context.Background(), "i1", map[string]any{"session_id": "s1", "text": "hi"})
	if err != nil {
		t.Fatalf("入队失败应是结果而非 RPC 错误：%v", err)
	}
	m := v.(map[string]any)
	if m["accepted"] != false || !strings.Contains(m["error"].(string), "dialog_required") {
		t.Fatalf("失败应答未透出：%#v", m)
	}
}

// 2026-11-01：两端共享同一 inbox → H5 拿到**全量** pending 项（含 insert 插话项与来源标识）。
func TestSessionsInboxReturnsAll(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.inbox.list", map[string]any{"items": []any{
		map[string]any{"id": "i1", "type": "next_turn", "content": "排队A", "created_at": "2026-01-01", "source": "mobile"},
		map[string]any{"id": "i2", "type": "insert", "content": "插话B", "created_at": "2026-01-02"},
	}})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsInbox(context.Background(), "i1", map[string]any{"session_id": "s1"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	items := v.(map[string]any)["items"].([]map[string]any)
	if len(items) != 2 {
		t.Fatalf("应返回全量 pending 项：%#v", items)
	}
	if items[0]["id"] != "i1" || items[0]["source"] != "mobile" {
		t.Fatalf("排队项/来源不符：%#v", items[0])
	}
	if items[1]["id"] != "i2" || items[1]["type"] != "insert" {
		t.Fatalf("插话项应返回：%#v", items[1])
	}
	args := h.callsOf("plugin/v1/session.inbox.list")
	if len(args) != 1 || args[0]["status"] != "pending" || args[0]["instance_id"] != "i1" {
		t.Fatalf("inbox.list 入参不符：%#v", args)
	}
}

// 手机端 inbox 项操作（插话 / 取消 / 删除）→ 宿主对应方法面。
func TestSessionsInboxActions(t *testing.T) {
	for _, tc := range []struct{ rpc, host string }{
		{"sessions.inbox.promote", "plugin/v1/session.inbox.promote"},
		{"sessions.inbox.revert", "plugin/v1/session.inbox.revert"},
		{"sessions.inbox.discard", "plugin/v1/session.inbox.discard"},
	} {
		h := newFakeHost()
		h.setResponse(tc.host, map[string]any{"ok": true, "type": "insert"})
		c := newTestClient(t, h, nil)
		v, err := c.dispatchRPC(context.Background(), "i1", tc.rpc, map[string]any{"session_id": "s1", "id": "in-9"})
		if err != nil {
			t.Fatalf("%s: %v", tc.rpc, err)
		}
		m := v.(map[string]any)
		if m["ok"] != true {
			t.Fatalf("%s 应答不符：%#v", tc.rpc, m)
		}
		args := h.callsOf(tc.host)
		if len(args) != 1 || args[0]["id"] != "in-9" || args[0]["instance_id"] != "i1" {
			t.Fatalf("%s 入参不符：%#v", tc.rpc, args)
		}
	}
}

// 手机端发送可携带 insert=true（插话意向）。
func TestSessionsSendInsertFlag(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.inbox.add", map[string]any{"ok": true, "id": "in-3", "type": "insert"})
	c := newTestClient(t, h, nil)
	v, err := c.dispatchRPC(context.Background(), "i1", "sessions.send", map[string]any{
		"session_id": "s1", "text": "插话", "insert": true,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	m := v.(map[string]any)
	if m["accepted"] != true || m["queued"] != true || m["type"] != "insert" {
		t.Fatalf("插入意向应排队：%#v", m)
	}
	args := h.callsOf("plugin/v1/session.inbox.add")
	if len(args) != 1 || args[0]["insert"] != true || args[0]["source"] != "mobile" {
		t.Fatalf("inbox.add 入参不符：%#v", args)
	}
}

func TestSessionsHistoryNormalizesEnvelope(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.history", map[string]any{"messages": map[string]any{
		"turns": []any{},
		"messages": []any{
			map[string]any{"message_id": "m1", "role": "user", "content": "hi", "turn_id": "t1"},
			map[string]any{"message_id": "m2", "role": "tool", "type": "tool_pair", "tool": "file_read",
				"tool_call_status": "done", "turn_id": "t1"},
		},
		"has_more": true,
	}})
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsHistory(context.Background(), "i1", map[string]any{"session_id": "s1", "dialog_id": "d1"})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	m := v.(map[string]any)
	if m["has_more"] != true {
		t.Fatalf("has_more 不符：%#v", m)
	}
	msgs := m["messages"].([]map[string]any)
	if len(msgs) != 2 || msgs[1]["tool_name"] != "file_read" || msgs[1]["tool_status"] != "done" {
		t.Fatalf("消息归一不符：%#v", msgs)
	}
	args := h.callsOf("plugin/v1/session.history")[0]
	if args["dialog_id"] != "d1" || args["session_id"] != "s1" || args["instance_id"] != "i1" {
		t.Fatalf("history 入参不符：%#v", args)
	}
}

func TestSessionsStopTasksStopAndAskAnswer(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/session.cancel", map[string]any{"cancelled": true})
	h.setResponse("plugin/v1/task.stop", map[string]any{"cancelled": false})
	h.setResponse("plugin/v1/ask.reply", nil)
	c := newTestClient(t, h, nil)

	v, err := c.rpcSessionsStop(context.Background(), "i1", map[string]any{"session_id": "s1"})
	if err != nil || v.(map[string]any)["cancelled"] != true {
		t.Fatalf("stop: %#v err=%v", v, err)
	}
	v, err = c.rpcTasksStop(context.Background(), "i1", map[string]any{"task_id": "tk1"})
	if err != nil || v.(map[string]any)["cancelled"] != false {
		t.Fatalf("tasks.stop: %#v err=%v", v, err)
	}

	c.asks.Put("a1", "d1")
	v, err = c.rpcAskAnswer(context.Background(), map[string]any{"ask_id": "a1", "answer": "x"})
	if err != nil || v.(map[string]any)["ok"] != true {
		t.Fatalf("ask.answer: %#v err=%v", v, err)
	}
	args := h.callsOf("plugin/v1/ask.reply")[0]
	if args["ask_id"] != "a1" || args["dialog_id"] != "d1" || args["answer"] != "x" {
		t.Fatalf("ask.reply 入参不符：%#v", args)
	}
}

func TestDispatchRPCMethodNotAllowed(t *testing.T) {
	c := newTestClient(t, newFakeHost(), nil)
	if _, err := c.dispatchRPC(context.Background(), "i1", "unknown.method", nil); !errors.Is(err, errMethodNotAllowed) {
		t.Fatalf("白名单外方法应返回 method_not_allowed，得到 %v", err)
	}
}

func TestInstanceForWorkspace(t *testing.T) {
	c := newTestClient(t, newFakeHost(), nil)
	c.ws.Register("i1", "/repo/proj", "", "")
	id := workspaceID("/repo/proj")
	if inst, ok := c.instanceFor(id); !ok || inst != "i1" {
		t.Fatalf("workspace → instance 反查不符：%q ok=%v", inst, ok)
	}
	if _, ok := c.instanceFor("missing"); ok {
		t.Fatal("未知 workspace 不应命中")
	}
}

func TestFilePassThrough(t *testing.T) {
	h := newFakeHost()
	h.setResponse("plugin/v1/file.list", map[string]any{"path": "/repo/proj", "is_dir": true, "children": []any{}})
	c := newTestClient(t, h, nil)
	v, err := c.rpcFile(context.Background(), "i1", map[string]any{"path": "/repo/proj"}, "plugin/v1/file.list")
	if err != nil {
		t.Fatalf("files.list: %v", err)
	}
	if v.(map[string]any)["is_dir"] != true {
		t.Fatalf("应原样返回：%#v", v)
	}
	args := h.callsOf("plugin/v1/file.list")[0]
	if args["instance_id"] != "i1" || args["path"] != "/repo/proj" {
		t.Fatalf("files.list 入参不符：%#v", args)
	}
}
