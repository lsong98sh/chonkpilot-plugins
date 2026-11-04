package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/gorilla/websocket"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

var (
	errMethodNotAllowed  = errors.New("method_not_allowed")
	errWorkspaceNotFound = errors.New("workspace_not_found")
)

// handleCall 处理 Server → 插件的 `call` 帧，按白名单映射到 `plugin/v1/*` 并回 result。
func (c *client) handleCall(ctx context.Context, conn *websocket.Conn, f wsFrame) {
	args := map[string]any{}
	if len(f.Args) > 0 {
		if err := json.Unmarshal(f.Args, &args); err != nil || args == nil {
			_ = c.write(conn, errorResultFrame(f.ID, "args_invalid"))
			return
		}
	}
	instanceID, ok := c.instanceFor(f.WorkspaceID)
	if !ok {
		_ = c.write(conn, errorResultFrame(f.ID, errWorkspaceNotFound.Error()))
		return
	}
	res, err := c.dispatchRPC(ctx, instanceID, f.Method, args)
	if err != nil {
		_ = c.write(conn, errorResultFrame(f.ID, err.Error()))
		return
	}
	_ = c.write(conn, resultFrame(f.ID, res))
}

// instanceFor 由 workspace_id 反查顶层实例 id。
func (c *client) instanceFor(workspaceID string) (string, bool) {
	for _, ws := range c.ws.List() {
		if ws.ID == workspaceID {
			return ws.InstanceID, true
		}
	}
	return "", false
}

// dispatchRPC 是设备 RPC 白名单（26-remote §4.4）。
func (c *client) dispatchRPC(ctx context.Context, instanceID, method string, args map[string]any) (any, error) {
	switch method {
	case "sessions.list":
		return c.rpcSessionsList(ctx, instanceID)
	case "sessions.history":
		return c.rpcSessionsHistory(ctx, instanceID, args)
	case "sessions.create":
		return c.rpcSessionsCreate(ctx, instanceID, args)
	case "sessions.send":
		return c.rpcSessionsSend(ctx, instanceID, args)
	case "sessions.inbox":
		return c.rpcSessionsInbox(ctx, instanceID, args)
	case "sessions.inbox.promote":
		return c.rpcSessionsInboxAction(ctx, instanceID, args, "plugin/v1/session.inbox.promote")
	case "sessions.inbox.revert":
		return c.rpcSessionsInboxAction(ctx, instanceID, args, "plugin/v1/session.inbox.revert")
	case "sessions.inbox.discard":
		return c.rpcSessionsInboxAction(ctx, instanceID, args, "plugin/v1/session.inbox.discard")
	case "sessions.stop":
		return c.rpcSessionsStop(ctx, instanceID, args)
	case "ask.answer":
		return c.rpcAskAnswer(ctx, args)
	case "tasks.tree":
		return c.rpcTaskTree(ctx, instanceID, args, "plugin/v1/task.tree")
	case "tasks.tasks":
		return c.rpcTaskTree(ctx, instanceID, args, "plugin/v1/task.tasks")
	case "tasks.stop":
		return c.rpcTasksStop(ctx, instanceID, args)
	case "files.list":
		return c.rpcFile(ctx, instanceID, args, "plugin/v1/file.list")
	case "files.read":
		return c.rpcFile(ctx, instanceID, args, "plugin/v1/file.read")
	}
	return nil, errMethodNotAllowed
}

// cacheDialog 记录 session → dialog（宿主「一树一库」：会话内容只在对话库内）。
func (c *client) cacheDialog(sessionID, dialogID string) {
	sessionID, dialogID = strings.TrimSpace(sessionID), strings.TrimSpace(dialogID)
	if sessionID == "" || dialogID == "" {
		return
	}
	c.dialogMu.Lock()
	c.dialogs[sessionID] = dialogID
	c.dialogMu.Unlock()
}

