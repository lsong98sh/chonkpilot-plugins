// session_test.go —— 定时任务会话归属（SANDBOX-WRITE-1 P4 / C11）的 L1 用例。
//
// 覆盖：根会话 / 子会话派生与层级（kind / parent / origin）、每轮一个 turn、收轮终态、
// 归属失败**不阻断执行**（RunRecord.Detail 留痕）、调度器把会话归属写回任务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeSessions 是 sessionHost 的记录型假实现。
type fakeSessions struct {
	ensures   []sessionEnsureReq
	turns     []sessionRef
	completes []completion

	ensureErr error
	turnErr   error
	dialogID  string
}

type completion struct {
	ref    sessionRef
	status string
	reason string
}

func (f *fakeSessions) EnsureSession(_ context.Context, req sessionEnsureReq) (string, error) {
	f.ensures = append(f.ensures, req)
	if f.ensureErr != nil {
		return "", f.ensureErr
	}
	if req.DialogID != "" {
		return req.DialogID, nil
	}
	if f.dialogID != "" {
		return f.dialogID, nil
	}
	return "dlg-host", nil
}

func (f *fakeSessions) EnsureTurn(_ context.Context, _ string, ref sessionRef) error {
	f.turns = append(f.turns, ref)
	return f.turnErr
}

func (f *fakeSessions) CompleteTurn(_ context.Context, _ string, ref sessionRef, status, reason string) error {
	f.completes = append(f.completes, completion{ref: ref, status: status, reason: reason})
	return nil
}

// TestRootSessionIDDeterministic：根会话 id 由 work_dir 确定性派生（同项目恒同 id）。
func TestRootSessionIDDeterministic(t *testing.T) {
	a := rootSessionID(`E:\proj\demo`)
	if a != rootSessionID(`e:/proj/demo`) {
		t.Fatalf("同目录不同写法应得同一 id: %q", a)
	}
	if a == rootSessionID(`E:\proj\other`) {
		t.Fatal("不同项目不应共用根会话 id")
	}
	if !strings.HasPrefix(a, "crontab-") || len(a) != len("crontab-")+8 {
		t.Fatalf("根会话 id 形状应为 crontab-xxxxxxxx: %q", a)
	}
	if got := taskSessionID("ct_ab12"); got != "crontab-ct_ab12" {
		t.Fatalf("任务子会话 id=%q", got)
	}
}

// TestBeginCronRoundCreatesHierarchy：根会话（kind=crontab）→ 子会话（kind=crontab_task，
// parent=根会话）→ 本轮 turn。
func TestBeginCronRoundCreatesHierarchy(t *testing.T) {
	fake := &fakeSessions{}
	r := &runner{sessions: fake, logf: t.Logf}
	wd := t.TempDir()
	task := Task{ID: "ct_hier", Description: "执行：echo hi", CreatedBy: CreatedBy{WorkDir: wd}}

	ref, detail := r.beginCronRound(context.Background(), task)
	if detail != "" {
		t.Fatalf("归属应成功: %q", detail)
	}
	root := rootSessionID(wd)
	if len(fake.ensures) != 2 {
		t.Fatalf("应建根会话 + 子会话: %+v", fake.ensures)
	}
	if e := fake.ensures[0]; e.SessionID != root || e.Kind != sessionKindCrontab ||
		e.Title != cronDialogTitle || e.ParentSessionID != "" || e.DialogID != "" {
		t.Fatalf("根会话请求不符: %+v", e)
	}
	if e := fake.ensures[1]; e.SessionID != "crontab-ct_hier" || e.ParentSessionID != root ||
		e.Kind != sessionKindCrontabTask || e.DialogID != "dlg-host" || e.Title != "执行：echo hi" {
		t.Fatalf("子会话请求不符: %+v", e)
	}
	if ref.SessionID != "crontab-ct_hier" || ref.DialogID != "dlg-host" || ref.TurnID == "" {
		t.Fatalf("会话引用不符: %+v", ref)
	}
	if len(fake.turns) != 1 || fake.turns[0].TurnID != ref.TurnID {
		t.Fatalf("每轮应建一个 turn: %+v", fake.turns)
	}
}

