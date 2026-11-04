package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// SANDBOX-WRITE-1 P3（C13 / 方案 §5.5.2 ②）：执行前经宿主 `plugin/v1/workdir.ensure` 确保项目
// 已打开；失败 → 本轮跳过 + 记错（`workdir-open-failed`），**不 disable**、不执行命令。
func TestRunCommandSkipsWhenWorkdirEnsureFails(t *testing.T) {
	var gotDir string
	r := &runner{
		cfg:      &configManager{values: map[string]string{}, loaded: true},
		notifier: &fakeNotifier{},
		logf:     t.Logf,
		ensureWorkdir: func(_ context.Context, workDir string) (string, error) {
			gotDir = workDir
			return "", errors.New("等待 instance-register 超时（15s）")
		},
	}
	task := Task{
		ID:        "ct_ensure_fail",
		Action:    Action{Type: ActionRun},
		CreatedBy: CreatedBy{WorkDir: t.TempDir()},
	}
	spec := &ActionSpec{Type: ActionRun, Run: &RunParam{Command: "echo should-not-run"}}

	rec := r.runCommand(context.Background(), task, spec, time.Now().Unix())
	if rec.OK {
		t.Fatalf("ensure 失败应跳过本轮: %+v", rec)
	}
	if rec.Kind != "skipped" || rec.Detail != "workdir-open-failed" {
		t.Fatalf("运行记录不符: kind=%q detail=%q", rec.Kind, rec.Detail)
	}
	if !strings.Contains(rec.Error, "超时") {
		t.Fatalf("错误原因应透传: %q", rec.Error)
	}
	if gotDir == "" {
		t.Fatal("应把任务 work_dir 透传给宿主")
	}
	if rec.OutputTail != "" || rec.ExitCode != 0 {
		t.Fatalf("跳过时不得执行命令: %+v", rec)
	}
}

// ensure 成功 → 照常执行（且先 ensure 再 resolveSandbox：由调用顺序保证）。
func TestRunCommandEnsuresProjectBeforeSandbox(t *testing.T) {
	var order []string
	r := &runner{
		cfg:      &configManager{values: map[string]string{KeySandbox: "true", KeyDefaultTimeoutSec: "60"}, loaded: true},
		notifier: &fakeNotifier{},
		logf:     t.Logf,
		ensureWorkdir: func(context.Context, string) (string, error) {
			order = append(order, "ensure")
			return "ins-main", nil
		},
		resolveSandbox: func(context.Context, string) ([]string, error) {
			order = append(order, "sandbox")
			return nil, errors.New("method not found: plugin/v1/sandbox.resolve")
		},
	}
	task := Task{
		ID:        "ct_ensure_ok",
		Action:    Action{Type: ActionRun},
		CreatedBy: CreatedBy{WorkDir: t.TempDir()},
	}
	spec := &ActionSpec{Type: ActionRun, Run: &RunParam{Command: "echo ok"}}

	rec := r.runCommand(context.Background(), task, spec, time.Now().Unix())
	if !rec.OK {
		t.Fatalf("应照常执行: %+v", rec)
	}
	if len(order) != 2 || order[0] != "ensure" || order[1] != "sandbox" {
		t.Fatalf("先 ensure 后 sandbox，实际 %v", order)
	}
}

// 调度口径：ensure 失败**不 disable** 任务（与 P2 的 work_dir_missing 区分），运行标记被清除。
func TestExecuteDoesNotDisableWhenWorkdirEnsureFails(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	s.runner.ensureWorkdir = func(context.Context, string) (string, error) {
		return "", errors.New("等待 instance-register 超时（15s）")
	}
	task := Task{
		ID:          "ct_ensure_retry",
		Cron:        "* * * * *",
		Action:      Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi"}`)},
		Description: "执行：echo hi",
		Enabled:     true,
		CreatedBy:   CreatedBy{WorkDir: t.TempDir()},
		NextRunAt:   time.Now().Unix(),
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	got, _ := store.Get("ct_ensure_retry")
	if !got.Enabled {
		t.Fatal("ensure 失败不得停用任务（可重试）")
	}
	if got.DisabledReason != "" {
		t.Fatalf("不得写 disabled_reason: %q", got.DisabledReason)
	}
	if got.RunningSince != 0 {
		t.Fatalf("运行标记应清除: %d", got.RunningSince)
	}
	if got.LastRun == nil || got.LastRun.Kind != "skipped" || got.LastRun.Detail != "workdir-open-failed" {
		t.Fatalf("last_run = %+v", got.LastRun)
	}
	runs := store.RecentRunsFor("ct_ensure_retry", 5)
	if len(runs) != 1 || runs[0].Detail != "workdir-open-failed" {
		t.Fatalf("runs = %+v", runs)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("本轮跳过不弹通知: %d", len(fake.calls))
	}
}