func (c *client) dialogFor(sessionID string) string {
	c.dialogMu.Lock()
	defer c.dialogMu.Unlock()
	return c.dialogs[strings.TrimSpace(sessionID)]
}

// resolveDialog 从宿主会话列表反查 dialog_id（新建会话后调用）。
func (c *client) resolveDialog(ctx context.Context, instanceID, sessionID string) string {
	if d := c.dialogFor(sessionID); d != "" {
		return d
	}
	v, err := c.callRaw(ctx, "plugin/v1/session.list", map[string]any{"instance_id": instanceID})
	if err != nil {
		return ""
	}
	for _, item := range asList(asMap(v)["list"]) {
		m := asMap(item)
		sid := strx.FirstNonEmpty(pluginsdk.Str(m["session_id"]), pluginsdk.Str(m["id"]))
		did := pluginsdk.Str(m["dialog_id"])
		if sid != "" && did != "" {
			c.cacheDialog(sid, did)
		}
		if sid == sessionID {
			return did
		}
	}
	return ""
}

// callRaw 调宿主能力面并把结果解成通用 JSON 值。
func (c *client) callRaw(ctx context.Context, method string, args map[string]any) (any, error) {
	raw, err := c.host.Call(ctx, method, args)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func callRequestID() string { return "remote-" + randomHex(4) }

// rpcSessionsList → `{list:[{session_id,dialog_id,title,updated_at,running}]}`。
func (c *client) rpcSessionsList(ctx context.Context, instanceID string) (any, error) {
	v, err := c.callRaw(ctx, "plugin/v1/session.list", map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, err
	}
	items := asList(asMap(v)["list"])
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		m := asMap(item)
		if m == nil {
			continue
		}
		sessionID := strx.FirstNonEmpty(pluginsdk.Str(m["session_id"]), pluginsdk.Str(m["id"]))
		dialogID := pluginsdk.Str(m["dialog_id"])
		c.cacheDialog(sessionID, dialogID)
		out = append(out, map[string]any{
			"session_id": sessionID,
			"dialog_id":  dialogID,
			"title":      pluginsdk.Str(m["title"]),
			"updated_at": strx.FirstNonEmpty(pluginsdk.Str(m["last_activity"]), pluginsdk.Str(m["updated_at"])),
			"running":    c.running.Has(sessionID),
		})
	}
	return map[string]any{"list": out}, nil
}

// buildHistoryArgs 拼装 `plugin/v1/session.history` 入参（空值不下发）。
//
// 分页口径（H5 `?before=&limit=`）：`before` = 上一页最早一条的 **turn_id**，映射到宿主
// `before_turn_id`；`limit` 映射到宿主 `target_messages`（页大小）。
func buildHistoryArgs(instanceID string, args map[string]any) map[string]any {
	out := map[string]any{
		"instance_id": instanceID,
		"session_id":  pluginsdk.Str(args["session_id"]),
	}
	before := pluginsdk.Str(args["before_turn_id"])
	if before == "" {
		before = pluginsdk.Str(args["before"])
	}
	putIf(out, "before_turn_id", before)
	putIf(out, "dialog_id", pluginsdk.Str(args["dialog_id"]))
	n := pluginsdk.Int(args["target_messages"])
	if n <= 0 {
		n = pluginsdk.Int(args["limit"])
	}
	if n > 0 {
		out["target_messages"] = n
	}
	return out
}

// rpcSessionsHistory → `{messages:[…], has_more}`（兼容宿主 `{messages:{…}}` 信封）。
func (c *client) rpcSessionsHistory(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	if pluginsdk.Str(args["dialog_id"]) == "" {
		sid := pluginsdk.Str(args["session_id"])
		dlg := c.dialogFor(sid)
		if dlg == "" {
			dlg = c.resolveDialog(ctx, instanceID, sid) // 插件重启后缓存为空 → 反查一次
		}
		putIf(args, "dialog_id", dlg)
	}
	v, err := c.callRaw(ctx, "plugin/v1/session.history", buildHistoryArgs(instanceID, args))
	if err != nil {
		return nil, err
	}
	root := asMap(v)
	var raw []any
	var hasMore bool
	if inner := asMap(root["messages"]); inner != nil {
		raw = asList(inner["messages"])
		hasMore = pluginsdk.Bool(inner["has_more"], false)
	} else {
		raw = asList(root["messages"])
		hasMore = pluginsdk.Bool(root["has_more"], false)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m := asMap(item); m != nil {
			out = append(out, normalizeHistoryMessage(m))
		}
	}
	return map[string]any{"messages": out, "has_more": hasMore}, nil
}