// TestRunCommandBindsSessionAndCompletesTurn：run 动作执行前挂载、执行后收轮（done）。
func TestRunCommandBindsSessionAndCompletesTurn(t *testing.T) {
	fake := &fakeSessions{}
	r := &runner{
		cfg:      &configManager{values: map[string]string{}, loaded: true},
		notifier: &fakeNotifier{},
		logf:     t.Logf,
		sessions: fake,
	}
	task := Task{ID: "ct_run", Description: "执行：echo ok", CreatedBy: CreatedBy{WorkDir: t.TempDir()}}
	spec := &ActionSpec{Type: ActionRun, Run: &RunParam{Command: "echo ok"}}

	rec := r.runCommand(context.Background(), task, spec, time.Now().Unix())
	if !rec.OK {
		t.Fatalf("命令应执行成功: %+v", rec)
	}
	if rec.SessionID != "crontab-ct_run" || rec.DialogID != "dlg-host" || rec.TurnID == "" {
		t.Fatalf("运行记录应带会话归属: %+v", rec)
	}
	if len(fake.completes) != 1 || fake.completes[0].status != "done" || fake.completes[0].ref.TurnID != rec.TurnID {
		t.Fatalf("应收轮 done: %+v", fake.completes)
	}
}

// TestRunCommandSessionFailureDoesNotBlock：归属失败 → 命令照常执行，Detail 留痕。
func TestRunCommandSessionFailureDoesNotBlock(t *testing.T) {
	fake := &fakeSessions{ensureErr: errors.New("instance_not_found")}
	r := &runner{
		cfg:      &configManager{values: map[string]string{}, loaded: true},
		notifier: &fakeNotifier{},
		logf:     t.Logf,
		sessions: fake,
	}
	task := Task{ID: "ct_nofail", Description: "执行：echo ok", CreatedBy: CreatedBy{WorkDir: t.TempDir()}}
	spec := &ActionSpec{Type: ActionRun, Run: &RunParam{Command: "echo ok"}}

	rec := r.runCommand(context.Background(), task, spec, time.Now().Unix())
	if !rec.OK || rec.OutputTail == "" {
		t.Fatalf("归属失败不得阻断命令执行: %+v", rec)
	}
	if !strings.Contains(rec.Detail, "session_bind_failed") || rec.TurnID != "" {
		t.Fatalf("应留痕且不记 turn: %+v", rec)
	}
	if len(fake.completes) != 0 {
		t.Fatalf("未归属不应收轮: %+v", fake.completes)
	}
}

// TestOpenableSessions（G-99）：任务详情页「可打开会话」列表 = 该任务的子会话；
// 未执行过（无 session_id / dialog_id）→ 空。
func TestOpenableSessions(t *testing.T) {
	if got := openableSessions(Task{ID: "ct_none"}); len(got) != 0 {
		t.Fatalf("未执行过不应有可打开会话: %+v", got)
	}
	task := Task{
		ID: "ct_open", Description: "执行：echo hi",
		SessionID: "crontab-ct_open", DialogID: "dlg-1",
	}
	got := openableSessions(task)
	if len(got) != 1 {
		t.Fatalf("应有且仅有 1 个可打开会话: %+v", got)
	}
	if got[0]["session_id"] != "crontab-ct_open" || got[0]["dialog_id"] != "dlg-1" ||
		got[0]["title"] != "执行：echo hi" {
		t.Fatalf("可打开会话字段不符: %+v", got[0])
	}
}

// TestExecutePersistsSessionRefs：调度器执行后把子会话 / 对话写回任务（详情页展示用）。
func TestExecutePersistsSessionRefs(t *testing.T) {
	fake := &fakeSessions{}
	s, store := newTestScheduler(t, &fakeNotifier{})
	s.runner.sessions = fake
	s.ctx = context.Background() // 直接驱动 execute（未 Start 时 ctx 为空）
	task := sampleTask("ct_persist")
	task.Action = Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi"}`)}
	task.CreatedBy.WorkDir = t.TempDir()
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	got, _ := store.Get("ct_persist")
	if got.SessionID != "crontab-ct_persist" || got.DialogID != "dlg-host" {
		t.Fatalf("任务应记录会话归属: session=%q dialog=%q", got.SessionID, got.DialogID)
	}
	if got.LastRun == nil || got.LastRun.TurnID == "" {
		t.Fatalf("最近运行应带 turn id: %+v", got.LastRun)
	}
}
