package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t.TempDir())
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func sampleTask(id string) Task {
	return Task{
		ID:          id,
		Cron:        "*/5 * * * *",
		Action:      Action{Type: ActionNotify, Param: json.RawMessage(`"<b>x</b>"`)},
		Description: "通知：x",
		Enabled:     true,
		CreatedAt:   1761878400,
		NextRunAt:   1761878700,
	}
}

func TestStoreCRUDAndReload(t *testing.T) {
	dir := t.TempDir()
	s := newStore(dir)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(sampleTask("ct_a")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(sampleTask("ct_b")); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Update("ct_a", func(cur *Task) error {
		cur.Enabled = false
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := s.Delete("ct_b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// 新实例重新读盘，验证持久化。
	s2 := newStore(dir)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	list := s2.List()
	if len(list) != 1 || list[0].ID != "ct_a" || list[0].Enabled {
		t.Fatalf("reload list=%+v", list)
	}
	if _, ok := s2.Get("ct_b"); ok {
		t.Fatal("ct_b 应已删除")
	}
}

func TestStoreCorruptFileBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	if err := s.Load(); err == nil {
		t.Fatal("损坏文件应返回错误")
	}
	// 原始内容备份保留（不静默覆盖）。
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if len(e.Name()) > len("tasks.json.corrupt-") && e.Name()[:len("tasks.json.corrupt-")] == "tasks.json.corrupt-" {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到损坏备份文件: %v", entries)
	}
	if len(s.List()) != 0 {
		t.Fatal("损坏后应按空任务启动")
	}
}

func TestStoreRecentRuns(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.AppendRun(RunRecord{TaskID: "ct_a", At: int64(100 + i), Kind: "run", OK: i%2 == 0}); err != nil {
			t.Fatalf("AppendRun: %v", err)
		}
	}
	runs := s.RecentRuns(2)
	if len(runs) != 2 {
		t.Fatalf("runs=%d", len(runs))
	}
	if runs[0].At != 102 || runs[1].At != 101 {
		t.Fatalf("应新→旧返回: %+v", runs)
	}
}
