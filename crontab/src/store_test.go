package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t.TempDir())
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s := newStore(dir)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
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
	s := openStore(t, dir)
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
	on := true
	updated, err := s.UpdateTx("ct_b", func(cur *Task) error {
		cur.Sandbox = &on
		cur.DisabledReason = ReasonWorkDirMissing
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateTx: %v", err)
	}
	if updated.Sandbox == nil || !*updated.Sandbox || updated.DisabledReason != ReasonWorkDirMissing {
		t.Fatalf("UpdateTx 返回值不符: %+v", updated)
	}
	if _, err := s.Delete("ct_b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 新实例重新打开同一 DB，验证持久化。
	s2 := openStore(t, dir)
	list := s2.List()
	if len(list) != 1 || list[0].ID != "ct_a" || list[0].Enabled {
		t.Fatalf("reload list=%+v", list)
	}
	if _, ok := s2.Get("ct_b"); ok {
		t.Fatal("ct_b 应已删除")
	}
}

func TestStorePersistsLastRunAndActionParam(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	task := sampleTask("ct_json")
	task.Action = Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi","notify":true}`)}
	task.LastRun = &RunRecord{TaskID: "ct_json", At: 1761878400, Kind: "run", OK: true, ExitCode: 0, MS: 12}
	if err := s.Add(task); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	s2 := openStore(t, dir)
	got, ok := s2.Get("ct_json")
	if !ok {
		t.Fatal("任务应存在")
	}
	if got.Action.Type != ActionRun || string(got.Action.Param) != `{"command":"echo hi","notify":true}` {
		t.Fatalf("action 往返不符: %+v", got.Action)
	}
	if got.LastRun == nil || !got.LastRun.OK || got.LastRun.MS != 12 {
		t.Fatalf("last_run 往返不符: %+v", got.LastRun)
	}
	if got.Sandbox != nil {
		t.Fatalf("nil sandbox 应保持「继承」: %v", *got.Sandbox)
	}
}

func TestStoreCloseIsIdempotentAndOperationsFail(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	if err := s.Add(sampleTask("ct_c")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等: %v", err)
	}
	if err := s.Add(sampleTask("ct_d")); err == nil {
		t.Fatal("Close 后 Add 应返回错误")
	}
	if err := s.Update("ct_c", func(*Task) error { return nil }); err == nil {
		t.Fatal("Close 后 Update 应返回错误")
	}
	if _, err := s.UpdateTx("ct_c", func(*Task) error { return nil }); err == nil {
		t.Fatal("Close 后 UpdateTx 应返回错误")
	}
	if _, err := s.Delete("ct_c"); err == nil {
		t.Fatal("Close 后 Delete 应返回错误")
	}
	if _, ok := s.Get("ct_c"); ok {
		t.Fatal("Close 后 Get 应失败")
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("Close 后 List 应为空: %+v", list)
	}
	if err := s.AppendRun(RunRecord{TaskID: "ct_c", At: 1, Kind: "run"}); err == nil {
		t.Fatal("Close 后 AppendRun 应返回错误")
	}
	if runs := s.RecentRuns(5); len(runs) != 0 {
		t.Fatalf("Close 后 RecentRuns 应为空: %+v", runs)
	}
}

// AddLimited 的上限判定与插入同事务；达到上限后不再写入。
func TestStoreAddLimited(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddLimited(sampleTask("ct_a"), 1); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := s.AddLimited(sampleTask("ct_b"), 1); err == nil {
		t.Fatal("超过上限应拒绝")
	}
	if err := s.Add(sampleTask("ct_a")); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 id 应拒绝: %v", err)
	}
	list := s.List()
	if len(list) != 1 || list[0].ID != "ct_a" {
		t.Fatalf("list=%+v", list)
	}
}

// mutate 返回错误 → 事务回滚，任务保持原值。
func TestStoreUpdateTxRollsBackOnMutateError(t *testing.T) {
	s := newTestStore(t)
	if err := s.Add(sampleTask("ct_u")); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	_, err := s.UpdateTx("ct_u", func(cur *Task) error {
		cur.Cron = "0 0 * * *"
		cur.Description = "changed"
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回 mutate 错误: %v", err)
	}
	got, _ := s.Get("ct_u")
	if got.Cron != "*/5 * * * *" || got.Description != "通知：x" {
		t.Fatalf("失败后应回滚: %+v", got)
	}
	if err := s.Update("ct_missing", func(*Task) error { return nil }); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("不存在应返回 ErrTaskNotFound: %v", err)
	}
}

func TestStoreRecentRuns(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.AppendRun(RunRecord{TaskID: "ct_a", At: int64(100 + i), Kind: "run", OK: i%2 == 0}); err != nil {
			t.Fatalf("AppendRun: %v", err)
		}
	}
	if err := s.AppendRun(RunRecord{TaskID: "ct_b", At: 200, Kind: "run", OK: true}); err != nil {
		t.Fatal(err)
	}
	runs := s.RecentRuns(2)
	if len(runs) != 2 {
		t.Fatalf("runs=%d", len(runs))
	}
	// at 相同则按 seq 新→旧；这里 at 不同，应新→旧返回。
	if runs[0].TaskID != "ct_b" || runs[0].At != 200 || runs[1].At != 102 {
		t.Fatalf("应新→旧返回: %+v", runs)
	}
	only := s.RecentRunsFor("ct_a", 10)
	if len(only) != 3 || only[0].At != 102 || only[2].At != 100 {
		t.Fatalf("按任务过滤不符: %+v", only)
	}
	if got := s.RecentRunsFor("ct_missing", 5); len(got) != 0 {
		t.Fatalf("未知任务应为空: %+v", got)
	}
}

