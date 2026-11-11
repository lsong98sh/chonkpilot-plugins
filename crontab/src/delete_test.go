package main

import (
	"context"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func TestDeleteReturnsRemainingAndRunning(t *testing.T) {
	p, store := addTestPlugin(t, false)
	keep := sampleTask("ct_keep")
	if err := store.Add(keep); err != nil {
		t.Fatal(err)
	}
	running := sampleTask("ct_running")
	running.RunningSince = time.Now().Add(-5 * time.Second).Unix()
	if err := store.Add(running); err != nil {
		t.Fatal(err)
	}

	res, err := p.delete(context.Background(), map[string]any{"task_id": running.ID}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	out := res.(map[string]any)
	if out["deleted"] != true || out["remaining"] != 1 || out["count"] != 1 || out["running"] != true {
		t.Fatalf("返回=%v", out)
	}
	if _, ok := store.Get(running.ID); ok {
		t.Fatal("任务应从 store 移除")
	}
	if _, ok := store.Get(keep.ID); !ok {
		t.Fatal("其它任务不应受影响")
	}
}

func TestDeleteMissingAndInvalidArgs(t *testing.T) {
	p, _ := addTestPlugin(t, false)
	for name, args := range map[string]map[string]any{
		"任务不存在":      {"task_id": "ct_nope"},
		"task_id 空":  {"task_id": "  "},
		"缺少 task_id": {},
		"未知参数":       {"task_id": "ct_x", "force": true},
	} {
		if _, err := p.delete(context.Background(), args, pluginsdk.CallContext{}); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
	_, err := p.delete(context.Background(), map[string]any{"task_id": "ct_nope"}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "任务不存在") {
		t.Fatalf("错误文案=%v", err)
	}
}
