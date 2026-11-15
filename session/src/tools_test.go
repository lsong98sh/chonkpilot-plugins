// tools_test.go —— session 插件的契约与适配层测试。
//
// 覆盖：
//   - 4 个只读工具契约齐全、均 hot、schema/description 可用；
//   - handler 只做适配：上下文（work_dir/instance/session/top_session）注入、
//     参数按白名单透传、view 固定；
//   - `session_id` 缺省时**不写**该键（搜索 = 全部会话；get/history = 由宿主解析为本会话）；
//   - 宿主桥错误原样透出（稳定错误码不被吞）。
package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// fakeCaller 记录一次宿主能力调用并返回固定应答 / 错误。
type fakeCaller struct {
	method  string
	timeout time.Duration
	args    map[string]any
	reply   map[string]any
	err     error
}

func (f *fakeCaller) CallIntoWithTimeout(_ context.Context, d time.Duration, method string, args, out any) error {
	f.method = method
	f.timeout = d
	if m, ok := args.(map[string]any); ok {
		f.args = m
	}
	if f.err != nil {
		return f.err
	}
	if out != nil && f.reply != nil {
		*(out.(*map[string]any)) = f.reply
	}
	return nil
}

// seqCaller 按方法返回应答并记录调用序列（rename 需要先 read 反查 dialog_id）。
type seqCaller struct {
	methods []string
	args    []map[string]any
	replies map[string]map[string]any
}

func (f *seqCaller) CallIntoWithTimeout(_ context.Context, _ time.Duration, method string, args, out any) error {
	f.methods = append(f.methods, method)
	m, _ := args.(map[string]any)
	f.args = append(f.args, m)
	if out != nil && f.replies[method] != nil {
		*(out.(*map[string]any)) = f.replies[method]
	}
	return nil
}

func testCx() pluginsdk.CallContext {
	return pluginsdk.CallContext{
		InstanceID: "ins-1", WorkDir: `E:\proj\a`,
		Session: "s-self", TopSession: "s-top", Turn: "t-1",
	}
}

func TestContractsCoverAllTools(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	got := make([]string, 0, len(tools))
	for _, tool := range tools {
		got = append(got, tool.Name)
		if !tool.Hot {
			t.Fatalf("%s 应为 hot（LLM 可见）", tool.Name)
		}
		if tool.Schema == nil || tool.Schema["properties"] == nil {
			t.Fatalf("%s 缺少参数 schema", tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("%s 缺少 description", tool.Name)
		}
	}
	sort.Strings(got)
	want := []string{
		"session_create", "session_get", "session_history", "session_list",
		"session_open", "session_rename", "session_search", "session_send",
		"session_status",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("契约工具集 = %v, want %v", got, want)
	}
}

// TestSearchForwardsContextAndOmitsMissingSession：search 注入上下文；缺省 session_id 不写键。
func TestSearchForwardsContextAndOmitsMissingSession(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"hits": []any{}, "total": 0}}
	p := &sessionPlugin{call: f}
	out, err := p.search(context.Background(), map[string]any{
		"query": "交付时间", "targets": []any{"bob"}, "limit": float64(5),
	}, testCx())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if f.method != "plugin/v1/session.search" {
		t.Fatalf("method=%q", f.method)
	}
	if f.timeout != queryTimeout {
		t.Fatalf("timeout=%v, want %v", f.timeout, queryTimeout)
	}
	for k, want := range map[string]string{
		"work_dir": `E:\proj\a`, "instance_id": "ins-1", "session": "s-self", "top_session": "s-top",
	} {
		if got := f.args[k]; got != want {
			t.Fatalf("args[%s]=%v, want %v", k, got, want)
		}
	}
	if _, ok := f.args["session_id"]; ok {
		t.Fatalf("缺省 session_id 不应写键（否则搜索被错误限定）: %+v", f.args)
	}
	if out == nil {
		t.Fatalf("结果不应为 nil")
	}
}

