package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newWSServer 起一个假 Remote Server（/tunnel），把收到的 hello 帧送入 hellos。
func newWSServer(t *testing.T, onHello func(conn *websocket.Conn, hello map[string]any)) (string, <-chan map[string]any) {
	t.Helper()
	hellos := make(chan map[string]any, 16)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var f map[string]any
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			switch f["type"] {
			case "hello":
				hellos <- f
				onHello(conn, f)
			case "ping":
				_ = conn.WriteJSON(map[string]any{"type": "pong"})
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/tunnel", hellos
}

func fastClient(t *testing.T, url string) *client {
	t.Helper()
	c := newTestClient(t, newFakeHost(), map[string]string{
		KeyServerURL: url, KeyUsername: "u", KeyPassword: "p",
	})
	c.handshakeTimeout = 3 * time.Second
	c.readTimeout = 3 * time.Second
	c.heartbeatEvery = 100 * time.Millisecond
	c.backoffMin = 20 * time.Millisecond
	c.backoffMax = 50 * time.Millisecond
	return c
}

func TestConnectHelloTokenAndReconnect(t *testing.T) {
	url, hellos := newWSServer(t, func(conn *websocket.Conn, _ map[string]any) {
		_ = conn.WriteJSON(map[string]any{"type": "hello-ok", "device_id": "dev-1", "token": "tok-1"})
	})
	c := fastClient(t, url)
	c.ws.Register("i1", filepath.Join(t.TempDir(), "proj"), "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	first := <-hellos
	if first["username"] != "u" || first["password"] != "p" {
		t.Fatalf("首连应带 username/password：%#v", first)
	}
	if _, ok := first["token"]; ok {
		t.Fatal("首连不应带 token")
	}
	if wsList, _ := first["workspaces"].([]any); len(wsList) != 1 {
		t.Fatalf("hello 应带 1 个工作区：%#v", first["workspaces"])
	}
	if first["client_version"] != "1.0.0" || first["protocol"] != protocolVersion {
		t.Fatalf("hello 版本 / 协议不符：%#v", first)
	}

	waitFor(t, 3*time.Second, func() bool { return c.dev.Token() == "tok-1" })

	// 配置变更 → 断开并以 token 重连。
	c.NotifyChange()
	second := <-hellos
	if second["token"] != "tok-1" {
		t.Fatalf("重连应带 token：%#v", second)
	}
	if second["username"] != "u" {
		t.Fatalf("重连应附 username 作提示：%#v", second)
	}
}

func TestTokenInvalidFallsBackToPassword(t *testing.T) {
	url, hellos := newWSServer(t, func(conn *websocket.Conn, hello map[string]any) {
		if _, hasToken := hello["token"]; hasToken {
			_ = conn.WriteJSON(map[string]any{"ok": false, "error": "unauthorized"})
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "hello-ok", "device_id": "dev-1", "token": "tok-2"})
	})
	c := fastClient(t, url)
	if err := c.dev.SetToken("stale"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	first := <-hellos
	if first["token"] != "stale" {
		t.Fatalf("应先用已存 token 连接：%#v", first)
	}
	second := <-hellos
	if _, hasToken := second["token"]; hasToken {
		t.Fatalf("token 失效后应改用账号密码：%#v", second)
	}
	if second["username"] != "u" || second["password"] != "p" {
		t.Fatalf("回退连接应带账号密码：%#v", second)
	}
	waitFor(t, 3*time.Second, func() bool { return c.dev.Token() == "tok-2" })
}

func TestUnconfiguredReportsStatus(t *testing.T) {
	h := newFakeHost()
	c := newTestClient(t, h, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c.run(ctx)

	c.statusMu.Lock()
	state := c.state
	c.statusMu.Unlock()
	if state != stateUnconfigured {
		t.Fatalf("未配置时应为 unconfigured，得到 %q", state)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.uiEvents) == 0 || h.uiEvents[0]["kind"] != "remote-status" {
		t.Fatalf("应推送状态栏事件：%#v", h.uiEvents)
	}
}
