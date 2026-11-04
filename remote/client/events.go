package main

import (
	"encoding/json"
	"sync"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

// forwardedTopics 是插件订阅并归一为产品事件的 61 相对主题（26-remote §4.4）。
var forwardedTopics = []string{
	"session-receive",
	"session-turn-start",
	"session-complete",
	"session-ask",
	"session-inbox-changed",
	"task-started",
	"task-updated",
	"task-done",
	"tool-notify",
}

// normalizeEvent 把 61 事件归一为产品事件（type + data）；不认识的返回 ok=false。
func normalizeEvent(topic string, p map[string]any) (typ string, data map[string]any, ok bool) {
	sessionID := pluginsdk.Str(p["session"])
	turnID := pluginsdk.Str(p["turn"])
	switch topic {
	case "session-receive":
		inner := asMap(p["payload"])
		switch pluginsdk.Str(p["type"]) {
		case "text":
			return "message.delta", map[string]any{
				"session_id": sessionID, "turn_id": turnID, "text": payloadText(p["payload"]),
			}, true
		case "reason":
			return "message.reasoning", map[string]any{
				"session_id": sessionID, "turn_id": turnID, "text": payloadText(p["payload"]),
			}, true
		case "tool-call":
			name, status := "", ""
			if inner != nil {
				name = pluginsdk.Str(inner["tool"])
				status = pluginsdk.Str(inner["status"])
			}
			if status == "" {
				status = "running"
			}
			return "message.tool", map[string]any{
				"session_id": sessionID, "turn_id": turnID, "name": name, "status": status,
			}, true
		}
		return "", nil, false
	case "session-turn-start":
		return "turn.start", map[string]any{"session_id": sessionID, "turn_id": turnID}, true
	case "session-complete":
		d := map[string]any{
			"session_id":    sessionID,
			"turn_id":       turnID,
			"status":        pluginsdk.Str(p["status"]),
			"finish_reason": pluginsdk.Str(p["finish_reason"]),
		}
		if msg := strx.FirstNonEmpty(pluginsdk.Str(p["message"]), pluginsdk.Str(p["text"])); msg != "" {
			d["message"] = msg
		}
		return "turn.complete", d, true
	case "session-ask":
		d := map[string]any{
			"ask_id":     pluginsdk.Str(p["ask_id"]),
			"session_id": sessionID,
			"turn_id":    turnID,
			"question":   pluginsdk.Str(p["question"]),
		}
		if opts := asList(p["options"]); len(opts) > 0 {
			d["options"] = opts
		}
		if pluginsdk.Bool(p["multi"], false) {
			d["multi"] = true
		}
		if pluginsdk.Bool(p["custom"], false) {
			d["custom"] = true
		}
		return "ask", d, true
	case "session-inbox-changed":
		d := map[string]any{
			"session_id": sessionID,
			"action":     pluginsdk.Str(p["action"]),
		}
		if ids := asList(p["ids"]); len(ids) > 0 {
			d["ids"] = ids
		}
		return "inbox.changed", d, true
	case "task-started", "task-updated", "task-done":
		return "task.update", map[string]any{
			"task_id":    pluginsdk.Str(p["task_id"]),
			"session_id": sessionID,
			"turn_id":    turnID,
			"state":      pluginsdk.Str(p["state"]),
			"tool":       pluginsdk.Str(p["tool"]),
			"kind":       pluginsdk.Str(p["kind"]),
		}, true
	case "tool-notify":
		d := map[string]any{
			"message": strx.FirstNonEmpty(pluginsdk.Str(p["message"]), pluginsdk.Str(p["text"])),
		}
		putIf(d, "plugin", pluginsdk.Str(p["plugin"]))
		putIf(d, "kind", pluginsdk.Str(p["kind"]))
		return "notice", d, true
	}
	return "", nil, false
}

// payloadText 取 session-receive 内层 payload 的文本（对象 `{text}` 或裸字符串）。
func payloadText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return pluginsdk.Str(t["text"])
	}
	return ""
}

// runningSet 记录正在运行的会话（session-turn-start 加入 / session-complete 移除）。
type runningSet struct {
	mu   sync.Mutex
	live map[string]bool
}

func newRunningSet() *runningSet { return &runningSet{live: map[string]bool{}} }

func (r *runningSet) Add(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	r.live[sessionID] = true
	r.mu.Unlock()
}

func (r *runningSet) Remove(sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	delete(r.live, sessionID)
	r.mu.Unlock()
}

func (r *runningSet) Has(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[sessionID]
}

// askCache 缓存 ask_id → dialog_id（作答时回带，61 §0.6 必带）。
type askCache struct {
	mu    sync.Mutex
	items map[string]string
}

const askCacheCap = 2048

func newAskCache() *askCache { return &askCache{items: map[string]string{}} }

func (a *askCache) Put(askID, dialogID string) {
	if askID == "" {
		return
	}
	a.mu.Lock()
	if len(a.items) >= askCacheCap {
		a.items = map[string]string{}
	}
	a.items[askID] = dialogID
	a.mu.Unlock()
}

func (a *askCache) Get(askID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.items[askID]
}

// onInstanceRegister 登记实例（宿主注册订阅后会重放已有实例）。
func (s *client) onInstanceRegister(payload []byte) {
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		s.log.Debug("remote: instance-register 解析失败", "err", err)
		return
	}
	changed := s.ws.Register(
		pluginsdk.Str(p["instance_id"]),
		pluginsdk.Str(p["work_dir"]),
		pluginsdk.Str(p["data_dir"]),
		pluginsdk.Str(p["parent_instance"]),
	)
	if changed {
		s.sendWorkspaces()
	}
}

// onInstanceExit 注销实例并刷新工作区列表。
func (s *client) onInstanceExit(payload []byte) {
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		s.log.Debug("remote: instance-exit 解析失败", "err", err)
		return
	}
	if changed := s.ws.Remove(pluginsdk.Str(p["instance_id"])); changed {
		s.sendWorkspaces()
	}
}

// onBusEvent 订阅回调：维护运行集 / ask 缓存 → 工作区归属过滤 → 归一 → 推 Server。
func (s *client) onBusEvent(topic string, payload []byte) {
	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		s.log.Debug("remote: 事件解析失败", "topic", topic, "err", err)
		return
	}
	switch topic {
	case "session-turn-start":
		s.running.Add(pluginsdk.Str(p["session"]))
	case "session-complete":
		s.running.Remove(pluginsdk.Str(p["session"]))
	case "session-ask":
		s.asks.Put(pluginsdk.Str(p["ask_id"]), pluginsdk.Str(p["dialog_id"]))
	}
	ws, ok := s.ws.Resolve(pluginsdk.Str(p["work_dir"]), pluginsdk.Str(p["instance_id"]))
	if !ok {
		s.log.Debug("remote: 事件无匹配工作区，丢弃",
			"topic", topic, "instance_id", pluginsdk.Str(p["instance_id"]))
		return
	}
	typ, data, ok := normalizeEvent(topic, p)
	if !ok {
		s.log.Debug("remote: 事件类型不支持，丢弃", "topic", topic)
		return
	}
	if err := s.send(eventFrame(ws.ID, typ, data)); err != nil {
		s.log.Debug("remote: 发送事件失败", "err", err)
	}
}
