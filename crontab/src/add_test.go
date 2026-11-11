package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// addTestPlugin 构造只带内存 store / 配置 / 假通知的插件（p=nil → 不推 UI 事件）。
func addTestPlugin(t *testing.T, toast bool) (*crontabPlugin, *Store) {
	t.Helper()
	store := newTestStore(t)
	cfg := testConfigManager()
	cfg.values[KeyCreateToast] = boolString(toast)
	return &crontabPlugin{store: store, cfg: cfg, notifier: &fakeNotifier{}}, store
}

func TestAddRejectsUnknownArgs(t *testing.T) {
	p, _ := addTestPlugin(t, false)
	_, err := p.add(context.Background(), map[string]any{
		"cron":   "*/5 * * * *",
		"name":   "夜间构建",
		"action": map[string]any{"type": "notify", "param": "<b>x</b>"},
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "不支持的参数") || !strings.Contains(err.Error(), "name") {
		t.Fatalf("未知参数应明确拒绝: %v", err)
	}
}

func TestAddRequiresWorkDirForRunAndAgent(t *testing.T) {
	p, store := addTestPlugin(t, false)
	for _, typ := range []string{ActionRun, ActionAgent} {
		param := map[string]any{"command": "echo hi"}
		if typ == ActionAgent {
			param = map[string]any{"prompt": "审查"}
		}
		_, err := p.add(context.Background(), map[string]any{
			"cron":   "*/5 * * * *",
			"action": map[string]any{"type": typ, "param": param},
		}, pluginsdk.CallContext{})
		if err == nil || !strings.Contains(err.Error(), "work_dir") {
			t.Fatalf("%s 空 work_dir 应拒绝: %v", typ, err)
		}
	}
	if n := len(store.List()); n != 0 {
		t.Fatalf("失败调用不得落库: %d", n)
	}
}

func TestAddNotifyWithoutWorkDirSucceeds(t *testing.T) {
	p, store := addTestPlugin(t, false)
	res, err := p.add(context.Background(), map[string]any{
		"delay":  "50m",
		"action": map[string]any{"type": "notify", "param": "<b>喝水</b>"},
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("notify 无需项目目录: %v", err)
	}
	out := res.(map[string]any)
	if out["ok"] != true {
		t.Fatalf("返回=%v", out)
	}
	tasks := store.List()
	if len(tasks) != 1 || tasks[0].CreatedBy.WorkDir != "" || tasks[0].Action.Type != ActionNotify {
		t.Fatalf("任务=%+v", tasks)
	}
	view := out["task"].(map[string]any)
	if view["schedule_type"] != "once" || view["run_at_local"] == nil || view["timezone"] == nil {
		t.Fatalf("一次性任务应返回本地时间字段: %v", view)
	}
}

func TestAddRunWithExistingWorkDirSucceeds(t *testing.T) {
	p, _ := addTestPlugin(t, false)
	dir := t.TempDir()
	res, err := p.add(context.Background(), map[string]any{
		"cron":   "*/5 * * * *",
		"action": map[string]any{"type": "run", "param": map[string]any{"command": "echo hi"}},
	}, pluginsdk.CallContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	view := res.(map[string]any)["task"].(map[string]any)
	if view["work_dir"] != dir || view["next_run_at_local"] == nil || view["timezone"] == nil {
		t.Fatalf("返回=%v", view)
	}
}

func TestAddRunRejectsMissingWorkDir(t *testing.T) {
	p, _ := addTestPlugin(t, false)
	dir := t.TempDir() + "/not-exists"
	_, err := p.add(context.Background(), map[string]any{
		"cron":   "*/5 * * * *",
		"action": map[string]any{"type": "run", "param": map[string]any{"command": "echo hi"}},
	}, pluginsdk.CallContext{WorkDir: dir})
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在目录应拒绝: %v", err)
	}
}

func TestWithJSONError(t *testing.T) {
	fn := withJSONError(func(context.Context, map[string]any, pluginsdk.CallContext) (any, error) {
		return nil, context.DeadlineExceeded
	})
	if _, err := fn(context.Background(), nil, pluginsdk.CallContext{}); err == nil {
		t.Fatal("应返回错误")
	} else {
		var body struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if jerr := json.Unmarshal([]byte(err.Error()), &body); jerr != nil {
			t.Fatalf("错误正文应为 JSON: %v (%q)", jerr, err.Error())
		}
		if body.OK || body.Error == "" {
			t.Fatalf("错误正文=%+v", body)
		}
	}
}

// notify 不访问工作目录：无项目目录时照常执行，不得被 work_dir_missing 停用。
func TestExecuteNotifyWithoutWorkDir(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	task := sampleTask("ct_global_notify")
	task.CreatedBy.WorkDir = ""
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	if len(fake.calls) != 1 {
		t.Fatalf("应弹一次通知，got %d", len(fake.calls))
	}
	got, _ := store.Get(task.ID)
	if !got.Enabled || got.DisabledReason != "" {
		t.Fatalf("notify 不应因空 work_dir 停用: %+v", got)
	}
	if got.LastRun == nil || !got.LastRun.OK {
		t.Fatalf("last_run=%+v", got.LastRun)
	}
}

// run 仍要求工作目录：不存在 → 停用并记 work_dir_missing。
func TestExecuteRunDisablesOnMissingWorkDir(t *testing.T) {
	fake := &fakeNotifier{}
	s, store := newTestScheduler(t, fake)
	task := Task{
		ID:          "ct_run_missing",
		Cron:        "*/5 * * * *",
		Action:      Action{Type: ActionRun, Param: json.RawMessage(`{"command":"echo hi"}`)},
		Description: "执行：echo hi",
		Enabled:     true,
		NextRunAt:   time.Now().Unix(),
	}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}

	s.execute(task)

	got, _ := store.Get(task.ID)
	if got.Enabled || got.DisabledReason != ReasonWorkDirMissing {
		t.Fatalf("run 无目录应停用: %+v", got)
	}
}

func TestTaskViewLocalFields(t *testing.T) {
	ts := int64(1761878700)
	v := taskView(Task{ID: "ct_tz", Cron: "* * * * *", NextRunAt: ts, Action: Action{Type: ActionNotify}})
	got, _ := v["next_run_at_local"].(string)
	if want := time.Unix(ts, 0).In(time.Local).Format(time.RFC3339); got != want {
		t.Fatalf("next_run_at_local=%q want %q", got, want)
	}
	if v["timezone"] == "" {
		t.Fatal("timezone 不能为空")
	}
}
