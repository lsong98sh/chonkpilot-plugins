package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadIndexStatus(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在 → (nil, nil)
	st, err := ReadIndexStatus(dir)
	if st != nil || err != nil {
		t.Fatalf("不存在时应为 (nil,nil)，实际 st=%+v err=%v", st, err)
	}

	// 坏 JSON → error
	path := IndexStatusPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndexStatus(dir); err == nil {
		t.Fatalf("坏 JSON 应返回 error")
	}

	// 正常读取
	want := IndexStatus{State: "indexing", Pending: 3, Message: "处理中", UpdatedAt: "2026-10-19T00:00:00Z"}
	if err := WriteIndexStatus(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadIndexStatus(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil || *got != want {
		t.Fatalf("读回不一致: got=%+v want=%+v", got, want)
	}
}

func TestWriteIndexStatusAtomic(t *testing.T) {
	dir := t.TempDir()
	path := IndexStatusPath(dir)

	first := IndexStatus{State: "ready", Pending: 0, Message: "", UpdatedAt: "2026-10-19T00:00:00Z"}
	if err := WriteIndexStatus(dir, first); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Fatalf("临时文件未清理: %s", path+".tmp")
	}
	got, err := ReadIndexStatus(dir)
	if err != nil || got == nil || *got != first {
		t.Fatalf("读回不一致: got=%+v err=%v", got, err)
	}

	second := IndexStatus{State: "error", Pending: 1, Message: "boom", UpdatedAt: "2026-10-19T01:00:00Z"}
	if err := WriteIndexStatus(dir, second); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	got, err = ReadIndexStatus(dir)
	if err != nil || got == nil || *got != second {
		t.Fatalf("覆盖后读回不一致: got=%+v err=%v", got, err)
	}
}

func withIndexWait(t *testing.T, wait, poll time.Duration) {
	t.Helper()
	oldWait, oldPoll := indexReadyWait, indexReadyPoll
	indexReadyWait, indexReadyPoll = wait, poll
	t.Cleanup(func() {
		indexReadyWait, indexReadyPoll = oldWait, oldPoll
	})
}

func TestReadyGateStatusError(t *testing.T) {
	dir := t.TempDir()
	if err := WriteIndexStatus(dir, IndexStatus{State: "error", Message: "索引文件损坏"}); err != nil {
		t.Fatal(err)
	}
	resp, w, ok := readyGate(map[string]any{"workdir": dir})
	if ok || w != nil {
		t.Fatalf("status=error 不应就绪: ok=%v w=%v", ok, w)
	}
	m, _ := resp.(map[string]any)
	if m["status"] != "error" || m["state"] != "error" || m["message"] != "索引文件损坏" {
		t.Fatalf("应答不符: %v", resp)
	}
}

func TestReadyGatePendingTimeout(t *testing.T) {
	withIndexWait(t, 100*time.Millisecond, 10*time.Millisecond)
	dir := t.TempDir()
	if err := WriteIndexStatus(dir, IndexStatus{State: "indexing", Pending: 3}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, w, ok := readyGate(map[string]any{"workdir": dir})
	if ok || w != nil {
		t.Fatalf("有待索引项不应就绪: ok=%v w=%v", ok, w)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Fatalf("等待时间过短（未按有界等待轮询）: %v", elapsed)
	}
	m, _ := resp.(map[string]any)
	if m["status"] != "pending" || m["state"] != "indexing" {
		t.Fatalf("应答不符: %v", resp)
	}
	if m["pending"] != 3 {
		t.Fatalf("pending=%v，期望 3", m["pending"])
	}
	if m["message"] == "" || m["message"] == nil {
		t.Fatalf("pending 应答应带 message: %v", resp)
	}
}

func TestReadyGateBecomesReady(t *testing.T) {
	withIndexWait(t, 2*time.Second, 10*time.Millisecond)
	dir := t.TempDir()
	if err := WriteIndexStatus(dir, IndexStatus{State: "indexing", Pending: 1}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = WriteIndexStatus(dir, IndexStatus{State: "ready", Pending: 0, UpdatedAt: "2026-10-19T00:00:00Z"})
	}()

	// 空 workspace 无 meta → EnsureReady 返回 not_initialized；关键是未被 pending 门拦下。
	resp, _, ok := readyGate(map[string]any{"workdir": dir})
	if ok {
		t.Fatalf("状态已 ready，不应被 pending 门拦下: %v", resp)
	}
	if m, _ := resp.(map[string]any); m["status"] == "pending" {
		t.Fatalf("状态已 ready 仍返回 pending: %v", resp)
	}
}

func TestReadyGateNoStatusKeepsExistingBehavior(t *testing.T) {
	dir := t.TempDir()
	resp, _, ok := readyGate(map[string]any{"workdir": dir})
	if ok {
		t.Fatalf("空 workspace 不应就绪: %v", resp)
	}
	m, _ := resp.(map[string]any)
	if m["status"] != "not_initialized" {
		t.Fatalf("无 status.json 应走既有 EnsureReady 分支（not_initialized），实际 %v", resp)
	}
}
