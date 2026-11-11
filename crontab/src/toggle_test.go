package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// toggleArgs 构造 crontab_toggle 的调用参数。
func toggleArgs(id string, enabled bool) map[string]any {
	return map[string]any{"task_id": id, "enabled": enabled}
}

func TestToggleEnableClearsReasonAndComputesNext(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_en")
	task.Enabled = false
	task.NextRunAt = 0
	task.DisabledReason = ReasonWorkDirMissing
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	res, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	out := res.(map[string]any)
	if out["enabled"] != true || out["changed"] != true {
		t.Fatalf("返回=%v", out)
	}
	got, _ := store.Get(task.ID)
	if !got.Enabled || got.DisabledReason != "" || got.NextRunAt <= now {
		t.Fatalf("启用结果=%+v", got)
	}
}

// 重复 enable 保持原 next_run_at，不推进已到期触发。
func TestToggleEnableIsIdempotentAndKeepsDueTime(t *testing.T) {
	p, store := addTestPlugin(t, false)
	due := time.Now().Add(-10 * time.Second).Unix()
	task := sampleTask("ct_idem")
	task.Enabled = true
	task.NextRunAt = due
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("toggle: %v", err)
	}
	out := res.(map[string]any)
	if out["enabled"] != true || out["changed"] != false {
		t.Fatalf("重复 enable 应 changed=false: %v", out)
	}
	got, _ := store.Get(task.ID)
	if got.NextRunAt != due {
		t.Fatalf("不得推进已到期 next_run_at: %d → %d", due, got.NextRunAt)
	}
}

// run 任务启用前校验 work_dir；失败不改状态。
func TestToggleEnableRejectsRunWithoutWorkDir(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := Task{
		ID:          "ct_run",
		Cron:        "*/5 * * * *",
		Action:      Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi"}`)},
		Description: "执行：echo hi",
		Enabled:     false,
		NextRunAt:   0,
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	_, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "work_dir") {
		t.Fatalf("run 空 work_dir 应拒绝: %v", err)
	}
	got, _ := store.Get(task.ID)
	if got.Enabled || got.NextRunAt != 0 {
		t.Fatalf("失败后不得启用: %+v", got)
	}
}

// notify 不依赖 work_dir，可在无项目任务上启用。
func TestToggleEnableNotifyWithoutWorkDir(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_notify")
	task.Enabled = false
	task.NextRunAt = 0
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{}); err != nil {
		t.Fatalf("notify 应可启用: %v", err)
	}
	got, _ := store.Get(task.ID)
	if !got.Enabled || got.NextRunAt == 0 {
		t.Fatalf("启用结果=%+v", got)
	}
}

func TestToggleEnableRejectsExpiredOnce(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_once")
	task.Cron = ""
	task.Once = true
	task.RunAt = time.Now().Add(-time.Hour).Unix()
	task.Enabled = false
	task.NextRunAt = 0
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	_, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("过期一次性任务应拒绝: %v", err)
	}
	got, _ := store.Get(task.ID)
	if got.Enabled || got.NextRunAt != 0 {
		t.Fatalf("失败后不得启用: %+v", got)
	}
}

// nextRun 失败时 UpdateTx 回滚，不得留下 enabled=true/next=0。
func TestToggleEnableInvalidCronRollsBack(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_badcron")
	task.Cron = "0 0 30 2 *" // 2 月 30 日无解
	task.Enabled = false
	task.NextRunAt = 0
	task.DisabledReason = ReasonWorkDirMissing
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	if _, err := p.toggle(context.Background(), toggleArgs(task.ID, true), pluginsdk.CallContext{}); err == nil {
		t.Fatal("非法 cron 应报错")
	}
	got, _ := store.Get(task.ID)
	if got.Enabled || got.NextRunAt != 0 || got.DisabledReason != ReasonWorkDirMissing {
		t.Fatalf("失败后应回滚: %+v", got)
	}
}

// 停用：保留定义、清 next、不中止在飞执行；重复停用幂等。
func TestToggleDisableIsIdempotentAndKeepsRunning(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_off")
	task.Enabled = true
	task.RunningSince = time.Now().Add(-5 * time.Second).Unix()
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.toggle(context.Background(), toggleArgs(task.ID, false), pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	out := res.(map[string]any)
	if out["enabled"] != false || out["changed"] != true || out["running"] != true {
		t.Fatalf("返回=%v", out)
	}
	got, _ := store.Get(task.ID)
	if got.Enabled || got.NextRunAt != 0 || got.RunningSince == 0 {
		t.Fatalf("停用结果=%+v", got)
	}
	if got.Action.Type != ActionNotify || got.Cron == "" {
		t.Fatalf("定义应保留: %+v", got)
	}

	res, err = p.toggle(context.Background(), toggleArgs(task.ID, false), pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("repeat disable: %v", err)
	}
	if out = res.(map[string]any); out["changed"] != false {
		t.Fatalf("重复停用应 changed=false: %v", out)
	}
}

func TestToggleRejectsMissingOrInvalidEnabled(t *testing.T) {
	p, _ := addTestPlugin(t, false)
	for name, args := range map[string]map[string]any{
		"缺少 enabled": {"task_id": "ct_x"},
		"enabled 非法": {"task_id": "ct_x", "enabled": "maybe"},
		"enabled 数字": {"task_id": "ct_x", "enabled": 1},
		"未知参数":       {"task_id": "ct_x", "enabled": true, "force": true},
	} {
		if _, err := p.toggle(context.Background(), args, pluginsdk.CallContext{}); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}