// normalizeHistoryMessage 把宿主消息视图归一为 H5 消费形状。
func normalizeHistoryMessage(m map[string]any) map[string]any {
	out := map[string]any{
		"id":         pluginsdk.Str(m["message_id"]),
		"role":       pluginsdk.Str(m["role"]),
		"text":       pluginsdk.Str(m["content"]),
		"reasoning":  pluginsdk.Str(m["reasoning"]),
		"type":       pluginsdk.Str(m["type"]),
		"turn_id":    pluginsdk.Str(m["turn_id"]),
		"created_at": pluginsdk.Str(m["created_at"]),
	}
	putIf(out, "tool_name", toolNameOf(m))
	putIf(out, "tool_status", pluginsdk.Str(m["tool_call_status"]))
	return out
}

// toolNameOf 取工具卡片名（tool_pair 行带 `tool`；普通消息取 tool_calls[0].name）。
func toolNameOf(m map[string]any) string {
	if name := pluginsdk.Str(m["tool"]); name != "" {
		return name
	}
	switch calls := m["tool_calls"].(type) {
	case string:
		var arr []any
		if json.Unmarshal([]byte(calls), &arr) != nil {
			return ""
		}
		return firstToolName(arr)
	case []any:
		return firstToolName(calls)
	}
	return ""
}

func firstToolName(calls []any) string {
	if len(calls) == 0 {
		return ""
	}
	first := asMap(calls[0])
	if first == nil {
		return ""
	}
	if fn := asMap(first["function"]); fn != nil {
		if name := pluginsdk.Str(fn["name"]); name != "" {
			return name
		}
	}
	if name := pluginsdk.Str(first["name"]); name != "" {
		return name
	}
	return pluginsdk.Str(first["tool"])
}

// rpcSessionsCreate：分配 session/turn → start →（可选）send，返回 `{session_id,turn_id}`。
func (c *client) rpcSessionsCreate(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	sessionID := "s_" + randomHex(6)
	turnID := "t_" + randomHex(6)
	if rej, err := c.startTurn(ctx, instanceID, sessionID, turnID); err != nil || rej != nil {
		return rej, err
	}
	if text := pluginsdk.Str(args["text"]); text != "" {
		if rej, err := c.sendText(ctx, instanceID, sessionID, turnID, text); err != nil || rej != nil {
			return rej, err
		}
	}
	out := map[string]any{"session_id": sessionID, "turn_id": turnID}
	// 新建会话后立刻反查 dialog_id 并回给 H5（后续 history/send 必须带）。
	if d := c.resolveDialog(ctx, instanceID, sessionID); d != "" {
		out["dialog_id"] = d
	}
	return out, nil
}

