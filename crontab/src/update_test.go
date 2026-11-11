package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// 把既有 notify 任务改成 run/agent 时同样要走 work_dir 前置校验。
func TestUpdateRejectsRunActionWithoutWorkDir(t *testing.T) {
	p, store := addTestPlugin(t, false)
	if err := store.Add(sampleTask("ct_u")); err != nil {
		t.Fatal(err)
	}
	_, err := p.update(context.Background(), map[string]any{
		"task_id": "ct_u",
		"action": map[string]any{
			"type":  ActionRun,
			"param": map[string]any{"command": "echo hi"},
		},
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "work_dir") {
		t.Fatalf("改 run 应校验 work_dir: %v", err)
	}
	got, _ := store.Get("ct_u")
	if got.Action.Type != ActionNotify {
		t.Fatalf("失败后动作不得改变: %+v", got.Action)
	}
}

// 仅传 task_id = 明确 no-op：changed 为空，且不推进 next_run_at。
func TestUpdateNoopReturnsEmptyChanged(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_noop")
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.update(context.Background(), map[string]any{"task_id": task.ID}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	out := res.(map[string]any)
	if got := out["changed"].([]string); len(got) != 0 {
		t.Fatalf("changed=%v", got)
	}
	got, _ := store.Get(task.ID)
	if got.NextRunAt != task.NextRunAt {
		t.Fatalf("no-op 不得推进 next_run_at: %d → %d", task.NextRunAt, got.NextRunAt)
	}
}

// 显式空串 / 空白不是「保持不变」，必须报错；null 才是省略。
func TestUpdateRejectsEmptyScheduleArgs(t *testing.T) {
	p, store := addTestPlugin(t, false)
	if err := store.Add(sampleTask("ct_empty")); err != nil {
		t.Fatal(err)
	}
	for key, val := range map[string]any{
		"cron":  "",
		"at":    "  ",
		"delay": "",
	} {
		_, err := p.update(context.Background(), map[string]any{
			"task_id": "ct_empty", key: val,
		}, pluginsdk.CallContext{})
		if err == nil || !strings.Contains(err.Error(), key+" 不能为空") {
			t.Fatalf("%s=%q 应报错: %v", key, val, err)
		}
	}
	if _, err := p.update(context.Background(), map[string]any{
		"task_id": "ct_empty", "cron": nil,
	}, pluginsdk.CallContext{}); err != nil {
		t.Fatalf("null 应与省略同义: %v", err)
	}
	// 非字符串类型不能静默忽略。
	if _, err := p.update(context.Background(), map[string]any{
		"task_id": "ct_empty", "cron": map[string]any{"x": 1},
	}, pluginsdk.CallContext{}); err == nil {
		t.Fatal("cron 非字符串应报错")
	}
}

// 仅改 action/sandbox 不重算 next_run_at，避免丢掉已到期待认领的触发。
func TestUpdateActionOnlyKeepsDueNextRun(t *testing.T) {
	p, store := addTestPlugin(t, false)
	due := time.Now().Add(-10 * time.Second).Unix()
	task := sampleTask("ct_due")
	task.NextRunAt = due
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.update(context.Background(), map[string]any{
		"task_id": task.ID,
		"action":  map[string]any{"type": ActionNotify, "param": "<b>新文案</b>"},
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := store.Get(task.ID)
	if got.NextRunAt != due {
		t.Fatalf("action-only 不得推进 next_run_at: %d → %d", due, got.NextRunAt)
	}
	changed := res.(map[string]any)["changed"].([]string)
	if len(changed) != 1 || changed[0] != "action" {
		t.Fatalf("changed=%v", changed)
	}
}

// 一次性 completed_once 显式改期 → 重新武装，并回显 enabled。
func TestUpdateRearmsCompletedOnce(t *testing.T) {
	p, store := addTestPlugin(t, false)
	task := sampleTask("ct_once")
	task.Cron = ""
	task.Once = true
	task.RunAt = time.Now().Add(-time.Hour).Unix()
	task.NextRunAt = 0
	task.Enabled = false
	task.DisabledReason = ReasonOnceCompleted
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.update(context.Background(), map[string]any{
		"task_id": task.ID,
		"delay":   "50m",
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := store.Get(task.ID)
	if !got.Enabled || got.DisabledReason != "" || !got.Once || got.NextRunAt != got.RunAt {
		t.Fatalf("重新武装不符: %+v", got)
	}
	changed := res.(map[string]any)["changed"].([]string)
	found := false
	for _, c := range changed {
		if c == "enabled" {
			found = true
		}
	}
	if !found {
		t.Fatalf("changed 应含 enabled: %v", changed)
	}
}
