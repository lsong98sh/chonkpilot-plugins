package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// P2（C14）：run 任务的 work_dir 不存在 → 任务停用（work_dir_missing）+ skipped 记录，
// 只通知一次，且不执行命令。
func TestExecuteDisablesTaskWhenWorkDirMissing(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	missing := filepath.Join(t.TempDir(), "gone")
	task := Task{
		ID:          "ct_missing",
		Cron:        "* * * * *",
		Action:      Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi"}`)},
		Description: "执行：echo hi",
		Enabled:     true,
		CreatedBy:   CreatedBy{WorkDir: missing},
		NextRunAt:   time.Now().Unix(),
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	got, _ := store.Get("ct_missing")
	if got.Enabled {
		t.Fatal("work_dir 缺失应停用任务")
	}
	if got.NextRunAt != 0 || got.RunningSince != 0 {
		t.Fatalf("停用后应清 next_run_at / running_since: %+v", got)
	}
	if got.DisabledReason != ReasonWorkDirMissing {
		t.Fatalf("disabled_reason = %q", got.DisabledReason)
	}
	if got.LastRun == nil || got.LastRun.Kind != "skipped" || got.LastRun.Detail != "workdir-missing" {
		t.Fatalf("last_run = %+v", got.LastRun)
	}
	runs := store.RecentRunsFor("ct_missing", 5)
	if len(runs) != 1 || runs[0].Detail != "workdir-missing" {
		t.Fatalf("runs = %+v", runs)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("应通知一次，got %d", len(fake.calls))
	}

	// 再次执行（已带 disabled_reason）→ 不重复通知。
	s.execute(got)
	if len(fake.calls) != 1 {
		t.Fatalf("停用通知只应发生一次，got %d", len(fake.calls))
	}
}

// P2：拿不到沙箱策略 → fail-open（不隔离执行）+ RunRecord.Detail 留痕。
func TestRunCommandFailOpenWhenResolveUnavailable(t *testing.T) {
	notifier := &fakeNotifier{}
	r := &runner{
		cfg: &configManager{
			values: map[string]string{KeySandbox: "true", KeyDefaultTimeoutSec: "60"},
			loaded: true,
		},
		notifier: notifier,
		logf:     t.Logf,
		resolveSandbox: func(context.Context, string) ([]string, error) {
			return nil, errors.New("method not found: plugin/v1/sandbox.resolve")
		},
	}
	task := Task{
		ID:        "ct_failopen",
		Action:    Action{Type: ActionRun},
		CreatedBy: CreatedBy{WorkDir: t.TempDir()},
	}
	spec := &ActionSpec{Type: ActionRun, Run: &RunParam{Command: "echo ok"}}

	rec := r.runCommand(context.Background(), task, spec, time.Now().Unix())
	if !rec.OK {
		t.Fatalf("fail-open 不应阻断任务: %+v", rec)
	}
	if !strings.Contains(rec.Detail, "sandbox_unavailable") {
		t.Fatalf("应记录 sandbox_unavailable 原因，detail=%q", rec.Detail)
	}
}

// 任务级开关解析（三态）+ 生效优先级（任务级 > 插件配置）。
func TestParseSandboxArgAndEffective(t *testing.T) {
	val, present, err := parseSandboxArg(nil)
	if err != nil || present || val != nil {
		t.Fatalf("缺省应继承: val=%v present=%v err=%v", val, present, err)
	}
	val, present, err = parseSandboxArg(true)
	if err != nil || !present || val == nil || !*val {
		t.Fatalf("true 解析失败: %v %v %v", val, present, err)
	}
	val, present, err = parseSandboxArg("false")
	if err != nil || !present || val == nil || *val {
		t.Fatalf("false 解析失败: %v %v %v", val, present, err)
	}
	val, present, err = parseSandboxArg("inherit")
	if err != nil || !present || val != nil {
		t.Fatalf("inherit 应解析为「恢复继承」: %v %v %v", val, present, err)
	}
	if _, _, err := parseSandboxArg("yes-please"); err == nil {
		t.Fatal("非法取值应报错")
	}

	on, off := true, false
	if effectiveSandbox(Task{}, settings{Sandbox: false}) {
		t.Fatal("缺省应为关（继承 false）")
	}
	if !effectiveSandbox(Task{Sandbox: &on}, settings{Sandbox: false}) {
		t.Fatal("任务级 true 应覆盖插件配置 false")
	}
	if effectiveSandbox(Task{Sandbox: &off}, settings{Sandbox: true}) {
		t.Fatal("任务级 false 应覆盖插件配置 true")
	}
}

// 任务模型的 sandbox / disabled_reason 随 tasks.json 往返（nil = 继承，不落空值）。
func TestTaskSandboxPersistence(t *testing.T) {
	on := true
	task := sampleTask("ct_p")
	task.Sandbox = &on
	task.DisabledReason = ReasonWorkDirMissing
	raw, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var back Task
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Sandbox == nil || !*back.Sandbox || back.DisabledReason != ReasonWorkDirMissing {
		t.Fatalf("往返丢失字段: %+v", back)
	}

	inherit := sampleTask("ct_i")
	raw, _ = json.Marshal(inherit)
	if strings.Contains(string(raw), "sandbox") {
		t.Fatalf("nil sandbox 不应落盘（保持「继承」语义）: %s", raw)
	}
}
