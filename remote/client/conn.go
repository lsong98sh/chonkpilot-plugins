package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// 连接状态（状态栏 / 配置页共用）。
const (
	stateUnconfigured = "unconfigured"
	stateConnecting   = "connecting"
	stateConnected    = "connected"
	stateError        = "error"
)

// 连接默认参数。
const (
	defaultHandshakeTimeout = 10 * time.Second
	defaultReadTimeout      = 60 * time.Second
	defaultHeartbeat        = 20 * time.Second
	defaultBackoffMin       = time.Second
	defaultBackoffMax       = 30 * time.Second
	defaultWriteTimeout     = 10 * time.Second
)

var errNotConnected = errors.New("not_connected")

// host 是插件宿主能力面的最小依赖（生产 = pluginsdk.Plugin；测试 = 假实现）。
type host interface {
	Call(ctx context.Context, method string, args any) (json.RawMessage, error)
	ReportStatus(ctx context.Context, state, detail string) error
	PublishUI(ctx context.Context, instanceID string, payload any) error
}

// authError 表示 Server 明确的认证失败（unauthorized）。
type authError struct{ msg string }

func (e *authError) Error() string { return e.msg }

func isAuthError(err error) bool {
	var a *authError
	return errors.As(err, &a)
}

// client 是 Remote 插件的主体：出站连接 + 重连 + 心跳 + 状态上报。
type client struct {
	host    host
	log     *slog.Logger
	cfg     *configManager
	dev     *deviceStore
	ws      *workspaceStore
	running *runningSet
	asks    *askCache

	dialer           *websocket.Dialer
	handshakeTimeout time.Duration
	readTimeout      time.Duration
	heartbeatEvery   time.Duration
	backoffMin       time.Duration
	backoffMax       time.Duration
	jitter           func(time.Duration) time.Duration

	mu       sync.Mutex
	conn     *websocket.Conn
	changeCh chan struct{}
	reset    atomic.Bool
	writeMu  sync.Mutex

	dialogMu sync.Mutex
	dialogs  map[string]string

	statusMu sync.Mutex
	state    string
	detail   string
}

func newClient(h host, log *slog.Logger, cfg *configManager, dev *deviceStore, ws *workspaceStore, run *runningSet, asks *askCache) *client {
	return &client{
		host:             h,
		log:              log,
		cfg:              cfg,
		dev:              dev,
		ws:               ws,
		running:          run,
		asks:             asks,
		dialer:           &websocket.Dialer{HandshakeTimeout: defaultHandshakeTimeout},
		handshakeTimeout: defaultHandshakeTimeout,
		readTimeout:      defaultReadTimeout,
		heartbeatEvery:   defaultHeartbeat,
		backoffMin:       defaultBackoffMin,
		backoffMax:       defaultBackoffMax,
		jitter:           defaultJitter,
		changeCh:         make(chan struct{}),
		state:            stateUnconfigured,
		dialogs:          map[string]string{},
	}
}

// defaultJitter 返回 [d/2, d] 的抖动（避免多台设备同时重连）。
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// Changes 返回「配置变更」广播通道（每次变更关闭旧通道并换新）。
func (c *client) Changes() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changeCh
}

// NotifyChange 通知配置变更：唤醒重试等待并断开当前连接（立即用新配置重连）。
func (c *client) NotifyChange() {
	c.reset.Store(true)
	c.mu.Lock()
	close(c.changeCh)
	c.changeCh = make(chan struct{})
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *client) takeReset() bool { return c.reset.Swap(false) }

func (c *client) waitChange(ctx context.Context) bool {
	ch := c.Changes()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	}
}

// waitChangeTimeout 等配置变更或超时（用于未配置时的周期性重读）。
func (c *client) waitChangeTimeout(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	ch := c.Changes()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	case <-t.C:
		return true
	}
}

func (c *client) waitBackoff(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(c.jitter(d))
	defer t.Stop()
	ch := c.Changes()
	select {
	case <-ctx.Done():
		return false
	case <-ch:
		return true
	case <-t.C:
		return true
	}
}

