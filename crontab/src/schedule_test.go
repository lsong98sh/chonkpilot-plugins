package main

import (
	"encoding/json"
	"testing"
	"time"
)

func testConfigManager() *configManager {
	return &configManager{values: map[string]string{}, loaded: true}
}

// fakeNotifier 记录系统通知（测试不真弹）。
type fakeNotifier struct {
	calls [][3]string
	err   error
}

func (f *fakeNotifier) Notify(title, body, clickURL string) error {
	f.calls = append(f.calls, [3]string{title, body, clickURL})
	return f.err
}

func newTestScheduler(t *testing.T, notifier Notifier) (*scheduler, *Store) {
	t.Helper()
	store := newTestStore(t)
	cfg := testConfigManager()
	run := &runner{cfg: cfg, notifier: notifier, logf: t.Logf}
	s := newScheduler(store, cfg, run, t.Logf)
	return s, store
}

func TestRecoverGraceImmediateAndMissed(t *testing.T) {
	s, store := newTestScheduler(t, &fakeNotifier{})
	now := time.Unix(1761900000, 0)
	grace := int64(s.cfg.Settings().MissedGraceSec)

	due := sampleTask("ct_due")
	due.NextRunAt = now.Unix() - 10 // grace 内 → 立即补跑（NextRunAt = now）
	if err := store.Add(due); err != nil {
		t.Fatal(err)
	}
	missed := sampleTask("ct_missed")
	missed.NextRunAt = now.Unix() - grace - 100
	if err := store.Add(missed); err != nil {
		t.Fatal(err)
	}

	s.Recover(now)

	got, _ := store.Get("ct_due")
	if got.NextRunAt != now.Unix() {
		t.Fatalf("grace 内应 due now，got %d want %d", got.NextRunAt, now.Unix())
	}
	got, _ = store.Get("ct_missed")
	if got.NextRunAt <= now.Unix() {
		t.Fatalf("超出补偿窗口应重算到未来，got %d", got.NextRunAt)
	}
	runs := store.RecentRuns(10)
	found := false
	for _, r := range runs {
		if r.TaskID == "ct_missed" && r.Kind == "skipped" && r.Detail == "missed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录 skipped:missed，runs=%+v", runs)
	}
}

func TestRecoverInterruptedAndDisabled(t *testing.T) {
	s, store := newTestScheduler(t, &fakeNotifier{})
	now := time.Unix(1761900000, 0)

	task := sampleTask("ct_x")
	task.RunningSince = now.Unix() - 50
	task.Enabled = false
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	s.Recover(now)

	got, _ := store.Get("ct_x")
	if got.RunningSince != 0 {
		t.Fatal("中断标记应被清理")
	}
	if got.NextRunAt != 0 {
		t.Fatalf("停用任务 next_run_at 应为 0，got %d", got.NextRunAt)
	}
	if got.LastRun == nil || got.LastRun.Kind != "interrupted" {
		t.Fatalf("应记录 interrupted: %+v", got.LastRun)
	}
}

func TestTickOverlapSkipsAndAdvances(t *testing.T) {
	s, store := newTestScheduler(t, &fakeNotifier{})
	now := time.Unix(1761900000, 0)

	task := sampleTask("ct_o")
	task.Cron = "* * * * *"
	task.NextRunAt = now.Unix()
	task.RunningSince = now.Unix() - 5
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.tick(now)

	got, _ := store.Get("ct_o")
	if got.NextRunAt <= now.Unix() {
		t.Fatalf("overlap 后应推进下次执行，got %d", got.NextRunAt)
	}
	if got.RunningSince == 0 {
		t.Fatal("overlap 不应清除正在运行的标记")
	}
	runs := store.RecentRuns(10)
	found := false
	for _, r := range runs {
		if r.TaskID == "ct_o" && r.Kind == "skipped" && r.Detail == "overlap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应记录 skipped:overlap，runs=%+v", runs)
	}
}

func TestExecuteNotifyAction(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	now := time.Now()

	task := Task{
		ID:           "ct_n",
		Cron:         "* * * * *",
		Action:       Action{Type: ActionNotify, Param: json.RawMessage(`{"html":"<b>喝水</b>","click_url":"https://example.com"}`)},
		Description:  "通知：喝水",
		Enabled:      true,
		NextRunAt:    now.Unix(),
		RunningSince: now.Unix(),
		CreatedBy:    CreatedBy{WorkDir: t.TempDir()},
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	if len(fake.calls) != 1 {
		t.Fatalf("应弹一次通知，got %d", len(fake.calls))
	}
	if fake.calls[0][1] != "喝水" || fake.calls[0][2] != "https://example.com" {
		t.Fatalf("通知内容不符: %+v", fake.calls[0])
	}
	got, _ := store.Get("ct_n")
	if got.RunningSince != 0 || got.RunCount != 1 || got.LastRun == nil || !got.LastRun.OK {
		t.Fatalf("执行收口不符: %+v", got)
	}
}

func TestParseCronRejectsSeconds(t *testing.T) {
	if _, err := parseCron("*/5 * * * * *"); err == nil {
		t.Fatal("6 段（含秒）应被拒绝")
	}
	if _, err := parseCron("*/5 * * * *"); err != nil {
		t.Fatalf("5 段应通过: %v", err)
	}
	if _, err := parseCron("@daily"); err != nil {
		t.Fatalf("@daily 应通过: %v", err)
	}
}
