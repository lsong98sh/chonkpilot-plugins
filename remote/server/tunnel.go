package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Workspace is a project window reported by a desktop device. It only lives in
// server memory for the lifetime of the tunnel connection.
type Workspace struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	InstanceID string `json:"instance_id,omitempty"`
}

var (
	// ErrDeviceOffline means no live tunnel exists for the device.
	ErrDeviceOffline = errors.New("device_offline")
	// ErrMethodNotAllowed means the RPC is outside the frozen whitelist.
	ErrMethodNotAllowed = errors.New("method_not_allowed")
	// ErrForbidden means the device belongs to another account.
	ErrForbidden = errors.New("forbidden")
	// ErrCallTimeout means the device did not answer in time.
	ErrCallTimeout = errors.New("call_timeout")
)

// DeviceError wraps an error reported by the device for one RPC.
type DeviceError struct {
	Message string
}

func (e *DeviceError) Error() string {
	if e.Message == "" {
		return "device error"
	}
	return e.Message
}

// allowedMethods is the frozen RPC whitelist from spec 26 §4.4.
var allowedMethods = map[string]bool{
	"sessions.list":           true,
	"sessions.create":         true,
	"sessions.history":        true,
	"sessions.send":           true,
	"sessions.inbox":          true,
	"sessions.inbox.promote":  true,
	"sessions.inbox.revert":   true,
	"sessions.inbox.discard":  true,
	"sessions.stop":           true,
	"ask.answer":              true,
	"tasks.tree":              true,
	"tasks.tasks":             true,
	"tasks.stop":              true,
	"files.list":              true,
	"files.read":              true,
}

func isAllowedMethod(method string) bool { return allowedMethods[method] }