// TestGetAndHistoryPinView：get/history 固定 view，且透传 before_turn_id / 分页参数。
func TestGetAndHistoryPinView(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"session": map[string]any{}}}
	p := &sessionPlugin{call: f}

	if _, err := p.get(context.Background(), map[string]any{"session_id": "@top"}, testCx()); err != nil {
		t.Fatalf("get: %v", err)
	}
	if f.method != "plugin/v1/session.read" || f.args["view"] != "meta" || f.args["session_id"] != "@top" {
		t.Fatalf("get 适配异常: method=%q args=%+v", f.method, f.args)
	}
	if f.timeout != metaTimeout {
		t.Fatalf("get timeout=%v", f.timeout)
	}

	f.args = nil
	if _, err := p.history(context.Background(), map[string]any{
		"session_id": "s-1", "before_turn_id": "t-9", "target_messages": float64(20),
	}, testCx()); err != nil {
		t.Fatalf("history: %v", err)
	}
	if f.args["view"] != "history" || f.args["before_turn_id"] != "t-9" || f.args["target_messages"] != float64(20) {
		t.Fatalf("history 适配异常: %+v", f.args)
	}
}

// TestListForwardsIncludeSub：list 透传 include_sub / 分页，并注入上下文。
func TestListForwardsIncludeSub(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"sessions": []any{}}}
	p := &sessionPlugin{call: f}
	if _, err := p.list(context.Background(), map[string]any{"include_sub": true, "limit": float64(10)}, testCx()); err != nil {
		t.Fatalf("list: %v", err)
	}
	if f.method != "plugin/v1/session.browse" || f.args["include_sub"] != true || f.args["limit"] != float64(10) {
		t.Fatalf("list 适配异常: method=%q args=%+v", f.method, f.args)
	}
}

// TestCreateForwardsRunContext：create → session.run，注入 turn/tool_call_id/display_name，
// 不写 session_id（宿主据此新建顶层会话），wait_sec 透传且桥超时按 wait 计算。
func TestCreateForwardsRunContext(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"state": "running", "session_id": "split-x", "task_id": "tk-1"}}
	p := &sessionPlugin{call: f}
	cx := testCx()
	cx.ToolCallID = "tc-create"
	cx.DisplayName = "分解任务"
	_, err := p.create(context.Background(), map[string]any{
		"prompt": "处理任务 A", "title": "任务 A", "wait_sec": float64(30),
	}, cx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if f.method != "plugin/v1/session.run" || f.args["wait_sec"] != 30 {
		t.Fatalf("create 适配异常: method=%q args=%+v", f.method, f.args)
	}
	for k, want := range map[string]string{
		"turn": "t-1", "tool_call_id": "tc-create", "display_name": "分解任务",
		"work_dir": `E:\proj\a`, "session": "s-self", "top_session": "s-top",
	} {
		if got := f.args[k]; got != want {
			t.Fatalf("args[%s]=%v, want %v", k, got, want)
		}
	}
	if _, ok := f.args["session_id"]; ok {
		t.Fatalf("create 不应传 session_id（否则变成续跑）: %+v", f.args)
	}
	if f.timeout != 45*time.Second {
		t.Fatalf("timeout=%v, want 45s", f.timeout)
	}
}

// TestStatusForwardsPersistentQuery：status → session.status，wait_sec 透传且桥超时按等待计算。
func TestStatusForwardsPersistentQuery(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"state": "done", "text": "最终回复"}}
	p := &sessionPlugin{call: f}
	if _, err := p.status(context.Background(), map[string]any{
		"session_id": "split-1", "turn_id": "t-1", "wait_sec": float64(30),
	}, testCx()); err != nil {
		t.Fatalf("status: %v", err)
	}
	if f.method != "plugin/v1/session.status" {
		t.Fatalf("method=%q", f.method)
	}
	if f.args["session_id"] != "split-1" || f.args["turn_id"] != "t-1" || f.args["wait_sec"] != 30 {
		t.Fatalf("status 适配异常: %+v", f.args)
	}
	if f.timeout != 45*time.Second {
		t.Fatalf("timeout=%v, want 45s", f.timeout)
	}
}

