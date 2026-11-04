package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestProbeRealServer 是本地联调用的诊断用例：仅在显式设置 CK_REAL_SERVER 时运行，
// 连接真实 Remote Server 并打印一次连接的断开原因。
func TestProbeRealServer(t *testing.T) {
	url := os.Getenv("CK_REAL_SERVER")
	if url == "" {
		t.Skip("set CK_REAL_SERVER=ws://127.0.0.1:18080/tunnel")
	}
	c := newTestClient(t, newFakeHost(), map[string]string{
		KeyServerURL: url, KeyUsername: "demo", KeyPassword: "demo123",
	})
	dev := c.dev.Ensure()
	cfg := settings{ServerURL: url, Username: "demo", Password: "demo123"}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out := c.connectOnce(ctx, cfg, dev, false)
	t.Logf("outcome connected=%v authFailed=%v err=%v elapsed=%s", out.connected, out.authFailed, out.err, time.Since(start))
	if !out.connected {
		t.Fatal("expected the connection to be established")
	}
}
