// Command fakedevice pretends to be a desktop ChonkPilot on the remote tunnel.
// It exists so the H5 client can be exercised end to end without a real
// desktop, and it is also used to sanity-check the server by hand.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const demoWorkspaceID = "ws-demo"

type workspace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	InstanceID string `json:"instance_id,omitempty"`
}

type sessionItem struct {
	SessionID   string `json:"session_id"`
	DialogID    string `json:"dialog_id"`
	Title       string `json:"title"`
	UpdatedAt   int64  `json:"updated_at"`
	Running     bool   `json:"running"`
	WorkspaceID string `json:"-"`
}

type frame struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	WorkspaceID string          `json:"workspace_id"`
	Method      string          `json:"method"`
	Args        json.RawMessage `json:"args"`
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Error       string          `json:"error"`
	DeviceID    string          `json:"device_id"`
	Token       string          `json:"token"`
	ServerTime  int64           `json:"server_time"`
}

type fakeDevice struct {
	ws *websocket.Conn

	writeMu sync.Mutex

	mu       sync.Mutex
	sessions []sessionItem
	messages map[string][]map[string]any
	// inbox 模拟桌面 inbox（手机端 source=mobile 排队项；轮末自动消费）。
	inbox map[string][]fakeInboxItem
	seq   int
}

// fakeInboxItem 是模拟设备侧的排队项（与宿主 llm-inbox-list 项同形）。
type fakeInboxItem struct {
	ID        string
	Content   string
	CreatedAt int64
	Type      string // next_turn | insert（insert = 已登记插话意向）
	Source    string // mobile | ""（桌面）
}

func main() {
	server := flag.String("server", "ws://127.0.0.1:8080/tunnel", "tunnel WebSocket URL")
	username := flag.String("user", "", "account username")
	password := flag.String("pass", "", "account password")
	deviceID := flag.String("device", "fake-pc", "stable device id")
	token := flag.String("token", "", "reconnect token (optional, overrides -user/-pass)")
	flag.Parse()

	if *username == "" && *token == "" {
		*username = os.Getenv("CK_REMOTE_USER")
	}
	if *password == "" && *token == "" {
		*password = os.Getenv("CK_REMOTE_PASSWORD")
	}
	if *token == "" && (*username == "" || *password == "") {
		log.Fatal("usage: fakedevice -user <name> -pass <password> [-device fake-pc] [-server ws://...]")
	}

	conn, _, err := websocket.DefaultDialer.Dial(*server, nil)
	if err != nil {
		log.Fatalf("dial %s: %v", *server, err)
	}
	defer func() { _ = conn.Close() }()

	fd := newFakeDevice(conn)

	hello := map[string]any{
		"type":           "hello",
		"device":         map[string]string{"id": *deviceID, "name": "Fake PC", "platform": "windows", "version": "1.0.0"},
		"client_version": "fakedevice/1",
		"protocol":       "remote/1",
		"workspaces": []workspace{
			{ID: demoWorkspaceID, Name: "Demo 项目", Path: `E:\demo`, InstanceID: "inst-demo"},
			{ID: "ws-demo2", Name: "Demo 项目 2", Path: `E:\demo2`, InstanceID: "inst-demo2"},
		},
	}
	if *token != "" {
		hello["token"] = *token
	} else {
		hello["username"] = *username
		hello["password"] = *password
	}
	fd.send(hello)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Printf("tunnel closed: %v", err)
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case "hello-ok":
			if f.OK {
				log.Printf("connected: device_id=%s token=%s", f.DeviceID, f.Token)
			} else {
				log.Printf("hello rejected: %s", f.Error)
				return
			}
		case "ping":
			fd.send(map[string]any{"type": "pong"})
		case "call":
			fd.handleCall(f)
		}
	}
}