// rpcSessionsSend：H5 发送**一律进入 inbox**（`llm-inbox-add`，source=mobile）；`insert=true`
// = 手机端「插话」（登记插话意向，可插入相位批量插入），缺省 = 排队等下一轮。
// 空闲（无运行轮）时宿主立即消费起新一轮。
func (c *client) rpcSessionsSend(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	sessionID := pluginsdk.Str(args["session_id"])
	text := pluginsdk.Str(args["text"])
	if sessionID == "" || strings.TrimSpace(text) == "" {
		return map[string]any{"accepted": false, "error": "empty text"}, nil
	}
	insert := pluginsdk.Bool(args["insert"], false)
	queued := c.running.Has(sessionID) || insert
	payload := map[string]any{
		"instance_id": instanceID,
		"session":     sessionID,
		"content":     text,
		"source":      "mobile",
		"insert":      insert,
		"request_id":  callRequestID(),
	}
	v, err := c.callRaw(ctx, "plugin/v1/session.inbox.add", payload)
	if err != nil {
		return nil, err
	}
	m := asMap(v)
	if !pluginsdk.Bool(m["ok"], false) {
		msg := pluginsdk.Str(m["error"])
		if msg == "" {
			msg = "入队失败"
		}
		return map[string]any{"accepted": false, "error": msg}, nil
	}
	out := map[string]any{
		"accepted":   true,
		"queued":     queued,
		"session_id": sessionID,
		"type":       pluginsdk.Str(m["type"]),
	}
	putIf(out, "inbox_id", pluginsdk.Str(m["id"]))
	return out, nil
}

// rpcSessionsInbox：读该会话**全量** pending inbox（H5 进入会话 / 变更事件后刷新）。
// 2026-11-01：两端共享同一 inbox —— 不按来源 / 类型过滤（手机也能插话、删除、撤回）。
func (c *client) rpcSessionsInbox(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	sessionID := pluginsdk.Str(args["session_id"])
	v, err := c.callRaw(ctx, "plugin/v1/session.inbox.list", map[string]any{
		"instance_id": instanceID,
		"session":     sessionID,
		"status":      "pending",
	})
	if err != nil {
		return nil, err
	}
	root := asMap(v)
	in := asList(root["items"])
	out := make([]map[string]any, 0, len(in))
	for _, item := range in {
		m := asMap(item)
		if m == nil {
			continue
		}
		row := map[string]any{
			"id":         pluginsdk.Str(m["id"]),
			"content":    pluginsdk.Str(m["content"]),
			"type":       pluginsdk.Str(m["type"]),
			"created_at": pluginsdk.Str(m["created_at"]),
		}
		putIf(row, "source", pluginsdk.Str(m["source"]))
		out = append(out, row)
	}
	return map[string]any{"items": out}, nil
}

// rpcSessionsInboxAction：inbox 项操作（插话意向 / 取消意向 / 删除·撤回）。
// 入参 `{session_id, id}`；返回 `{ok, type?}`。H5 收到应答后由 `inbox.changed` 事件刷新列表。
func (c *client) rpcSessionsInboxAction(ctx context.Context, instanceID string, args map[string]any, method string) (any, error) {
	id := pluginsdk.Str(args["id"])
	if id == "" {
		return map[string]any{"ok": false, "error": "empty id"}, nil
	}
	payload := map[string]any{
		"instance_id": instanceID,
		"id":          id,
		"request_id":  callRequestID(),
	}
	v, err := c.callRaw(ctx, method, payload)
	if err != nil {
		return nil, err
	}
	m := asMap(v)
	out := map[string]any{"ok": pluginsdk.Bool(m["ok"], false)}
	putIf(out, "type", pluginsdk.Str(m["type"]))
	putIf(out, "error", pluginsdk.Str(m["error"]))
	return out, nil
}

// startTurn：受理新轮次（`accepted:false` 原样作为 RPC 结果返回）。
func (c *client) startTurn(ctx context.Context, instanceID, sessionID, turnID string) (map[string]any, error) {
	payload := map[string]any{
		"instance_id": instanceID,
		"session":     sessionID,
		"turn":        turnID,
		"request_id":  callRequestID(),
	}
	putIf(payload, "dialog_id", c.dialogFor(sessionID))
	v, err := c.callRaw(ctx, "plugin/v1/session.start", payload)
	if err != nil {
		return nil, err
	}
	return acceptedRejection(v), nil
}