func (c *client) nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next <= 0 || next > c.backoffMax {
		return c.backoffMax
	}
	return next
}

func (c *client) currentConn() *websocket.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn
}

func (c *client) setConn(conn *websocket.Conn) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
}

func (c *client) clearConn(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
}

// Snapshot 返回状态快照（配置页 / 徽标共用）。
func (c *client) Snapshot() map[string]any {
	c.statusMu.Lock()
	state, detail := c.state, c.detail
	c.statusMu.Unlock()
	cfg := c.cfg.Settings()
	return map[string]any{
		"ok":          true,
		"state":       state,
		"detail":      detail,
		"connected":   state == stateConnected,
		"configured":  cfg.Configured(),
		"serverUrl":   cfg.ServerURL,
		"username":    cfg.Username,
		"hasPassword": cfg.Password != "",
		"workspaces":  c.ws.List(),
	}
}

// isConnected 报告当前是否处于已连接状态。
func (c *client) isConnected() bool {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.state == stateConnected
}

// setStatus 更新状态并（变化时）上报宿主 + 推送状态栏。
func (c *client) setStatus(state, detail string) {
	c.statusMu.Lock()
	changed := c.state != state || c.detail != detail
	c.state, c.detail = state, detail
	c.statusMu.Unlock()
	if !changed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 宿主按 running/degraded 控制插件页可达性；连接状态放 detail，避免未配置时配置页被锁死。
	if err := c.host.ReportStatus(ctx, "running", detail); err != nil && c.log != nil {
		c.log.Debug("remote: 上报状态失败", "err", err)
	}
	cfg := c.cfg.Settings()
	payload := map[string]any{
		"kind":       "remote-status",
		"state":      state,
		"detail":     detail,
		"connected":  state == stateConnected,
		"configured": cfg.Configured(),
	}
	if err := c.host.PublishUI(ctx, "", payload); err != nil && c.log != nil {
		c.log.Debug("remote: 推送状态栏失败", "err", err)
	}
}

// run 是常驻连接循环：未配置等待 → 连接 → 断线指数退避重连（配置变更立即重连）。
func (c *client) run(ctx context.Context) {
	backoff := c.backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		cfg := c.cfg.Settings()
		if !cfg.Configured() {
			// 启动竞态：数据面可能晚于插件就绪；重试读取配置，而不是永久停在未配置。
			if err := c.cfg.Reload(ctx); err == nil {
				cfg = c.cfg.Settings()
			}
		}
		if !cfg.Configured() {
			c.setStatus(stateUnconfigured, "未配置服务器地址 / 账号")
			if !c.waitChangeTimeout(ctx, 5*time.Second) {
				return
			}
			continue
		}
		dev := c.dev.Ensure()
		usedToken := c.dev.Token() != ""
		out := c.connectOnce(ctx, cfg, dev, usedToken)
		if ctx.Err() != nil {
			return
		}
		if out.authFailed {
			if usedToken {
				// token 失效 → 清 token，用账号密码重连一次。
				if err := c.dev.ClearToken(); err != nil && c.log != nil {
					c.log.Warn("remote: 清除失效 token 失败", "err", err)
				}
				if c.log != nil {
					c.log.Info("remote: token 失效，改用账号密码重连")
				}
				out = c.connectOnce(ctx, cfg, dev, false)
				if ctx.Err() != nil {
					return
				}
			}
			if out.authFailed {
				c.setStatus(stateError, "认证失败，请检查用户名 / 密码")
				if !c.waitChange(ctx) {
					return
				}
				continue
			}
		}
		if out.connected {
			backoff = c.backoffMin
		}
		if c.takeReset() {
			backoff = c.backoffMin
			continue
		}
		c.setStatus(stateConnecting, "连接中断，正在重连")
		if out.err != nil {
			c.setStatus(stateConnecting, "连接中断："+out.err.Error())
		}
		if !c.waitBackoff(ctx, backoff) {
			return
		}
		if c.takeReset() {
			backoff = c.backoffMin
			continue
		}
		backoff = c.nextBackoff(backoff)
	}
}