func newFakeDevice(ws *websocket.Conn) *fakeDevice {
	now := time.Now().Unix()
	fd := &fakeDevice{
		ws:       ws,
		messages: map[string][]map[string]any{},
		inbox:    map[string][]fakeInboxItem{},
		sessions: []sessionItem{
			{SessionID: "s-1", DialogID: "d-1", Title: "示例会话 A", UpdatedAt: now - 60, WorkspaceID: demoWorkspaceID},
			{SessionID: "s-2", DialogID: "d-2", Title: "示例会话 B", UpdatedAt: now - 120, WorkspaceID: demoWorkspaceID},
			{SessionID: "s-3", DialogID: "d-3", Title: "第二个项目的会话", UpdatedAt: now - 30, WorkspaceID: "ws-demo2"},
		},
	}
	fd.messages["s-1"] = []map[string]any{
		{"id": "m-1", "role": "user", "text": "你好", "turn_id": "t-1", "created_at": now - 180},
		{"id": "m-2", "role": "assistant", "text": "你好，我是假设备。", "turn_id": "t-1", "created_at": now - 170},
	}
	fd.messages["s-2"] = []map[string]any{
		{"id": "m-3", "role": "user", "text": "看看项目结构", "turn_id": "t-2", "created_at": now - 300},
	}
	return fd
}

func (f *fakeDevice) send(v any) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_ = f.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := f.ws.WriteJSON(v); err != nil {
		log.Printf("write: %v", err)
	}
}

func (f *fakeDevice) emit(eventType string, data any) {
	f.emitFor(demoWorkspaceID, eventType, data)
}

func (f *fakeDevice) emitFor(workspaceID, eventType string, data any) {
	if workspaceID == "" {
		workspaceID = demoWorkspaceID
	}
	f.send(map[string]any{
		"type":         eventType,
		"workspace_id": workspaceID,
		"data":         data,
	})
}

func (f *fakeDevice) workspaceOf(sessionID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.sessions {
		if f.sessions[i].SessionID == sessionID && f.sessions[i].WorkspaceID != "" {
			return f.sessions[i].WorkspaceID
		}
	}
	return demoWorkspaceID
}

func (f *fakeDevice) nextSessionID() string {
	f.seq++
	return fmt.Sprintf("s-new-%d", f.seq)
}

func (f *fakeDevice) nextTurnID() string {
	n := atomic.AddInt32(&fakeTurnSeq, 1)
	return fmt.Sprintf("turn-%d", n)
}

var fakeTurnSeq int32

func (f *fakeDevice) handleCall(c frame) {
	var args map[string]any
	if len(c.Args) > 0 {
		_ = json.Unmarshal(c.Args, &args)
	}
	result, err := f.dispatch(c.WorkspaceID, c.Method, args)
	if err != nil {
		f.send(map[string]any{"type": "result", "id": c.ID, "ok": false, "error": err.Error()})
		return
	}
	f.send(map[string]any{"type": "result", "id": c.ID, "ok": true, "result": result})
}

var errMethodNotAllowed = errors.New("method_not_allowed")

