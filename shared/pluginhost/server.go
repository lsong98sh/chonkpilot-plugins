// Package pluginhost 是 browser / desktop 常驻插件共用的 UDS 宿主：
// 会话池 + 三端点（start / stop / operation）+ 空闲回收 + 健康检查。
//
// 插件差异只在 Engine 实现（WebView2 / Win32），宿主逻辑完全同构。
//
// 设计要点（GATEWAY-2 / L9-L10）：
//   - 一个进程承载多个会话；会话由 start 创建、stop 结束；
//   - operation 只做原子动作，不做编排（编排留给脚本）；
//   - 空闲超过 IdleTimeout（默认 10min）的会话自动回收，只关会话不杀进程；
//   - 不注册为 MCP 工具、不接 gateway —— 由脚本经 UDS 直连。
package pluginhost

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginproto"
)

// ContextOptions 是 start 的参数。
type ContextOptions struct {
	Purpose     string         `json:"purpose"`
	Profile     string         `json:"profile"`
	Headless    bool           `json:"headless"`
	Debug       bool           `json:"debug"`
	ConsoleSink string         `json:"consoleSink"`
	Width       int            `json:"width"`
	Height      int            `json:"height"`
	URL         string         `json:"url"`
	// Meta 是插件私有参数（browser 未用；desktop 用它传 title/class/hwnd 绑定目标窗口）。
	Meta map[string]any `json:"meta,omitempty"`
}

// Engine 是一个浏览器会话（WebView2 实现见 engine_webview.go）。
type Engine interface {
	Do(action string, args map[string]any) (map[string]any, error)
	Close() error
}

// Options 是服务端配置。
type Options struct {
	SocketPath  string
	IdleTimeout time.Duration
	MaxContexts int
	// JanitorInterval 是空闲扫描周期（缺省 1 分钟；测试可调小）。
	JanitorInterval time.Duration
	Logf            func(format string, args ...any)
	NewEngine       func(ContextOptions) (Engine, error)
}

// entry 是池中的一个 context。
type entry struct {
	id       string
	purpose  string
	engine   Engine
	mu       sync.Mutex // 串行化同一 context 上的 operation
	lastUsed time.Time
}

// Server 是 UDS 服务端。
type Server struct {
	opts Options

	ln net.Listener

	mu      sync.Mutex
	entries map[string]*entry
	seq     int
	closed  bool
	started time.Time

	closeCh chan struct{}
	wg      sync.WaitGroup
}

// New 构造服务端（未监听）。
func New(opts Options) *Server {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	if opts.MaxContexts <= 0 {
		opts.MaxContexts = 8
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &Server{
		opts:    opts,
		entries: map[string]*entry{},
		closeCh: make(chan struct{}),
		started: time.Now(),
	}
}

// Start 开始监听（非阻塞）。
func (s *Server) Start() error {
	path := s.opts.SocketPath
	if path == "" {
		path = DefaultSocketPath()
	}
	if len(path) > 100 {
		// Windows AF_UNIX 路径上限约 108 字节；超限换短路径，避免 listen 失败。
		path = filepath.Join(os.TempDir(), "chonkpilot-browser.sock")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// 陈旧 socket：能拨通则说明已有活实例，拒绝重复启动；否则删掉重建。
	if c, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		_ = c.Close()
		return fmt.Errorf("another browser plugin is already listening on %s", path)
	}
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", path, err)
	}
	s.ln = ln
	s.opts.SocketPath = path
	s.opts.Logf("browser plugin listening on %s", path)

	s.wg.Add(2)
	go s.acceptLoop()
	go s.janitor()
	return nil
}

// SocketPath 返回实际监听路径。
func (s *Server) SocketPath() string { return s.opts.SocketPath }

// Close 关闭服务并释放全部 context。
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	entries := s.entries
	s.entries = map[string]*entry{}
	s.mu.Unlock()

	close(s.closeCh)
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for _, e := range entries {
		_ = e.engine.Close()
	}
	s.wg.Wait()
	if s.opts.SocketPath != "" {
		_ = os.Remove(s.opts.SocketPath)
	}
	return nil
}

// acceptLoop 接收连接。
func (s *Server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closeCh:
				return
			default:
				s.opts.Logf("accept: %v", err)
				return
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serve(conn)
		}()
	}
}

// serve 处理一条连接上的多帧请求。
func (s *Server) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReaderSize(conn, 64<<10)
	for {
		req, err := pluginproto.ReadRequest(r)
		if err != nil {
			if err.Error() != "EOF" {
				_ = pluginproto.WriteResponse(conn, pluginproto.Fail("", pluginproto.CodeBadRequest, err.Error()))
			}
			return
		}
		resp := s.dispatch(req)
		if err := pluginproto.WriteResponse(conn, resp); err != nil {
			return
		}
		if req.Endpoint == pluginproto.EndpointShutdown {
			go func() { _ = s.Close(); os.Exit(0) }()
			return
		}
	}
}

// dispatch 按端点分发。
func (s *Server) dispatch(req *pluginproto.Request) *pluginproto.Response {
	switch req.Endpoint {
	case pluginproto.EndpointStart:
		return s.handleStart(req)
	case pluginproto.EndpointStop:
		return s.handleStop(req)
	case pluginproto.EndpointOperation:
		return s.handleOperation(req)
	case pluginproto.EndpointPing:
		return s.handlePing(req)
	case pluginproto.EndpointShutdown:
		return pluginproto.OK(req.ID, map[string]any{"closing": true})
	default:
		return pluginproto.Fail(req.ID, pluginproto.CodeUnknownEndpoint, "unknown endpoint: "+req.Endpoint)
	}
}