// outcome 是一次连接尝试的结果。
type outcome struct {
	connected  bool
	authFailed bool
	err        error
}

// connectOnce 建连 + 握手 + 读循环（阻塞到断开）。
func (c *client) connectOnce(ctx context.Context, cfg settings, dev deviceDesc, useToken bool) outcome {
	c.setStatus(stateConnecting, "正在连接 "+cfg.ServerURL)
	dialCtx, cancel := context.WithTimeout(ctx, c.handshakeTimeout)
	conn, resp, err := c.dialer.DialContext(dialCtx, cfg.ServerURL, nil)
	cancel()
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if c.log != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			c.log.Warn("remote: 连接失败", "url", cfg.ServerURL, "http_status", status, "err", err)
		}
		return outcome{}
	}
	c.setConn(conn)
	defer c.clearConn(conn)
	defer conn.Close()
	// ctx 取消（插件退出）时立即断开，避免读循环阻塞到读超时。
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopWatch:
		}
	}()

	if err := c.handshake(ctx, conn, cfg, dev, useToken); err != nil {
		if ctx.Err() != nil {
			return outcome{}
		}
		if isAuthError(err) {
			return outcome{authFailed: true}
		}
		if c.log != nil {
			c.log.Warn("remote: 握手失败", "url", cfg.ServerURL, "err", err)
		}
		return outcome{}
	}
	c.setStatus(stateConnected, "已连接 "+hostOf(cfg.ServerURL))
	if c.log != nil {
		c.log.Info("remote: 已连接", "url", cfg.ServerURL, "device", dev.ID)
	}
	// hello 后补发一次全量工作区（覆盖「握手读列表」与「实例重放」之间的竞态）。
	c.sendWorkspaces()

	done := make(chan struct{})
	go c.heartbeat(ctx, conn, done)
	authFailed, readErr := c.readLoop(ctx, conn)
	close(done)
	return outcome{connected: true, authFailed: authFailed, err: readErr}
}

// handshake 发送 hello 并等待 hello-ok（token 落盘）。
func (c *client) handshake(ctx context.Context, conn *websocket.Conn, cfg settings, dev deviceDesc, useToken bool) error {
	token := ""
	if useToken {
		token = c.dev.Token()
	}
	if err := c.write(conn, helloFrame(cfg, dev, token, c.ws.List())); err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout))
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var f wsFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			if c.log != nil {
				c.log.Debug("remote: 丢弃无法解析的帧", "err", err)
			}
			continue
		}
		if f.Type == frameHelloOK {
			if f.Token != "" {
				if err := c.dev.SetToken(f.Token); err != nil && c.log != nil {
					c.log.Warn("remote: 保存 token 失败", "err", err)
				}
			}
			return nil
		}
		if isUnauthorized(f) {
			return &authError{msg: "unauthorized"}
		}
	}
}

// readLoop 读循环：处理 ping / call / 心跳超时 / 认证失效。
func (c *client) readLoop(ctx context.Context, conn *websocket.Conn) (authFailed bool, err error) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil && c.log != nil {
				c.log.Info("remote: 连接断开", "err", err)
			}
			return false, err
		}
		var f wsFrame
		if err := json.Unmarshal(raw, &f); err != nil {
			if c.log != nil {
				c.log.Debug("remote: 丢弃无法解析的帧", "err", err)
			}
			continue
		}
		switch f.Type {
		case framePing:
			_ = c.write(conn, pongFrame())
		case framePong, frameHelloOK:
		case frameCall:
			go c.handleCall(ctx, conn, f)
		default:
			if isUnauthorized(f) {
				if err := c.dev.ClearToken(); err != nil && c.log != nil {
					c.log.Warn("remote: 清除失效 token 失败", "err", err)
				}
				return true, nil
			}
		}
	}
}