// TestSendResolvesTarget：send 必须解析出目标会话（省略/@top → top_session；@self → session）。
func TestSendResolvesTarget(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "s-top"},
		{"@top", "s-top"},
		{"@self", "s-self"},
		{"s-explicit", "s-explicit"},
	}
	for _, tc := range cases {
		f := &fakeCaller{reply: map[string]any{"state": "running"}}
		p := &sessionPlugin{call: f}
		if _, err := p.send(context.Background(), map[string]any{
			"session_id": tc.in, "prompt": "继续",
		}, testCx()); err != nil {
			t.Fatalf("send(%v): %v", tc.in, err)
		}
		if f.method != "plugin/v1/session.run" || f.args["session_id"] != tc.want {
			t.Fatalf("send(%v) → %q (method %q)", tc.in, f.args["session_id"], f.method)
		}
	}
}

// TestRenameResolvesOwnTargetAndDialog：rename 省略 = 执行会话；@top = 顶层；title 去空白；
// 先 read(view=meta) 反查 dialog_id，再带 `id` + `dialog_id` 提交（一树一库路由要求）。
func TestRenameResolvesOwnTargetAndDialog(t *testing.T) {
	f := &seqCaller{replies: map[string]map[string]any{
		"plugin/v1/session.read":   {"session": map[string]any{"dialog_id": "d-9"}},
		"plugin/v1/session.rename": {"accepted": true},
	}}
	p := &sessionPlugin{call: f}
	if _, err := p.rename(context.Background(), map[string]any{"title": "  新名字  "}, testCx()); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(f.methods) != 2 || f.methods[0] != "plugin/v1/session.read" || f.methods[1] != "plugin/v1/session.rename" {
		t.Fatalf("rename 调用序列异常: %v", f.methods)
	}
	if f.args[0]["session_id"] != "s-self" || f.args[0]["view"] != "meta" {
		t.Fatalf("read 入参异常: %+v", f.args[0])
	}
	ren := f.args[1]
	if ren["session_id"] != "s-self" || ren["id"] != "s-self" || ren["dialog_id"] != "d-9" || ren["title"] != "新名字" {
		t.Fatalf("rename 适配异常: %+v", ren)
	}
	if _, err := p.rename(context.Background(), map[string]any{"session_id": "@top", "title": "x"}, testCx()); err != nil {
		t.Fatalf("rename @top: %v", err)
	}
	if f.args[3]["session_id"] != "s-top" || f.args[3]["id"] != "s-top" {
		t.Fatalf("@top 应解析为 top_session: %+v", f.args[3])
	}
}

// TestOpenForwardsSession：open 透传 session_id（宿主解析 @top / 省略）。
func TestOpenForwardsSession(t *testing.T) {
	f := &fakeCaller{reply: map[string]any{"window_id": "w-1"}}
	p := &sessionPlugin{call: f}
	if _, err := p.open(context.Background(), map[string]any{"session_id": "@top"}, testCx()); err != nil {
		t.Fatalf("open: %v", err)
	}
	if f.method != "plugin/v1/session.open" || f.args["session_id"] != "@top" || f.args["top_session"] != "s-top" {
		t.Fatalf("open 适配异常: method=%q args=%+v", f.method, f.args)
	}
}

// TestBoundedIntRejectsOutOfRange：wait_sec 越界报错而非静默夹取。
func TestBoundedIntRejectsOutOfRange(t *testing.T) {
	if _, err := boundedInt(map[string]any{"wait_sec": float64(601)}, "wait_sec", 0, 600); err == nil {
		t.Fatal("601 应报错")
	}
	if _, err := boundedInt(map[string]any{"wait_sec": float64(-1)}, "wait_sec", 0, 600); err == nil {
		t.Fatal("-1 应报错")
	}
}

// TestBridgeErrorPropagates：宿主错误原样返回（调用方可识别 permission_denied 等稳定码）。
func TestBridgeErrorPropagates(t *testing.T) {
	sentinel := errors.New("permission_denied: 跨用户访问未开启")
	f := &fakeCaller{err: sentinel}
	p := &sessionPlugin{call: f}
	_, err := p.search(context.Background(), map[string]any{"query": "x", "targets": []any{"bob"}}, testCx())
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误未透出: %v", err)
	}
}

// TestBaseArgsEmptyContext：无注入上下文时不写空键（避免宿主收到空串 work_dir）。
func TestBaseArgsEmptyContext(t *testing.T) {
	in := baseArgs(pluginsdk.CallContext{})
	if len(in) != 0 {
		t.Fatalf("空上下文不应产生键: %+v", in)
	}
}
