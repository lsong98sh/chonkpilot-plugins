package pluginhost

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginproto"
)

// 错误码别名（减少测试里的包名前缀噪声）。
const (
	CodeUnknownContext  = pluginproto.CodeUnknownContext
	CodeUnknownEndpoint = pluginproto.CodeUnknownEndpoint
	CodeBadRequest      = pluginproto.CodeBadRequest
	CodeTooManyContexts = pluginproto.CodeTooManyContexts
	CodeStartFailed     = pluginproto.CodeStartFailed
)

// fakeEngine 是可观测的假会话。
type fakeEngine struct {
	closed int32
	lastDo string
}

func (f *fakeEngine) Do(action string, args map[string]any) (map[string]any, error) {
	f.lastDo = action
	return map[string]any{"echo": action}, nil
}

func (f *fakeEngine) Close() error {
	atomic.AddInt32(&f.closed, 1)
	return nil
}

// call 发一帧请求并读一帧响应。
func call(t *testing.T, sock string, req map[string]any) map[string]any {
	t.Helper()
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))

	b, _ := json.Marshal(req)
	if _, err := c.Write(append(b, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("bad response: %v\n%s", err, line)
	}
	return resp
}

// newTestServer 起一个临时 UDS 服务。
func newTestServer(t *testing.T, opts Options) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "p.sock")
	opts.SocketPath = sock
	if opts.NewEngine == nil {
		opts.NewEngine = func(ContextOptions) (Engine, error) { return &fakeEngine{}, nil }
	}
	s := New(opts)
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, sock
}

func TestStartOperationStop(t *testing.T) {
	_, sock := newTestServer(t, Options{})

	r := call(t, sock, map[string]any{"id": "1", "endpoint": "start", "params": map[string]any{"purpose": "t"}})
	if r["ok"] != true {
		t.Fatalf("start failed: %v", r)
	}
	res, _ := r["result"].(map[string]any)
	ctxID, _ := res["context_id"].(string)
	if ctxID == "" {
		t.Fatalf("missing context_id: %v", r)
	}

	r = call(t, sock, map[string]any{
		"id": "2", "endpoint": "operation",
		"params": map[string]any{"context_id": ctxID, "action": "navigate", "args": map[string]any{"url": "x"}},
	})
	if r["ok"] != true {
		t.Fatalf("operation failed: %v", r)
	}
	res, _ = r["result"].(map[string]any)
	if res["echo"] != "navigate" {
		t.Fatalf("result=%v", res)
	}
	if res["context_id"] != ctxID {
		t.Fatalf("result missing context_id: %v", res)
	}

	r = call(t, sock, map[string]any{"id": "3", "endpoint": "stop", "params": map[string]any{"context_id": ctxID}})
	if r["ok"] != true {
		t.Fatalf("stop failed: %v", r)
	}

	// 停止后再 operation → unknown_context
	r = call(t, sock, map[string]any{
		"id": "4", "endpoint": "operation",
		"params": map[string]any{"context_id": ctxID, "action": "navigate"},
	})
	if r["ok"] != false {
		t.Fatalf("want failure after stop: %v", r)
	}
	e, _ := r["error"].(map[string]any)
	if e["code"] != CodeUnknownContext {
		t.Fatalf("code=%v", e["code"])
	}
}

func TestUnknownEndpointAndBadParams(t *testing.T) {
	_, sock := newTestServer(t, Options{})

	r := call(t, sock, map[string]any{"id": "1", "endpoint": "nope"})
	if r["ok"] != false {
		t.Fatalf("want failure: %v", r)
	}
	e, _ := r["error"].(map[string]any)
	if e["code"] != CodeUnknownEndpoint {
		t.Fatalf("code=%v", e["code"])
	}

	r = call(t, sock, map[string]any{"id": "2", "endpoint": "operation", "params": map[string]any{}})
	if r["ok"] != false {
		t.Fatalf("want failure for missing context_id/action: %v", r)
	}
	e, _ = r["error"].(map[string]any)
	if e["code"] != CodeBadRequest {
		t.Fatalf("code=%v", e["code"])
	}
}

func TestMaxContexts(t *testing.T) {
	_, sock := newTestServer(t, Options{MaxContexts: 1})

	if r := call(t, sock, map[string]any{"id": "1", "endpoint": "start", "params": map[string]any{}}); r["ok"] != true {
		t.Fatalf("first start failed: %v", r)
	}
	r := call(t, sock, map[string]any{"id": "2", "endpoint": "start", "params": map[string]any{}})
	if r["ok"] != false {
		t.Fatalf("want too_many_contexts: %v", r)
	}
	e, _ := r["error"].(map[string]any)
	if e["code"] != CodeTooManyContexts {
		t.Fatalf("code=%v", e["code"])
	}
}

func TestPing(t *testing.T) {
	_, sock := newTestServer(t, Options{})
	call(t, sock, map[string]any{"id": "1", "endpoint": "start", "params": map[string]any{}})

	r := call(t, sock, map[string]any{"id": "2", "endpoint": "ping"})
	if r["ok"] != true {
		t.Fatalf("ping failed: %v", r)
	}
	res, _ := r["result"].(map[string]any)
	if res["contexts"].(float64) != 1 {
		t.Fatalf("contexts=%v", res["contexts"])
	}
}

func TestIdleReclaim(t *testing.T) {
	fake := &fakeEngine{}
	s, sock := newTestServer(t, Options{
		IdleTimeout:     80 * time.Millisecond,
		JanitorInterval: 20 * time.Millisecond,
		NewEngine:       func(ContextOptions) (Engine, error) { return fake, nil },
	})

	r := call(t, sock, map[string]any{"id": "1", "endpoint": "start", "params": map[string]any{}})
	res, _ := r["result"].(map[string]any)
	ctxID, _ := res["context_id"].(string)

	// 等回收
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fake.closed) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&fake.closed) == 0 {
		t.Fatal("idle context was not reclaimed")
	}

	// 回收后 operation → unknown_context（语义与「未知」一致，避免静默重建）
	r = call(t, sock, map[string]any{
		"id": "2", "endpoint": "operation",
		"params": map[string]any{"context_id": ctxID, "action": "x"},
	})
	if r["ok"] != false {
		t.Fatalf("want failure after reclaim: %v", r)
	}
	_ = s
}

func TestStartEngineError(t *testing.T) {
	_, sock := newTestServer(t, Options{
		NewEngine: func(ContextOptions) (Engine, error) {
			return nil, errStart
		},
	})
	r := call(t, sock, map[string]any{"id": "1", "endpoint": "start", "params": map[string]any{}})
	if r["ok"] != false {
		t.Fatalf("want start_failed: %v", r)
	}
	e, _ := r["error"].(map[string]any)
	if e["code"] != CodeStartFailed {
		t.Fatalf("code=%v", e["code"])
	}
}

// errStart 是测试用启动错误。
var errStart = &startErr{}

type startErr struct{}

func (e *startErr) Error() string { return "engine start failed" }
