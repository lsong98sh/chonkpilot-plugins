package main

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testDevice is a minimal desktop stand-in for tunnel tests.
type testDevice struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
	calls   chan tunnelFrame
	onCall  func(tunnelFrame) map[string]any
}

func (d *testDevice) send(v any) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_ = d.ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = d.ws.WriteJSON(v)
}

func dialTestDevice(t *testing.T, ts *httptest.Server, hello map[string]any, onCall func(tunnelFrame) map[string]any) (*testDevice, map[string]any) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	if err := ws.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ack map[string]any
	if err := ws.ReadJSON(&ack); err != nil {
		t.Fatalf("read hello-ok: %v", err)
	}
	_ = ws.SetReadDeadline(time.Time{})

	d := &testDevice{
		ws:     ws,
		calls:  make(chan tunnelFrame, 16),
		onCall: onCall,
	}
	go func() {
		for {
			var f tunnelFrame
			if err := ws.ReadJSON(&f); err != nil {
				return
			}
			switch f.Type {
			case "call":
				select {
				case d.calls <- f:
				default:
				}
				res := d.onCall(f)
				res["type"] = "result"
				res["id"] = f.ID
				d.send(res)
			case "ping":
				d.send(map[string]any{"type": "pong"})
			}
		}
	}()
	return d, ack
}

func helloFrame(deviceID, username, password string, workspaces []map[string]any) map[string]any {
	return map[string]any{
		"type":           "hello",
		"username":       username,
		"password":       password,
		"client_version": "test/1",
		"protocol":       "remote/1",
		"device": map[string]any{
			"id":       deviceID,
			"name":     "Test PC",
			"platform": "windows",
			"version":  "1.0.0",
		},
		"workspaces": workspaces,
	}
}

func TestTunnelCallRoundTrip(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	device, ack := dialTestDevice(t, ts, helloFrame("dev-1", "alice", "secret", []map[string]any{
		{"id": "ws-demo", "name": "Demo", "path": `E:\demo`, "instance_id": "inst-1"},
	}), func(f tunnelFrame) map[string]any {
		switch f.Method {
		case "sessions.list":
			return map[string]any{"ok": true, "result": []map[string]any{
				{"session_id": "s-1", "dialog_id": "d-1", "title": "Hello", "updated_at": 1, "running": false},
			}}
		case "sessions.send":
			return map[string]any{"ok": true, "result": map[string]any{"accepted": true}}
		default:
			return map[string]any{"ok": false, "error": "method_not_allowed"}
		}
	})

	if ack["ok"] != true {
		t.Fatalf("hello-ok not accepted: %+v", ack)
	}
	if ack["device_id"] != "dev-1" {
		t.Fatalf("hello-ok device_id = %v", ack["device_id"])
	}
	if token, _ := ack["token"].(string); token == "" {
		t.Fatalf("hello-ok did not return a device token")
	}

	client := newTestClient(t, ts, "alice", "secret")

	resp := doRequest(t, client, http.MethodPost, ts.URL+"/api/context", map[string]string{"device_id": "dev-1", "workspace_id": "ws-demo"})
	expectStatus(t, resp, http.StatusOK)

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/devices", nil)
	var devices []map[string]any
	expectJSON(t, resp, http.StatusOK, &devices)
	if len(devices) != 1 || devices[0]["online"] != true {
		t.Fatalf("device should be online: %+v", devices)
	}
	if devices[0]["protocol"] != "remote/1" {
		t.Fatalf("protocol = %v", devices[0]["protocol"])
	}

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/sessions", nil)
	var sessions []map[string]any
	expectJSON(t, resp, http.StatusOK, &sessions)
	if len(sessions) != 1 || sessions[0]["session_id"] != "s-1" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}

	select {
	case call := <-device.calls:
		if call.Method != "sessions.list" {
			t.Fatalf("method = %q", call.Method)
		}
		if call.WorkspaceID != "ws-demo" {
			t.Fatalf("workspace_id = %q", call.WorkspaceID)
		}
		if call.ID == "" {
			t.Fatalf("call id is empty")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("device never received the rpc call")
	}

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/sessions/s-1/messages", map[string]string{"text": "hi"})
	var sendResult struct {
		Accepted bool `json:"accepted"`
	}
	expectJSON(t, resp, http.StatusOK, &sendResult)
	if !sendResult.Accepted {
		t.Fatalf("send not accepted: %+v", sendResult)
	}
}