// rpcResult is the payload of a `result` frame.
type rpcResult struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// tunnelFrame covers every frame type carried by the /tunnel WebSocket.
type tunnelFrame struct {
	Type string `json:"type"`

	// hello
	Username      string       `json:"username,omitempty"`
	Password      string       `json:"password,omitempty"`
	Token         string       `json:"token,omitempty"`
	Device        *helloDevice `json:"device,omitempty"`
	ClientVersion string       `json:"client_version,omitempty"`
	Protocol      string       `json:"protocol,omitempty"`
	Workspaces    []Workspace  `json:"workspaces,omitempty"`

	// call / result
	ID     string          `json:"id,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Method string          `json:"method,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`

	// event
	WorkspaceID string          `json:"workspace_id,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
}

type helloDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Platform string `json:"platform"`
	Version  string `json:"version"`
}

// deviceConn is one live tunnel for one device.
type deviceConn struct {
	deviceID string
	userID   string
	protocol string
	ws       *websocket.Conn

	send chan []byte

	pendingMu sync.Mutex
	pending   map[string]chan rpcResult

	wsMu       sync.RWMutex
	workspaces []Workspace

	done      chan struct{}
	closeOnce sync.Once
	closed    atomic.Bool
}

func (c *deviceConn) setWorkspaces(ws []Workspace) {
	copyWS := make([]Workspace, len(ws))
	copy(copyWS, ws)
	c.wsMu.Lock()
	c.workspaces = copyWS
	c.wsMu.Unlock()
}

func (c *deviceConn) getWorkspaces() []Workspace {
	c.wsMu.RLock()
	defer c.wsMu.RUnlock()
	out := make([]Workspace, len(c.workspaces))
	copy(out, c.workspaces)
	return out
}

// enqueue queues an outbound frame, dropping it if the device cannot keep up.
func (c *deviceConn) enqueue(payload []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.send <- payload:
	case <-c.done:
	default:
	}
}

func (c *deviceConn) close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.done)
		_ = c.ws.Close()
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			delete(c.pending, id)
			select {
			case ch <- rpcResult{OK: false, Error: "connection closed"}:
			default:
			}
		}
		c.pendingMu.Unlock()
	})
}

func (c *deviceConn) deliver(f tunnelFrame) {
	c.pendingMu.Lock()
	ch := c.pending[f.ID]
	if ch != nil {
		delete(c.pending, f.ID)
	}
	c.pendingMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- rpcResult{OK: f.OK, Result: f.Result, Error: f.Error}:
	default:
	}
}

// subscriber is one SSE client bound to a user + device + workspace.
type subscriber struct {
	id          int64
	userID      string
	deviceID    string
	workspaceID string
	ch          chan []byte
}

// Hub tracks live device tunnels and SSE subscribers.
type Hub struct {
	mu   sync.RWMutex
	conn map[string]*deviceConn
	subs map[int64]*subscriber
	next atomic.Int64
	log  *slog.Logger
}

func NewHub(logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		conn: map[string]*deviceConn{},
		subs: map[int64]*subscriber{},
		log:  logger,
	}
}

func (h *Hub) register(c *deviceConn) {
	h.mu.Lock()
	old := h.conn[c.deviceID]
	h.conn[c.deviceID] = c
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (h *Hub) unregister(c *deviceConn) {
	h.mu.Lock()
	if h.conn[c.deviceID] == c {
		delete(h.conn, c.deviceID)
	}
	h.mu.Unlock()
}

// Status reports whether a device is online, its workspaces and protocol.
func (h *Hub) Status(deviceID string) (bool, []Workspace, string) {
	h.mu.RLock()
	c := h.conn[deviceID]
	h.mu.RUnlock()
	if c == nil {
		return false, nil, ""
	}
	return true, c.getWorkspaces(), c.protocol
}

// Call forwards one whitelisted RPC to the device and waits for its result.
func (h *Hub) Call(ctx context.Context, deviceID, userID, workspaceID, method string, args any, timeout time.Duration) (json.RawMessage, error) {
	if !isAllowedMethod(method) {
		return nil, ErrMethodNotAllowed
	}
	h.mu.RLock()
	c := h.conn[deviceID]
	h.mu.RUnlock()
	if c == nil {
		return nil, ErrDeviceOffline
	}
	if c.userID != userID {
		return nil, ErrForbidden
	}

	id, err := randomHex(8)
	if err != nil {
		return nil, err
	}
	frame := map[string]any{
		"type":         "call",
		"id":           id,
		"workspace_id": workspaceID,
		"method":       method,
	}
	if args == nil {
		frame["args"] = map[string]any{}
	} else {
		frame["args"] = args
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}

	ch := make(chan rpcResult, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	select {
	case c.send <- raw:
	case <-c.done:
		return nil, ErrDeviceOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if !res.OK {
			return nil, &DeviceError{Message: res.Error}
		}
		return res.Result, nil
	case <-timer.C:
		return nil, ErrCallTimeout
	case <-c.done:
		return nil, ErrDeviceOffline
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Subscribe registers an SSE client and returns its id + delivery channel.
func (h *Hub) Subscribe(userID, deviceID, workspaceID string) (int64, <-chan []byte) {
	id := h.next.Add(1)
	sub := &subscriber{
		id:          id,
		userID:      userID,
		deviceID:    deviceID,
		workspaceID: workspaceID,
		ch:          make(chan []byte, 64),
	}
	h.mu.Lock()
	h.subs[id] = sub
	h.mu.Unlock()
	return id, sub.ch
}

func (h *Hub) Unsubscribe(id int64) {
	h.mu.Lock()
	delete(h.subs, id)
	h.mu.Unlock()
}

func (h *Hub) broadcast(c *deviceConn, workspaceID string, payload []byte) {
	h.mu.RLock()
	targets := make([]*subscriber, 0, len(h.subs))
	for _, sub := range h.subs {
		if sub.userID == c.userID && sub.deviceID == c.deviceID && sub.workspaceID == workspaceID {
			targets = append(targets, sub)
		}
	}
	h.mu.RUnlock()
	for _, sub := range targets {
		select {
		case sub.ch <- payload:
		default:
			h.log.Warn("sse subscriber queue full, dropping event", "subscriber", sub.id, "device", c.deviceID, "workspace", workspaceID)
		}
	}
}

// ---------------------------------------------------------------- websocket server side

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.log.Warn("tunnel upgrade failed", "err", err)
		return
	}
	ws.SetReadLimit(4 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))

	_, msg, err := ws.ReadMessage()
	if err != nil {
		_ = ws.Close()
		return
	}
	var hello tunnelFrame
	if err := json.Unmarshal(msg, &hello); err != nil || hello.Type != "hello" {
		writeWSJSON(ws, map[string]any{"type": "hello-ok", "ok": false, "error": "bad_hello"})
		_ = ws.Close()
		return
	}

	dev, token, failReason := s.authTunnelHello(&hello)
	if failReason != "" {
		writeWSJSON(ws, map[string]any{"type": "hello-ok", "ok": false, "error": failReason})
		_ = ws.Close()
		return
	}

	c := &deviceConn{
		deviceID: dev.ID,
		userID:   dev.UserID,
		protocol: hello.Protocol,
		ws:       ws,
		send:     make(chan []byte, 64),
		pending:  map[string]chan rpcResult{},
		done:     make(chan struct{}),
	}
	c.setWorkspaces(hello.Workspaces)
	s.hub.register(c)
	s.log.Info("tunnel connected",
		"device", dev.ID,
		"user", dev.UserID,
		"protocol", hello.Protocol,
		"client_version", hello.ClientVersion,
		"workspaces", len(hello.Workspaces),
	)

	if err := writeWSJSON(ws, map[string]any{
		"type":        "hello-ok",
		"ok":          true,
		"device_id":   dev.ID,
		"token":       token,
		"server_time": time.Now().Unix(),
	}); err != nil {
		c.close()
		return
	}

	go c.writeLoop()
	go c.readLoop(s.hub, s.st)
}

// authTunnelHello validates the hello frame. It returns the device, a freshly
// issued token, and a non-empty failure reason when authentication fails.
func (s *Server) authTunnelHello(f *tunnelFrame) (*Device, string, string) {
	if f.Device == nil || f.Device.ID == "" {
		return nil, "", "bad_hello"
	}
	deviceID := f.Device.ID
	var userID string

	if f.Token != "" {
		dev, err := s.st.GetDevice(deviceID)
		if err != nil {
			return nil, "", "unauthorized"
		}
		if dev.TokenHash == "" || !constantTimeEqual(dev.TokenHash, hashToken(f.Token)) {
			return nil, "", "unauthorized"
		}
		userID = dev.UserID
	} else {
		if f.Username == "" || f.Password == "" {
			return nil, "", "unauthorized"
		}
		user, err := s.st.GetUser(f.Username)
		if err != nil || user.Disabled || !bcryptCheck(user.PasswordHash, f.Password) {
			return nil, "", "unauthorized"
		}
		if existing, err := s.st.GetDevice(deviceID); err == nil && existing.UserID != user.ID {
			return nil, "", "unauthorized"
		}
		userID = user.ID
	}

	token, err := randomHex(32)
	if err != nil {
		return nil, "", "internal"
	}
	now := time.Now().Unix()
	dev := &Device{
		ID:        deviceID,
		UserID:    userID,
		Name:      f.Device.Name,
		Platform:  f.Device.Platform,
		Version:   f.Device.Version,
		TokenHash: hashToken(token),
		LastSeen:  now,
		CreatedAt: now,
	}
	if existing, err := s.st.GetDevice(deviceID); err == nil {
		dev.CreatedAt = existing.CreatedAt
	}
	if err := s.st.UpsertDevice(dev); err != nil {
		s.log.Error("persist device failed", "device", deviceID, "err", err)
		return nil, "", "internal"
	}
	return dev, token, ""
}

func (c *deviceConn) writeLoop() {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case payload := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, payload); err != nil {
				c.close()
				return
			}
		case <-ping.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
				c.close()
				return
			}
		}
	}
}

func (c *deviceConn) readLoop(h *Hub, st *Store) {
	var readErr error
	defer func() {
		h.unregister(c)
		c.close()
		_ = st.TouchDevice(c.deviceID, time.Now().Unix())
		h.log.Info("tunnel closed", "device", c.deviceID, "err", readErr)
	}()
	for {
		_ = c.ws.SetReadDeadline(time.Now().Add(60 * time.Second))
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			readErr = err
			return
		}
		if len(data) == 0 {
			continue
		}
		var f tunnelFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		switch f.Type {
		case "result":
			c.deliver(f)
		case "workspaces":
			c.setWorkspaces(f.Workspaces)
			h.log.Debug("device workspaces updated", "device", c.deviceID, "count", len(f.Workspaces))
		case "ping":
			c.enqueue([]byte(`{"type":"pong"}`))
		case "pong":
			// heartbeat only
		case "hello", "hello-ok":
			// ignore repeated handshakes
		default:
			c.handleEvent(h, f)
		}
	}
}

// handleEvent normalizes both accepted event shapes:
//
//	{"type":"message.delta","workspace_id":"ws","data":{...}}
//	{"type":"event","workspace_id":"ws","data":{"type":"message.delta","data":{...}}}
func (c *deviceConn) handleEvent(h *Hub, f tunnelFrame) {
	typ, data, ok := normalizeEvent(f)
	if !ok || f.WorkspaceID == "" {
		return
	}
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	payload, err := json.Marshal(map[string]any{"type": typ, "data": data})
	if err != nil {
		return
	}
	h.broadcast(c, f.WorkspaceID, payload)
}

func normalizeEvent(f tunnelFrame) (string, json.RawMessage, bool) {
	if f.Type != "" && f.Type != "event" {
		return f.Type, f.Data, true
	}
	if len(f.Data) > 0 {
		var nested tunnelFrame
		if err := json.Unmarshal(f.Data, &nested); err == nil && nested.Type != "" {
			return nested.Type, nested.Data, true
		}
	}
	return "", nil, false
}

func writeWSJSON(ws *websocket.Conn, v any) error {
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteJSON(v)
}
