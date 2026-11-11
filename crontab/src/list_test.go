package main

// list_test.go —— crontab_list 的 limit / output_tail_bytes 上限。

import (
	"context"
	"strings"
	"testing"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func TestListLimitAndOutputTail(t *testing.T) {
	store := newTestStore(t)
	for i, id := range []string{"ct_a", "ct_b", "ct_c"} {
		task := sampleTask(id)
		task.CreatedAt = int64(1761878400 + i)
		task.LastRun = &RunRecord{TaskID: id, At: task.CreatedAt, OK: true, OutputTail: strings.Repeat("x", 1000)}
		if err := store.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	p := &crontabPlugin{store: store}

	res, err := p.list(context.Background(), map[string]any{
		"limit": 1, "output_tail_bytes": 16,
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := res.(map[string]any)
	if out["count"] != 1 || out["truncated"] != true || out["total"] != 3 {
		t.Fatalf("limit 语义不符: %v", out)
	}
	tasks := out["tasks"].([]map[string]any)
	rec := tasks[0]["last_run"].(RunRecord)
	if len(rec.OutputTail) > 16 {
		t.Fatalf("output_tail 未截断: %d", len(rec.OutputTail))
	}

	// 0 = 不返回 output_tail。
	res, err = p.list(context.Background(), map[string]any{
		"output_tail_bytes": 0,
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out = res.(map[string]any)
	for _, task := range out["tasks"].([]map[string]any) {
		if rec := task["last_run"].(RunRecord); rec.OutputTail != "" {
			t.Fatalf("output_tail_bytes=0 仍返回 tail: %q", rec.OutputTail)
		}
	}
}

func newListPlugin(t *testing.T) (*crontabPlugin, *Store) {
	t.Helper()
	store := newTestStore(t)
	for i, id := range []string{"ct_a", "ct_b", "ct_c"} {
		task := sampleTask(id)
		task.CreatedAt = int64(1761878400 + i)
		if err := store.Add(task); err != nil {
			t.Fatal(err)
		}
	}
	return &crontabPlugin{store: store}, store
}

func taskIDs(tasks []map[string]any) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task["id"].(string))
	}
	return out
}

func TestListOrderAndTaskID(t *testing.T) {
	p, _ := newListPlugin(t)

	// 默认 desc：新任务优先。
	res, err := p.list(context.Background(), map[string]any{}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out := res.(map[string]any)
	if got := taskIDs(out["tasks"].([]map[string]any)); strings.Join(got, ",") != "ct_c,ct_b,ct_a" {
		t.Fatalf("默认顺序=%v", got)
	}
	if out["order"] != "desc" || out["count"] != 3 || out["total"] != 3 || out["truncated"] != false {
		t.Fatalf("返回=%v", out)
	}

	// asc + limit：取最早的两条，total 仍是 3。
	res, err = p.list(context.Background(), map[string]any{"order": "asc", "limit": 2}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out = res.(map[string]any)
	if got := taskIDs(out["tasks"].([]map[string]any)); strings.Join(got, ",") != "ct_a,ct_b" {
		t.Fatalf("asc 顺序=%v", got)
	}
	if out["truncated"] != true || out["total"] != 3 || out["count"] != 2 {
		t.Fatalf("分页字段=%v", out)
	}

	// offset 在过滤/排序后生效。
	res, err = p.list(context.Background(), map[string]any{"order": "asc", "offset": 1, "limit": 1}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out = res.(map[string]any)
	if got := taskIDs(out["tasks"].([]map[string]any)); strings.Join(got, ",") != "ct_b" {
		t.Fatalf("offset 结果=%v", got)
	}
	if out["offset"] != 1 || out["truncated"] != true {
		t.Fatalf("offset 回显=%v", out)
	}
	res, err = p.list(context.Background(), map[string]any{"offset": 99}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out = res.(map[string]any)
	if out["count"] != 0 || out["truncated"] != false {
		t.Fatalf("offset 超界=%v", out)
	}

	// task_id 精确查询；未命中返回空数组而非错误。
	res, err = p.list(context.Background(), map[string]any{"task_id": "ct_b"}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out = res.(map[string]any)
	if got := taskIDs(out["tasks"].([]map[string]any)); len(got) != 1 || got[0] != "ct_b" {
		t.Fatalf("task_id 过滤=%v", got)
	}
	if out["task_id"] != "ct_b" || out["total"] != 1 {
		t.Fatalf("返回=%v", out)
	}
	res, err = p.list(context.Background(), map[string]any{"task_id": "ct_nope"}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out = res.(map[string]any)
	if out["count"] != 0 || out["total"] != 0 || len(out["tasks"].([]map[string]any)) != 0 {
		t.Fatalf("未命中应返回空列表: %v", out)
	}
}

func TestListRejectsInvalidArgs(t *testing.T) {
	p, _ := newListPlugin(t)
	cases := map[string]map[string]any{
		"enabled 非布尔":         {"enabled": 1},
		"limit 非整数":           {"limit": "abc"},
		"limit 小于 1":          {"limit": 0},
		"offset 负数":           {"offset": -1},
		"output_tail_bytes 负": {"output_tail_bytes": -1},
		"order 非法":            {"order": "up"},
		"task_id 空串":          {"task_id": ""},
		"未知参数":                {"name": "x"},
	}
	for name, args := range cases {
		if _, err := p.list(context.Background(), args, pluginsdk.CallContext{}); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestListClampsAndReportsEffectiveLimits(t *testing.T) {
	p, store := newListPlugin(t)
	task := sampleTask("ct_big")
	task.CreatedAt = int64(1761878500)
	task.LastRun = &RunRecord{TaskID: task.ID, At: task.CreatedAt, OK: true, OutputTail: strings.Repeat("y", 10000)}
	if err := store.Add(task); err != nil {
		t.Fatal(err)
	}
	res, err := p.list(context.Background(), map[string]any{
		"limit": 999, "output_tail_bytes": 9999,
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	out := res.(map[string]any)
	if out["limit"] != 200 || out["output_tail_bytes"] != outputTailMax {
		t.Fatalf("未回显有效上限: %v", out)
	}
	for _, view := range out["tasks"].([]map[string]any) {
		if rec, ok := view["last_run"].(RunRecord); ok && len(rec.OutputTail) > outputTailMax {
			t.Fatalf("output_tail 超上限: %d", len(rec.OutputTail))
		}
	}
}
