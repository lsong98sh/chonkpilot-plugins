package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// memConfigStore 是 usr 配置存储的内存桩。
type memConfigStore struct {
	mu     sync.Mutex
	values map[string]string
}

func (m *memConfigStore) Load(context.Context) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.values))
	for k, v := range m.values {
		out[k] = v
	}
	return out, nil
}

func (m *memConfigStore) Save(_ context.Context, values map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values = make(map[string]string, len(values))
	for k, v := range values {
		m.values[k] = v
	}
	return nil
}

func (m *memConfigStore) Delete(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values = map[string]string{}
	return nil
}

type callRecord struct {
	Method string
	Args   map[string]any
}

// fakeHost 是插件宿主能力面的假实现（记录调用，返回预置应答）。
type fakeHost struct {
	mu        sync.Mutex
	responses map[string]any
	errors    map[string]error
	calls     []callRecord
	statuses  []string
	uiEvents  []map[string]any
}

func newFakeHost() *fakeHost {
	return &fakeHost{responses: map[string]any{}, errors: map[string]error{}}
}

func (f *fakeHost) Call(_ context.Context, method string, args any) (json.RawMessage, error) {
	f.mu.Lock()
	m, _ := args.(map[string]any)
	f.calls = append(f.calls, callRecord{Method: method, Args: m})
	err := f.errors[method]
	resp, ok := f.responses[method]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("fakeHost: 未预置应答 %s", method)
	}
	return json.Marshal(resp)
}

func (f *fakeHost) ReportStatus(_ context.Context, state, detail string) error {
	f.mu.Lock()
	f.statuses = append(f.statuses, state+"|"+detail)
	f.mu.Unlock()
	return nil
}

func (f *fakeHost) PublishUI(_ context.Context, _ string, payload any) error {
	f.mu.Lock()
	if m, ok := payload.(map[string]any); ok {
		f.uiEvents = append(f.uiEvents, m)
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeHost) setResponse(method string, resp any) {
	f.mu.Lock()
	f.responses[method] = resp
	f.mu.Unlock()
}

func (f *fakeHost) callsOf(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, 0, len(f.calls))
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c.Args)
		}
	}
	return out
}

func newTestClient(t *testing.T, h host, values map[string]string) *client {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := &memConfigStore{values: map[string]string{}}
	for k, v := range values {
		store.values[k] = v
	}
	cfg := newConfigManagerWithStore(store, log)
	if err := cfg.Init(context.Background()); err != nil {
		t.Fatalf("cfg.Init: %v", err)
	}
	dev := newDeviceStore(t.TempDir(), "1.0.0", log)
	return newClient(h, log, cfg, dev, newWorkspaceStore(), newRunningSet(), newAskCache())
}

// waitFor 轮询等待条件成立（测试用）。
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待条件超时（%s）", timeout)
}