func (f *fakeDevice) dispatch(workspaceID, method string, args map[string]any) (any, error) {
	switch method {
	case "sessions.list":
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make([]sessionItem, 0, len(f.sessions))
		for _, s := range f.sessions {
			if workspaceID == "" || s.WorkspaceID == "" || s.WorkspaceID == workspaceID {
				out = append(out, s)
			}
		}
		return out, nil

	case "sessions.create":
		text, _ := args["text"].(string)
		f.mu.Lock()
		s := sessionItem{
			SessionID:   f.nextSessionID(),
			DialogID:    fmt.Sprintf("d-%d", f.seq),
			Title:       titleFromText(text),
			UpdatedAt:   time.Now().Unix(),
			WorkspaceID: workspaceID,
		}
		f.sessions = append([]sessionItem{s}, f.sessions...)
		f.messages[s.SessionID] = []map[string]any{}
		f.mu.Unlock()
		turnID := f.nextTurnID()
		if strings.TrimSpace(text) != "" {
			f.startTurn(s.SessionID, turnID, text)
		}
		return map[string]any{"session_id": s.SessionID, "turn_id": turnID}, nil

	case "sessions.history":
		sessionID, _ := args["session_id"].(string)
		f.mu.Lock()
		msgs := f.messages[sessionID]
		out := make([]map[string]any, len(msgs))
		copy(out, msgs)
		f.mu.Unlock()
		return map[string]any{"messages": out, "has_more": false}, nil

	case "sessions.send":
		sessionID, _ := args["session_id"].(string)
		text, _ := args["text"].(string)
		f.mu.Lock()
		found := false
		running := false
		for i := range f.sessions {
			if f.sessions[i].SessionID == sessionID {
				found = true
				running = f.sessions[i].Running
				if !running {
					f.sessions[i].UpdatedAt = time.Now().Unix()
					f.sessions[i].Running = true
				}
				break
			}
		}
		if !found {
			f.mu.Unlock()
			return nil, fmt.Errorf("session not found")
		}
		if running {
			// 手机端发送走 inbox（source=mobile）：运行中 → 排队 / 插话意向，本轮结束后自动发送。
			typ := "next_turn"
			if ins, _ := args["insert"].(bool); ins {
				typ = "insert"
			}
			item := fakeInboxItem{ID: fmt.Sprintf("in-%d", time.Now().UnixNano()), Content: text,
				CreatedAt: time.Now().Unix(), Type: typ, Source: "mobile"}
			f.inbox[sessionID] = append(f.inbox[sessionID], item)
			f.mu.Unlock()
			return map[string]any{"accepted": true, "queued": true, "type": typ,
				"session_id": sessionID, "inbox_id": item.ID}, nil
		}
		f.mu.Unlock()
		f.startTurn(sessionID, f.nextTurnID(), text)
		return map[string]any{"accepted": true, "queued": false, "type": "next_turn", "session_id": sessionID}, nil

	case "sessions.inbox":
		sessionID, _ := args["session_id"].(string)
		f.mu.Lock()
		items := f.inbox[sessionID]
		out := make([]map[string]any, 0, len(items))
		for _, it := range items {
			row := map[string]any{"id": it.ID, "content": it.Content,
				"type": it.Type, "created_at": it.CreatedAt}
			if it.Source != "" {
				row["source"] = it.Source
			}
			out = append(out, row)
		}
		f.mu.Unlock()
		return map[string]any{"items": out}, nil

	case "sessions.inbox.promote", "sessions.inbox.revert", "sessions.inbox.discard":
		sessionID, _ := args["session_id"].(string)
		id, _ := args["id"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		items := f.inbox[sessionID]
		for i := range items {
			if items[i].ID != id {
				continue
			}
			switch method {
			case "sessions.inbox.promote":
				items[i].Type = "insert"
			case "sessions.inbox.revert":
				items[i].Type = "next_turn"
			default:
				f.inbox[sessionID] = append(items[:i], items[i+1:]...)
			}
			return map[string]any{"ok": true, "type": "next_turn"}, nil
		}
		return map[string]any{"ok": false}, nil

	case "sessions.stop":
		sessionID, _ := args["session_id"].(string)
		f.mu.Lock()
		for i := range f.sessions {
			if f.sessions[i].SessionID == sessionID {
				f.sessions[i].Running = false
			}
		}
		f.mu.Unlock()
		return map[string]any{"cancelled": true}, nil

	case "ask.answer":
		return map[string]any{"ok": true}, nil

	case "tasks.tree":
		sessionID, _ := args["top_session"].(string)
		if sessionID == "" {
			sessionID, _ = args["session_id"].(string)
		}
		return map[string]any{
			"tasks": []map[string]any{
				{
					"task_id":    "task-1",
					"session_id": sessionID,
					"turn_id":    "t-1",
					"state":      "done",
					"tool":       "bash",
					"kind":       "task",
					"title":      "扫描仓库",
					"updated_at": time.Now().Unix(),
					"args":       map[string]any{"path": `E:\demo`, "pattern": "*.go"},
					"children": []map[string]any{
						{"task_id": "task-1-1", "title": "读取 README", "state": "done", "tool": "file_read"},
					},
				},
			},
		}, nil

	case "tasks.tasks":
		return map[string]any{"tasks": []map[string]any{}}, nil

	case "tasks.stop":
		return map[string]any{"cancelled": true}, nil

	case "files.list":
		p, _ := args["path"].(string)
		if strings.Contains(p, "src") {
			return map[string]any{
				"path": p,
				"entries": []map[string]any{
					{"name": "main.go", "path": joinPath(p, "main.go"), "is_dir": false, "size": 1280},
				},
			}, nil
		}
		return map[string]any{
			"path": p,
			"entries": []map[string]any{
				{"name": "README.md", "path": joinPath(p, "README.md"), "is_dir": false, "size": 42},
				{"name": "src", "path": joinPath(p, "src"), "is_dir": true, "size": 0},
			},
		}, nil

	case "files.read":
		p, _ := args["path"].(string)
		return map[string]any{
			"path":    p,
			"content": "# Demo 项目\n\n这是假设备返回的文件内容。\n",
		}, nil

	default:
		return nil, errMethodNotAllowed
	}
}

func joinPath(base, name string) string {
	base = strings.TrimRight(base, `/\`)
	if base == "" {
		return name
	}
	return base + "/" + name
}

func titleFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "新会话"
	}
	runes := []rune(text)
	if len(runes) > 20 {
		runes = runes[:20]
	}
	return string(runes)
}

// startTurn pushes the scripted turn.start -> 3x message.delta -> turn.complete
// event sequence so the H5 client can be tested without a desktop.
func (f *fakeDevice) startTurn(sessionID, turnID, text string) {
	wsID := f.workspaceOf(sessionID)
	go func() {
		f.emitFor(wsID, "turn.start", map[string]any{"session_id": sessionID, "turn_id": turnID})
		reply := "收到：" + strings.TrimSpace(text) + "（这是假设备的模拟回复）"
		for _, chunk := range split3(reply) {
			time.Sleep(300 * time.Millisecond)
			f.emitFor(wsID, "message.delta", map[string]any{
				"session_id": sessionID,
				"turn_id":    turnID,
				"text":       chunk,
			})
		}
		f.emitFor(wsID, "turn.complete", map[string]any{
			"session_id": sessionID,
			"turn_id":    turnID,
			"status":     "complete",
		})
		f.mu.Lock()
		for i := range f.sessions {
			if f.sessions[i].SessionID == sessionID {
				f.sessions[i].Running = false
				f.sessions[i].UpdatedAt = time.Now().Unix()
			}
		}
		f.messages[sessionID] = append(f.messages[sessionID],
			map[string]any{"id": fmt.Sprintf("m-%d", time.Now().UnixNano()), "role": "assistant", "text": reply, "turn_id": turnID, "created_at": time.Now().Unix()},
		)
		// 轮末消费 inbox：排队项落 user 消息并自动起下一轮（模拟宿主 consumeNextTurn）。
		pending := f.inbox[sessionID]
		f.inbox[sessionID] = nil
		var nextText string
		for _, it := range pending {
			if nextText != "" {
				nextText += "\n"
			}
			nextText += it.Content
			f.messages[sessionID] = append(f.messages[sessionID],
				map[string]any{"id": it.ID, "role": "user", "text": it.Content, "turn_id": "", "created_at": it.CreatedAt},
			)
		}
		f.mu.Unlock()
		if nextText != "" {
			f.startTurn(sessionID, f.nextTurnID(), nextText)
		}

		// 文本含「问答」时补一个 ask 事件，供 H5 作答面板联调。
		if strings.Contains(text, "问答") || strings.Contains(text, "ask") {
			time.Sleep(200 * time.Millisecond)
			f.emitFor(wsID, "ask", map[string]any{
				"ask_id":     "ask-" + turnID,
				"session_id": sessionID,
				"turn_id":    turnID,
				"question":   "要继续执行下一步吗？",
				"options": []map[string]any{
					{"value": "yes", "label": "继续"},
					{"value": "no", "label": "停止"},
				},
				"multi":  false,
				"custom": true,
			})
		}
	}()
}

func split3(s string) []string {
	runes := []rune(s)
	n := len(runes)
	parts := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		start := n * i / 3
		end := n * (i + 1) / 3
		parts = append(parts, string(runes[start:end]))
	}
	return parts
}