// 旧版 tasks.json / runs.jsonl：首启一次性导入 → 旧文件改名；重复 Load 不重复导入。
func TestStoreImportsLegacyJSONOnce(t *testing.T) {
	dir := t.TempDir()
	on := true
	legacy := tasksFile{Tasks: []*Task{
		{
			ID: "ct_legacy_a", Cron: "*/5 * * * *",
			Action:      Action{Type: ActionNotify, Param: json.RawMessage(`"<b>A</b>"`)},
			Description: "通知：A", Enabled: true, CreatedAt: 100, NextRunAt: 200,
			Sandbox: &on,
			LastRun: &RunRecord{TaskID: "ct_legacy_a", At: 150, Kind: "notify", OK: true},
		},
		{
			ID: "ct_legacy_b", Once: true, RunAt: 300,
			Action:      Action{Type: ActionNotify, Param: json.RawMessage(`"B"`)},
			Description: "通知：B", Enabled: true, CreatedAt: 101, NextRunAt: 300,
		},
	}}
	rawTasks, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, legacyTasksName), rawTasks, 0o600); err != nil {
		t.Fatal(err)
	}
	runsRaw := strings.Join([]string{
		`{"task_id":"ct_legacy_a","at":100,"kind":"run","ok":true}`,
		`not-json`,
		`{"task_id":"ct_legacy_a","at":300,"kind":"run","ok":false,"error":"x"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, legacyRunsName), []byte(runsRaw), 0o600); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir)
	list := s.List()
	if len(list) != 2 || list[0].ID != "ct_legacy_a" || list[1].ID != "ct_legacy_b" {
		t.Fatalf("legacy tasks 导入不符: %+v", list)
	}
	if list[0].Sandbox == nil || !*list[0].Sandbox || list[0].LastRun == nil || !list[0].LastRun.OK {
		t.Fatalf("legacy 字段丢失: %+v", list[0])
	}
	runs := s.RecentRuns(10)
	if len(runs) != 2 || runs[0].At != 300 || runs[1].At != 100 {
		t.Fatalf("legacy runs 导入不符: %+v", runs)
	}
	assertMigrated(t, filepath.Join(dir, legacyTasksName))
	assertMigrated(t, filepath.Join(dir, legacyRunsName))
	_ = s.Close()

	// 重开同一目录：不重复导入（行数不变）。
	s2 := openStore(t, dir)
	if n := len(s2.List()); n != 2 {
		t.Fatalf("重开后 tasks=%d，不应重复导入", n)
	}
	if n := len(s2.RecentRuns(10)); n != 2 {
		t.Fatalf("重开后 runs=%d，不应重复导入", n)
	}
}

// assertMigrated 断言原文件已不存在、存在 .migrated-<unix> 备份。
func assertMigrated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("旧文件应已改名: %s exists err=%v", path, err)
	}
	matches, _ := filepath.Glob(path + ".migrated-*")
	if len(matches) != 1 {
		t.Fatalf("未找到迁移备份 %s.migrated-*: %v", path, matches)
	}
}

func TestStoreCorruptLegacyTasksBackedUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, legacyTasksName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newStore(dir)
	if err := s.Load(); err == nil {
		t.Fatal("损坏文件应返回错误")
	}
	t.Cleanup(func() { _ = s.Close() })
	// 原始内容备份保留（不静默覆盖）。
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), legacyTasksName+".corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("未找到损坏备份文件: %v", entries)
	}
	if len(s.List()) != 0 {
		t.Fatal("损坏后应按空任务启动")
	}
	if _, err := os.Stat(s.path); err != nil {
		t.Fatalf("DB 应已建表: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("损坏原文件应保留: %v", err)
	}
}

// 两个 Store（模拟两个进程）并发认领同一任务：只有一个能 claim。
func TestStoreCrossInstanceAtomicClaim(t *testing.T) {
	dir := t.TempDir()
	s1 := openStore(t, dir)
	s2 := openStore(t, dir)
	if err := s1.Add(sampleTask("ct_claim")); err != nil {
		t.Fatal(err)
	}

	var claims, sawRunning int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, st := range []*Store{s1, s2} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			<-start
			err := st.Update("ct_claim", func(cur *Task) error {
				if cur.RunningSince > 0 {
					atomic.AddInt32(&sawRunning, 1)
					return nil
				}
				cur.RunningSince = time.Now().Unix()
				atomic.AddInt32(&claims, 1)
				return nil
			})
			if err != nil {
				t.Errorf("Update: %v", err)
			}
		}(st)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt32(&claims); got != 1 {
		t.Fatalf("应恰好一个 claim，实际 %d", got)
	}
	if got := atomic.LoadInt32(&sawRunning); got != 1 {
		t.Fatalf("另一个应看到 RunningSince>0，实际 %d", got)
	}
	got, ok := s1.Get("ct_claim")
	if !ok || got.RunningSince == 0 {
		t.Fatalf("认领后的任务应带 running_since: %+v", got)
	}
}