// handleStart 建立一个 context。
func (s *Server) handleStart(req *pluginproto.Request) *pluginproto.Response {
	var opts ContextOptions
	if err := pluginproto.DecodeParams(req.Params, &opts); err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeBadRequest, err.Error())
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return pluginproto.Fail(req.ID, pluginproto.CodeInternal, "server closing")
	}
	if len(s.entries) >= s.opts.MaxContexts {
		s.mu.Unlock()
		return pluginproto.Fail(req.ID, pluginproto.CodeTooManyContexts,
			fmt.Sprintf("max contexts reached (%d); stop an idle context first", s.opts.MaxContexts))
	}
	s.seq++
	id := fmt.Sprintf("ctx-%d", s.seq)
	s.mu.Unlock()

	engine, err := s.opts.NewEngine(opts)
	if err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeStartFailed, err.Error())
	}
	e := &entry{id: id, purpose: opts.Purpose, engine: engine, lastUsed: time.Now()}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = engine.Close()
		return pluginproto.Fail(req.ID, pluginproto.CodeInternal, "server closing")
	}
	s.entries[id] = e
	s.mu.Unlock()

	s.opts.Logf("context %s started (purpose=%q)", id, opts.Purpose)
	return pluginproto.OK(req.ID, map[string]any{
		"context_id": id,
		"engine":     "webview2",
		"purpose":    opts.Purpose,
	})
}

// handleStop 结束一个 context。
func (s *Server) handleStop(req *pluginproto.Request) *pluginproto.Response {
	var p struct {
		ContextID string `json:"context_id"`
	}
	if err := pluginproto.DecodeParams(req.Params, &p); err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeBadRequest, err.Error())
	}
	if p.ContextID == "" {
		return pluginproto.Fail(req.ID, pluginproto.CodeBadRequest, "context_id is required")
	}
	s.mu.Lock()
	e, ok := s.entries[p.ContextID]
	if ok {
		delete(s.entries, p.ContextID)
	}
	s.mu.Unlock()
	if !ok {
		return pluginproto.Fail(req.ID, pluginproto.CodeUnknownContext, "unknown context: "+p.ContextID)
	}
	if err := e.engine.Close(); err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeOperationFailed, err.Error())
	}
	s.opts.Logf("context %s stopped", p.ContextID)
	return pluginproto.OK(req.ID, map[string]any{"closed": true, "context_id": p.ContextID})
}

// handleOperation 执行一个原子动作。
func (s *Server) handleOperation(req *pluginproto.Request) *pluginproto.Response {
	var p struct {
		ContextID string         `json:"context_id"`
		Action    string         `json:"action"`
		Args      map[string]any `json:"args"`
	}
	if err := pluginproto.DecodeParams(req.Params, &p); err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeBadRequest, err.Error())
	}
	if p.ContextID == "" || p.Action == "" {
		return pluginproto.Fail(req.ID, pluginproto.CodeBadRequest, "context_id and action are required")
	}
	s.mu.Lock()
	e, ok := s.entries[p.ContextID]
	s.mu.Unlock()
	if !ok {
		return pluginproto.Fail(req.ID, pluginproto.CodeUnknownContext,
			"unknown or expired context: "+p.ContextID)
	}

	e.mu.Lock()
	e.lastUsed = time.Now()
	result, err := e.engine.Do(p.Action, p.Args)
	e.lastUsed = time.Now()
	e.mu.Unlock()

	if err != nil {
		return pluginproto.Fail(req.ID, pluginproto.CodeOperationFailed, err.Error())
	}
	if result == nil {
		result = map[string]any{}
	}
	result["context_id"] = p.ContextID
	return pluginproto.OK(req.ID, result)
}

// handlePing 返回健康信息。
func (s *Server) handlePing(req *pluginproto.Request) *pluginproto.Response {
	s.mu.Lock()
	n := len(s.entries)
	s.mu.Unlock()
	return pluginproto.OK(req.ID, map[string]any{
		"ok":       true,
		"contexts": n,
		"uptime_s": int(time.Since(s.started).Seconds()),
		"idle_s":   int(s.opts.IdleTimeout.Seconds()),
		"socket":   s.opts.SocketPath,
	})
}

// janitor 回收空闲超过 IdleTimeout 的 context（只关 context，不杀进程）。
func (s *Server) janitor() {
	defer s.wg.Done()
	interval := s.opts.JanitorInterval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.closeCh:
			return
		case <-ticker.C:
			now := time.Now()
			var expired []*entry
			s.mu.Lock()
			for id, e := range s.entries {
				if now.Sub(e.lastUsed) < s.opts.IdleTimeout {
					continue
				}
				// TryLock 成功 = 当前没有 operation 在跑，可安全回收。
				if e.mu.TryLock() {
					e.mu.Unlock()
					delete(s.entries, id)
					expired = append(expired, e)
				}
			}
			s.mu.Unlock()
			for _, e := range expired {
				_ = e.engine.Close()
				s.opts.Logf("context %s reclaimed after %s idle", e.id, s.opts.IdleTimeout)
			}
		}
	}
}

// DefaultSocketPath 返回默认 UDS 路径。
func DefaultSocketPath() string {
	if p := os.Getenv("CHONK_BROWSER_SOCK"); p != "" {
		return p
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "chonkpilot", "plugins", "browser.sock")
}

// NormalizeAction 归一化动作名（大小写/连字符）。
func NormalizeAction(a string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(a), "-", "_"))
}