// heartbeat 每 20s 主动 ping（Server 不回 pong 由读超时兜底）。
func (c *client) heartbeat(ctx context.Context, conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(c.heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			if err := c.write(conn, pingFrame()); err != nil {
				return
			}
		}
	}
}

// write 串行化写入（gorilla 不允许并发写）。
func (c *client) write(conn *websocket.Conn, v any) error {
	if conn == nil {
		return errNotConnected
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(defaultWriteTimeout))
	return conn.WriteJSON(v)
}

// send 向当前连接写一帧（未连接 → errNotConnected）。
func (c *client) send(v any) error {
	conn := c.currentConn()
	if conn == nil {
		return errNotConnected
	}
	return c.write(conn, v)
}

// sendWorkspaces 全量覆盖上报工作区列表。
func (c *client) sendWorkspaces() {
	if !c.isConnected() {
		return
	}
	if err := c.send(workspacesFrame(c.ws.List())); err != nil && c.log != nil {
		c.log.Debug("remote: 上报工作区失败", "err", err)
	}
}

// handleTest 测试连接（不打断已建立的连接）。
func (c *client) handleTest(ctx context.Context, payload map[string]any) (any, error) {
	cur := c.cfg.Settings()
	cfg := cur
	changed := false
	if v := strings.TrimSpace(pluginsdk.Str(payload["serverUrl"])); v != "" {
		norm, err := normalizeServerURL(v)
		if err != nil {
			return nil, err
		}
		changed = changed || norm != cur.ServerURL
		cfg.ServerURL = norm
	}
	if v := strings.TrimSpace(pluginsdk.Str(payload["username"])); v != "" {
		changed = changed || v != cur.Username
		cfg.Username = v
	}
	if v := pluginsdk.Str(payload["password"]); v != "" {
		changed = changed || v != cur.Password
		cfg.Password = v
	}
	if !cfg.Configured() {
		return nil, errors.New("未配置：请先填写服务器地址、用户名、密码")
	}
	// 目标与当前生效配置一致且已连接 → 不重复握手（避免顶替在线连接）。
	if c.isConnected() && !changed {
		return map[string]any{"ok": true, "connected": true, "detail": "当前已连接"}, nil
	}
	if err := c.probe(ctx, cfg); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "connected": false, "detail": "握手成功（未保持连接）"}, nil
}

// probe 做一次一次性握手（用于「测试连接」）。
func (c *client) probe(ctx context.Context, cfg settings) error {
	dev := c.dev.Ensure()
	token := c.dev.Token()
	dialCtx, cancel := context.WithTimeout(ctx, c.handshakeTimeout)
	defer cancel()
	conn, resp, err := c.dialer.DialContext(dialCtx, cfg.ServerURL, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("连接失败：%v", err)
	}
	defer conn.Close()
	if err := c.handshake(ctx, conn, cfg, dev, token != ""); err != nil {
		if isAuthError(err) {
			return errors.New("认证失败：用户名或密码错误")
		}
		return fmt.Errorf("握手失败：%v", err)
	}
	return nil
}

// handleStatus 是配置页的「状态」动作。
func (c *client) handleStatus(_ context.Context, _ map[string]any) (any, error) {
	return c.Snapshot(), nil
}

// handleBadge 是状态栏徽标的取数动作。
func (c *client) handleBadge(_ context.Context, _ map[string]any) (any, error) {
	snap := c.Snapshot()
	return map[string]any{
		"ok":         true,
		"state":      snap["state"],
		"detail":     snap["detail"],
		"connected":  snap["connected"],
		"configured": snap["configured"],
	}, nil
}

// isUnauthorized 判定 Server 的认证失败帧（`{ok:false,error:"unauthorized"}`）。
func isUnauthorized(f wsFrame) bool {
	if !strings.EqualFold(strings.TrimSpace(f.Error), "unauthorized") {
		return false
	}
	return f.OK == nil || !*f.OK
}

// hostOf 取服务器地址的 host（日志 / 状态文案用）。
func hostOf(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" {
		return serverURL
	}
	return u.Host
}
