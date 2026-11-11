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

func TestParseScheduleInput(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)

	got, err := parseScheduleInput("*/5 * * * *", nil, nil, now)
	if err != nil || got.Once || got.Cron != "*/5 * * * *" {
		t.Fatalf("cron: %+v err=%v", got, err)
	}

	got, err = parseScheduleInput(nil, nil, "50m", now)
	if err != nil {
		t.Fatalf("delay: %v", err)
	}
	if !got.Once || got.Cron != "" || got.RunAt != now.Add(50*time.Minute).Unix() {
		t.Fatalf("delay=%+v", got)
	}

	at := now.Add(2 * time.Hour).Format("2006-01-02 15:04")
	got, err = parseScheduleInput(nil, at, nil, now)
	if err != nil {
		t.Fatalf("at: %v", err)
	}
	if !got.Once || got.RunAt != now.Add(2*time.Hour).Unix() {
		t.Fatalf("at=%+v", got)
	}

	// 互斥 / 缺失 / 过期 / 非法时长均拒绝。
	cases := []struct {
		name          string
		cron, at, del any
	}{
		{"cron+delay", "*/5 * * * *", nil, "5m"},
		{"at+delay", nil, "2026-11-08 18:30", "5m"},
		{"missing", nil, nil, nil},
		{"past at", nil, "2020-01-01 00:00", nil},
		{"zero delay", nil, nil, "0s"},
		{"bad delay", nil, nil, "soon"},
	}
	for _, c := range cases {
		if _, err := parseScheduleInput(c.cron, c.at, c.del, now); err == nil {
			t.Errorf("%s 应报错", c.name)
		}
	}
}

func TestRecoverOnceTask(t *testing.T) {
	s, store := newTestScheduler(t, &fakeNotifier{})
	now := time.Unix(1761900000, 0)
	grace := int64(s.cfg.Settings().MissedGraceSec)

	future := Task{ID: "ct_once_f", Once: true, RunAt: now.Unix() + 300, Action: Action{Type: ActionNotify},
		Enabled: true, NextRunAt: 1}
	future.Action.Param = json.RawMessage(`"x"`)
	if err := store.Add(future); err != nil {
		t.Fatal(err)
	}
	due := Task{ID: "ct_once_due", Once: true, RunAt: now.Unix() - 10, Action: Action{Type: ActionNotify},
		Enabled: true, NextRunAt: 1}
	due.Action.Param = json.RawMessage(`"x"`)
	if err := store.Add(due); err != nil {
		t.Fatal(err)
	}
	missed := Task{ID: "ct_once_missed", Once: true, RunAt: now.Unix() - grace - 100,
		Action: Action{Type: ActionNotify}, Enabled: true, NextRunAt: 1}
	missed.Action.Param = json.RawMessage(`"x"`)
	if err := store.Add(missed); err != nil {
		t.Fatal(err)
	}

	s.Recover(now)

	if got, _ := store.Get("ct_once_f"); got.NextRunAt != future.RunAt {
		t.Fatalf("未来一次性任务 next_run_at=%d want %d", got.NextRunAt, future.RunAt)
	}
	if got, _ := store.Get("ct_once_due"); got.NextRunAt != now.Unix() {
		t.Fatalf("grace 内应 due now，got %d", got.NextRunAt)
	}
	got, _ := store.Get("ct_once_missed")
	if got.Enabled || got.NextRunAt != 0 || got.DisabledReason != ReasonOnceMissed {
		t.Fatalf("错过的一次性任务应终态停用: %+v", got)
	}
	if got.LastRun == nil || got.LastRun.Detail != "missed" {
		t.Fatalf("应记录 skipped:missed: %+v", got.LastRun)
	}
}

func TestExecuteOnceTaskAutoDisables(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	task := Task{
		ID: "ct_once_exec", Once: true, RunAt: time.Now().Unix() - 1,
		Action:      Action{Type: ActionNotify, Param: json.RawMessage(`"一次性完成"`)},
		Description: "通知：一次性完成", Enabled: true, RunningSince: time.Now().Unix(),
		CreatedBy: CreatedBy{WorkDir: t.TempDir()},
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	s.execute(task)

	got, _ := store.Get("ct_once_exec")
	if got.Enabled || got.NextRunAt != 0 || got.DisabledReason != ReasonOnceCompleted {
		t.Fatalf("一次性任务执行后应终态停用: %+v", got)
	}
	if got.RunCount != 1 || got.LastRun == nil || !got.LastRun.OK {
		t.Fatalf("执行收口不符: %+v", got)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("应弹一次通知，got %d", len(fake.calls))
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

func TestParseCronRejectsEmptyAndBadEvery(t *testing.T) {
	for _, expr := range []string{"", "   ", "@every", "@every 0s", "@every 500ms", "@every not-a-duration"} {
		if _, err := parseCron(expr); err == nil {
			t.Fatalf("%q 应被拒绝", expr)
		}
	}
	if _, err := parseCron("@every 5m"); err != nil {
		t.Fatalf("@every 5m 应通过: %v", err)
	}
	if _, err := parseCron("@every 1s"); err != nil {
		t.Fatalf("@every 1s 应通过: %v", err)
	}
}

// TestMissedAndOverlapVisibleInLastRun：misfire / overlap 必须写入 Task.LastRun（不只是 runs.jsonl）。
func TestMissedAndOverlapVisibleInLastRun(t *testing.T) {
	s, store := newTestScheduler(t, &fakeNotifier{})
	now := time.Unix(1761900000, 0)
	grace := int64(s.cfg.Settings().MissedGraceSec)

	missed := sampleTask("ct_missed_lr")
	missed.NextRunAt = now.Unix() - grace - 100
	if err := store.Add(missed); err != nil {
		t.Fatal(err)
	}
	s.Recover(now)
	got, _ := store.Get("ct_missed_lr")
	if got.LastRun == nil || got.LastRun.Detail != "missed" || got.LastRun.Kind != "skipped" {
		t.Fatalf("missed 应写入 LastRun: %+v", got.LastRun)
	}

	o := sampleTask("ct_overlap_lr")
	o.Cron = "* * * * *"
	o.NextRunAt = now.Unix()
	o.RunningSince = now.Unix() - 5
	if err := store.Add(o); err != nil {
		t.Fatal(err)
	}
	s.tick(now)
	got, _ = store.Get("ct_overlap_lr")
	if got.LastRun == nil || got.LastRun.Detail != "overlap" || got.LastRun.Kind != "skipped" {
		t.Fatalf("overlap 应写入 LastRun: %+v", got.LastRun)
	}
}