// sendText 把用户文本注入指定轮次。
func (c *client) sendText(ctx context.Context, instanceID, sessionID, turnID, text string) (map[string]any, error) {
	payload := map[string]any{
		"instance_id": instanceID,
		"session":     sessionID,
		"turn":        turnID,
		"type":        "text-user",
		"content":     text,
		"request_id":  callRequestID(),
	}
	v, err := c.callRaw(ctx, "plugin/v1/session.send", payload)
	if err != nil {
		return nil, err
	}
	return acceptedRejection(v), nil
}

// acceptedRejection 把 `accepted:false` 应答原样转成 RPC 结果；其它 → nil。
func acceptedRejection(v any) map[string]any {
	m := asMap(v)
	if m == nil {
		return nil
	}
	accepted, ok := m["accepted"].(bool)
	if !ok || accepted {
		return nil
	}
	out := map[string]any{"accepted": false}
	if e := pluginsdk.Str(m["error"]); e != "" {
		out["error"] = e
	}
	return out
}

// rpcSessionsStop → `{cancelled}`。
func (c *client) rpcSessionsStop(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	payload := map[string]any{
		"instance_id": instanceID,
		"session":     pluginsdk.Str(args["session_id"]),
		"request_id":  callRequestID(),
	}
	putIf(payload, "dialog_id", c.dialogFor(pluginsdk.Str(args["session_id"])))
	v, err := c.callRaw(ctx, "plugin/v1/session.cancel", payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{"cancelled": cancelledValue(v)}, nil
}

// rpcAskAnswer → `{ok}`（dialog_id 取事件缓存）。
func (c *client) rpcAskAnswer(ctx context.Context, args map[string]any) (any, error) {
	askID := pluginsdk.Str(args["ask_id"])
	dialogID := pluginsdk.Str(args["dialog_id"])
	if dialogID == "" {
		dialogID = c.asks.Get(askID)
	}
	payload := map[string]any{
		"ask_id":     askID,
		"answer":     pluginsdk.Str(args["answer"]),
		"dialog_id":  dialogID,
		"request_id": callRequestID(),
	}
	putIf(payload, "custom", pluginsdk.Str(args["custom"]))
	if _, err := c.callRaw(ctx, "plugin/v1/ask.reply", payload); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// rpcTaskTree 原样返回任务树 / 任务列表。
func (c *client) rpcTaskTree(ctx context.Context, instanceID string, args map[string]any, method string) (any, error) {
	sessionID := pluginsdk.Str(args["session_id"])
	payload := map[string]any{
		"instance_id": instanceID,
		"top_session": sessionID,
	}
	// 任务树落对话库（一树一库）：宿主强制要求 dialog_id。
	dlg := c.dialogFor(sessionID)
	if dlg == "" {
		dlg = c.resolveDialog(ctx, instanceID, sessionID)
	}
	putIf(payload, "dialog_id", dlg)
	return c.callRaw(ctx, method, payload)
}

// rpcTasksStop → `{cancelled}`。
func (c *client) rpcTasksStop(ctx context.Context, instanceID string, args map[string]any) (any, error) {
	payload := map[string]any{
		"task_id":     pluginsdk.Str(args["task_id"]),
		"instance_id": instanceID,
		"request_id":  callRequestID(),
	}
	v, err := c.callRaw(ctx, "plugin/v1/task.stop", payload)
	if err != nil {
		return nil, err
	}
	return map[string]any{"cancelled": cancelledValue(v)}, nil
}

// rpcFile 原样返回文件列表 / 内容。
func (c *client) rpcFile(ctx context.Context, instanceID string, args map[string]any, method string) (any, error) {
	payload := map[string]any{"instance_id": instanceID}
	putIf(payload, "path", pluginsdk.Str(args["path"]))
	return c.callRaw(ctx, method, payload)
}

// cancelledValue 取宿主应答的 cancelled（缺省视为成功取消）。
func cancelledValue(v any) bool {
	m := asMap(v)
	if m == nil {
		return true
	}
	if b, ok := m["cancelled"].(bool); ok {
		return b
	}
	return true
}
