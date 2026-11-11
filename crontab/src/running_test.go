package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// taskView 暴露 running，供 list/update 等工具判断是否有在飞执行。
func TestTaskViewRunning(t *testing.T) {
	if v := taskView(Task{RunningSince: 1}); v["running"] != true {
		t.Fatalf("running=%v", v["running"])
	}
	if v := taskView(Task{}); v["running"] != false {
		t.Fatalf("running=%v", v["running"])
	}
}

// update 只影响后续触发：在飞执行不被取消，running 保持 true。
func TestUpdateWhileRunningKeepsExecution(t *testing.T) {
	p, store := addTestPlugin(t, false)
	runningSince := time.Now().Add(-5 * time.Second).Unix()
	task := sampleTask("ct_upd_running")
	task.RunningSince = runningSince
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
	if got.RunningSince != runningSince {
		t.Fatalf("update 不得取消/清空在飞执行: %d → %d", runningSince, got.RunningSince)
	}
	view := res.(map[string]any)["task"].(map[string]any)
	if view["running"] != true {
		t.Fatalf("返回应标记 running: %v", view)
	}
}

// 旧的一次性执行收口时，不能覆盖运行期间改期/改成 cron 的新定义。
func TestExecuteOnceCompletionDoesNotClobberUpdatedTask(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	old := Task{
		ID:           "ct_once_upd",
		Once:         true,
		RunAt:        time.Now().Add(-time.Minute).Unix(),
		Action:       Action{Type: ActionNotify, Param: json.RawMessage(`"<b>x</b>"`)},
		Description:  "通知：x",
		Enabled:      true,
		NextRunAt:    time.Now().Unix(),
		RunningSince: time.Now().Unix(),
	}
	if err := store.Add(old); err != nil {
		t.Fatal(err)
	}
	// 模拟运行期间 update：一次性 → 周期，next 指向未来。
	newNext := time.Now().Add(time.Hour).Unix()
	if err := store.Update(old.ID, func(cur *Task) error {
		cur.Once = false
		cur.RunAt = 0
		cur.Cron = "*/5 * * * *"
		cur.NextRunAt = newNext
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	s.execute(old)

	got, _ := store.Get(old.ID)
	if !got.Enabled || got.Once || got.NextRunAt != newNext || got.DisabledReason != "" {
		t.Fatalf("旧执行收口覆盖了新定义: %+v", got)
	}
	if got.RunningSince != 0 {
		t.Fatalf("运行标记应清除: %d", got.RunningSince)
	}
}

// 一次性任务运行期间改期（仍是 once）→ 旧执行收口不得停用新调度。
func TestExecuteOnceCompletionDoesNotClobberRearmedOnce(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	old := Task{
		ID:           "ct_once_rearm",
		Once:         true,
		RunAt:        time.Now().Add(-time.Minute).Unix(),
		Action:       Action{Type: ActionNotify, Param: json.RawMessage(`"<b>x</b>"`)},
		Description:  "通知：x",
		Enabled:      true,
		NextRunAt:    time.Now().Unix(),
		RunningSince: time.Now().Unix(),
	}
	if err := store.Add(old); err != nil {
		t.Fatal(err)
	}
	newRunAt := time.Now().Add(2 * time.Hour).Unix()
	if err := store.Update(old.ID, func(cur *Task) error {
		cur.RunAt = newRunAt
		cur.NextRunAt = newRunAt
		cur.Enabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	s.execute(old)

	got, _ := store.Get(old.ID)
	if !got.Enabled || !got.Once || got.RunAt != newRunAt || got.NextRunAt != newRunAt || got.DisabledReason != "" {
		t.Fatalf("旧执行收口覆盖了新调度: %+v", got)
	}
}