func TestTunnelCallMethodNotAllowed(t *testing.T) {
	st, hub, ts := testServer(t)
	alice, err := st.CreateUser("alice", "secret", "user")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	dialTestDevice(t, ts, helloFrame("dev-1", "alice", "secret", nil), func(f tunnelFrame) map[string]any {
		return map[string]any{"ok": false, "error": "method_not_allowed"}
	})

	_, err = hub.Call(context.Background(), "dev-1", alice.ID, "ws-demo", "sessions.delete", nil, time.Second)
	if !errors.Is(err, ErrMethodNotAllowed) {
		t.Fatalf("err = %v, want ErrMethodNotAllowed", err)
	}

	// Unknown device must surface as offline, not as an RPC error.
	_, err = hub.Call(context.Background(), "missing", alice.ID, "ws-demo", "sessions.list", nil, time.Second)
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("err = %v, want ErrDeviceOffline", err)
	}
}

func TestTunnelRejectsBadToken(t *testing.T) {
	_, _, ts := testServer(t)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/tunnel"
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer func() { _ = ws.Close() }()

	hello := map[string]any{
		"type":   "hello",
		"token":  "deadbeef",
		"device": map[string]any{"id": "dev-x"},
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteJSON(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	var ack map[string]any
	if err := ws.ReadJSON(&ack); err != nil {
		t.Fatalf("read hello-ok: %v", err)
	}
	if ack["ok"] != false || ack["error"] != "unauthorized" {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func TestSSEFiltersByWorkspace(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	device, _ := dialTestDevice(t, ts, helloFrame("dev-1", "alice", "secret", []map[string]any{
		{"id": "ws-demo", "name": "Demo", "path": `E:\demo`},
		{"id": "ws-other", "name": "Other", "path": `E:\other`},
	}), func(f tunnelFrame) map[string]any {
		return map[string]any{"ok": true, "result": map[string]any{}}
	})

	client := newTestClient(t, ts, "alice", "secret")

	resp := doRequest(t, client, http.MethodPost, ts.URL+"/api/context", map[string]string{"device_id": "dev-1", "workspace_id": "ws-demo"})
	expectStatus(t, resp, http.StatusOK)

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/events", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("events Content-Type = %q", ct)
	}

	lines := make(chan string, 32)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	waitFor := func(pred func(string) bool) (string, bool) {
		timeout := time.After(5 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					return "", false
				}
				if pred(line) {
					return line, true
				}
			case <-timeout:
				return "", false
			}
		}
	}

	if _, ok := waitFor(func(s string) bool { return strings.HasPrefix(s, ": connected") }); !ok {
		t.Fatalf("did not receive SSE connect line")
	}

	// Event for a workspace this H5 session is NOT bound to must be dropped.
	device.send(map[string]any{
		"type":         "message.delta",
		"workspace_id": "ws-other",
		"data":         map[string]any{"text": "leaked"},
	})
	// Bound workspace event must be forwarded.
	device.send(map[string]any{
		"type":         "message.delta",
		"workspace_id": "ws-demo",
		"data":         map[string]any{"text": "hello-from-demo"},
	})

	line, ok := waitFor(func(s string) bool { return strings.HasPrefix(s, "data: ") })
	if !ok {
		t.Fatalf("did not receive any SSE data frame")
	}
	if !strings.Contains(line, "hello-from-demo") {
		t.Fatalf("first data frame = %q, want the bound-workspace event", line)
	}
	if !strings.Contains(line, `"type":"message.delta"`) {
		t.Fatalf("event frame missing type: %q", line)
	}
}

func TestSSERequiresContext(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	client := newTestClient(t, ts, "alice", "secret")
	resp := doRequest(t, client, http.MethodGet, ts.URL+"/api/events", nil)
	expectStatus(t, resp, http.StatusBadRequest)
}
