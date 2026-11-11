package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/scriptrun"
)

// TestContractsCoverAllTools 校验 embed 的工具契约：八个工具齐全、均标 hot、schema/description 可用。
// （2026-10-28 P2-3：契约改 `*.tool.md` + go:embed，hot 注册与合并前一致。
// 2026-11-08：ask_user / next_run / subchat_open / subchat_finish 自查户进程内迁入。）
func TestContractsCoverAllTools(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	got := make([]string, 0, len(tools))
	for _, tool := range tools {
		got = append(got, tool.Name)
		if !tool.Hot {
			t.Fatalf("%s 应为 hot（hot_tools 不变）", tool.Name)
		}
		if tool.Schema == nil || tool.Schema["properties"] == nil {
			t.Fatalf("%s 缺少参数 schema", tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("%s 缺少 description（# 摘要 首段）", tool.Name)
		}
	}
	sort.Strings(got)
	want := []string{"ask_user", "next_run", "subchat_finish", "subchat_open", "task_llm", "task_run", "task_status", "task_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("契约工具集 = %v, want %v", got, want)
	}
}

// TestTaskRunContractShape task_run 契约必须同时暴露 command / file（二选一）与 runtime 等参数。
func TestTaskRunContractShape(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	var props map[string]any
	for _, tool := range tools {
		if tool.Name == "task_run" {
			props, _ = tool.Schema["properties"].(map[string]any)
		}
	}
	if props == nil {
		t.Fatal("未找到 task_run 契约")
	}
	for _, k := range []string{"command", "file", "runtime", "requires", "interpreter", "cwd", "timeoutSec", "maxOutputKB", "env", "args"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("task_run 契约缺少参数 %s", k)
		}
	}
	if _, ok := props["workdir"]; ok {
		t.Fatal("对外参数名应为 cwd（workdir 为内部名）")
	}
}

// testRunPlugin 构造最小可用的 taskPlugin（内存配置 + 默认值），用于 task_run 端到端用例。
func testRunPlugin(t *testing.T) *taskPlugin {
	t.Helper()
	m := &configManager{store: newMemStore(nil), logf: t.Logf}
	if err := m.Init(context.Background()); err != nil {
		t.Fatalf("config Init: %v", err)
	}
	return &taskPlugin{cfg: m}
}

// TestTaskRunOutputFile（2026-11-09）：输出超过 maxOutputKB 时完整输出落盘，
// 返回 output_file / total_bytes / output_bytes / truncated，预览不超过上限。
func TestTaskRunOutputFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("用例使用 Windows cmd 循环")
	}
	workDir := t.TempDir()
	p := testRunPlugin(t)
	out, err := p.run(context.Background(), map[string]any{
		"runtime":     "cmd",
		"command":     `for /L %i in (1,1,300) do @echo 0123456789012345678901234567890123456789`,
		"maxOutputKB": 1,
	}, pluginsdk.CallContext{WorkDir: workDir})
	if err != nil {
		t.Fatalf("task_run: %v", err)
	}
	res, ok := out.(scriptrun.Result)
	if !ok {
		t.Fatalf("返回类型异常：%T", out)
	}
	if !res.OK || !res.Truncated {
		t.Fatalf("应成功且 truncated=true，got %+v", res)
	}
	if res.TotalBytes <= 1024 {
		t.Fatalf("total_bytes 应 > 1KB，got %d", res.TotalBytes)
	}
	if res.OutputBytes <= 0 || res.OutputBytes > 1024 {
		t.Fatalf("预览字节应在 1..1024，got %d", res.OutputBytes)
	}
	if res.OutputFile == "" {
		t.Fatal("应返回 output_file")
	}
	wantDir := filepath.Join(workDir, ".chonkpilot", "tmp")
	if filepath.Dir(res.OutputFile) != wantDir {
		t.Fatalf("output_file 目录 = %s, want %s", filepath.Dir(res.OutputFile), wantDir)
	}
	base := filepath.Base(res.OutputFile)
	if !strings.HasPrefix(base, "task-run-") || !strings.HasSuffix(base, ".log") {
		t.Fatalf("文件名应匹配 task-run-*.log：%s", base)
	}
	content, rerr := os.ReadFile(res.OutputFile)
	if rerr != nil {
		t.Fatalf("读取 output_file: %v", rerr)
	}
	if int64(len(content)) != res.TotalBytes {
		t.Fatalf("落盘内容长度 = %d, want total_bytes %d", len(content), res.TotalBytes)
	}
	if !strings.Contains(res.Output, "完整输出见") {
		t.Fatalf("截断预览应附落盘提示，got %q", res.Output)
	}
}

// TestTaskRunExit5NotElevation（2026-11-09）：运行期 exit 5 是普通失败，不提示管理员权限。
func TestTaskRunExit5NotElevation(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("用例使用 Windows cmd 的 exit 5")
	}
	p := testRunPlugin(t)
	out, err := p.run(context.Background(), map[string]any{
		"runtime": "cmd",
		"command": "exit 5",
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("task_run: %v", err)
	}
	res := out.(scriptrun.Result)
	if res.OK {
		t.Fatalf("exit 5 应 ok=false，got %+v", res)
	}
	if res.ExitCode != 5 {
		t.Fatalf("exit_code = %d, want 5", res.ExitCode)
	}
	if strings.Contains(res.Error, "管理员权限") || strings.Contains(res.Error, "PowerShell") {
		t.Fatalf("exit 5 不应误判为提权：%q", res.Error)
	}
}

// TestTaskRunShellFileDispatch（2026-11-09）：Windows runtime=shell + file(.cmd) 走 cmd /c。
func TestTaskRunShellFileDispatch(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 的 shell + file 分派")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "dispatch.cmd")
	if err := os.WriteFile(file, []byte("@echo dispatch-ok\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := testRunPlugin(t)
	out, err := p.run(context.Background(), map[string]any{
		"runtime": "shell",
		"file":    file,
	}, pluginsdk.CallContext{WorkDir: dir})
	if err != nil {
		t.Fatalf("task_run: %v", err)
	}
	res := out.(scriptrun.Result)
	if !res.OK || !strings.Contains(res.Output, "dispatch-ok") {
		t.Fatalf(".cmd 应通过 cmd /c 成功执行，got %+v", res)
	}
}

// TestTaskLLMContractShape task_llm 是**全能力委派子代理**契约（2026-11-04）：
// 必填 prompt；可选 agent / session_id / title / waitSec / detach；旧的单次调用参数
// （system / timeoutSec）已移除。
func TestTaskLLMContractShape(t *testing.T) {
	props := contractProps(t, "task_llm")
	for _, k := range []string{"prompt", "agent", "session_id", "title", "waitSec", "detach"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("task_llm 契约缺少参数 %s", k)
		}
	}
	for _, k := range []string{"system", "timeoutSec"} {
		if _, ok := props[k]; ok {
			t.Fatalf("task_llm 不应再暴露单次调用参数 %s", k)
		}
	}
	if req, _ := props["prompt"].(map[string]any); req == nil {
		t.Fatal("task_llm.prompt 参数形状异常")
	}
}

// TestTaskStatusContractShape task_status 支持单任务查询（task_id + timeout）与任务树（limit）。
func TestTaskStatusContractShape(t *testing.T) {
	props := contractProps(t, "task_status")
	for _, k := range []string{"task_id", "timeout", "limit"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("task_status 契约缺少参数 %s", k)
		}
	}
}

// TestMovedDomainContractsShape 迁入的四个会话编排工具保持原契约必填项。
func TestMovedDomainContractsShape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		required []string
	}{
		{"ask_user", []string{"question"}},
		{"next_run", []string{"prompt"}},
		{"subchat_open", []string{"agent", "prompt", "llm", "scenario_id"}},
		{"subchat_finish", nil},
	} {
		props := contractProps(t, tc.name)
		for _, k := range tc.required {
			if _, ok := props[k]; !ok {
				t.Fatalf("%s 契约缺少参数 %s", tc.name, k)
			}
		}
	}
}

// TestTrimListField 任务列表 / 任务树按 limit 截断（P0：真实形状是 {list}/{nodes} 对象）。
func TestTrimListField(t *testing.T) {
	// 对象形状 {list:[...]}。
	tasks := map[string]any{"list": []any{"a", "b", "c"}, "other": 1}
	got, total, truncated := trimListField(tasks, "list", 2)
	m, _ := got.(map[string]any)
	list, _ := m["list"].([]any)
	if len(list) != 2 || list[0] != "a" || total != 3 || !truncated {
		t.Fatalf("对象形状截断异常：%v total=%d truncated=%v", m, total, truncated)
	}
	if m["other"] != 1 {
		t.Fatalf("截断不应丢弃其它字段：%v", m)
	}
	// 树形状 {nodes:[...]}。
	tree := map[string]any{"nodes": []any{1, 2, 3, 4}}
	got, total, truncated = trimListField(tree, "nodes", 2)
	m, _ = got.(map[string]any)
	nodes, _ := m["nodes"].([]any)
	if len(nodes) != 2 || total != 4 || !truncated {
		t.Fatalf("tree 截断异常：%v total=%d truncated=%v", m, total, truncated)
	}
	// 数组形状容错。
	if got, total, truncated := trimListField([]any{"a", "b", "c"}, "x", 2); total != 3 || !truncated || len(got.([]any)) != 2 {
		t.Fatalf("数组形状截断异常：%v total=%d truncated=%v", got, total, truncated)
	}
	// limit<=0 / 非数组 → 原样。
	if _, total, truncated := trimListField(tasks, "list", 0); total != 3 || truncated {
		t.Fatalf("limit<=0 不应截断：%d %v", total, truncated)
	}
	if got, _, _ := trimListField("x", "list", 2); got != "x" {
		t.Fatalf("非对象应原样返回：%v", got)
	}
}

// TestDelegateTimeout（P0）：delegate 桥超时按 waitSec 计算并有上限（15s 缺省不再截断）。
func TestDelegateTimeout(t *testing.T) {
	cases := []struct {
		waitSec int
		detach  bool
		want    time.Duration
	}{
		{-5, false, delegateMaxWait},
		{0, false, delegateMaxWait},
		{10, false, 25 * time.Second},
		{600, false, delegateMaxWait},
		{3600, false, delegateMaxWait},
		{10, true, delegateDetachTimeout},
		{0, true, delegateDetachTimeout},
	}
	for _, c := range cases {
		if got := delegateTimeout(c.waitSec, c.detach); got != c.want {
			t.Fatalf("delegateTimeout(%d,%v)=%v want %v", c.waitSec, c.detach, got, c.want)
		}
	}
	if delegateMaxWait > 600*time.Second {
		t.Fatalf("上限不得超过 600s：%v", delegateMaxWait)
	}
}

// TestAskUserEmptyQuestion ask_user 空 question 必须是参数错误（不发桥调用）。
// cx.Turn 非空即可在访问插件连接前返回。
func TestAskUserEmptyQuestion(t *testing.T) {
	p := &taskPlugin{}
	for _, q := range []any{nil, "", "   "} {
		if _, err := p.askUser(context.Background(), map[string]any{"question": q}, pluginsdk.CallContext{Turn: "t-1"}); err == nil {
			t.Fatalf("空 question 应返回参数错误（q=%q）", q)
		}
	}
}

// TestAskUserContractSyncPolicy 契约必须是 gateway 同步策略（ask 非阻塞语义由宿主 ask 能力
// 立即返回占位实现，不是 gateway 后台任务）。若写成 async: async，gateway 会走异步回执，
// 绕过 turn 侧 ask 的「任务节点保持 running、答复后收尾」分支。
func TestAskUserContractSyncPolicy(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	for _, tool := range tools {
		if tool.Name != "ask_user" {
			continue
		}
		if tool.Async != "sync" || !tool.AsyncSet {
			t.Fatalf("ask_user async = %q set=%v，期望显式 sync", tool.Async, tool.AsyncSet)
		}
		if !tool.PolicyLock {
			t.Fatalf("ask_user 应 policy_lock=true（界面/CLI 不可改成后台异步）")
		}
		if !tool.TimeoutSet || tool.Timeout != 0 {
			t.Fatalf("ask_user timeout = %d set=%v，期望显式 0（无调用超时）", tool.Timeout, tool.TimeoutSet)
		}
		return
	}
	t.Fatal("未找到 ask_user 契约")
}

// TestAskUserArgLimits ask_user 的运行时参数上限（与契约 schema 同步），且校验发生在桥调用前。
func TestAskUserArgLimits(t *testing.T) {
	p := &taskPlugin{}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"question 超长", map[string]any{"question": strings.Repeat("问", askMaxQuestionRunes+1)}, "question 过长"},
		{"context 超长", map[string]any{"question": "q", "context": strings.Repeat("背", askMaxContextRunes+1)}, "context 过长"},
		{"options 超量", map[string]any{"question": "q", "options": make([]any, askMaxOptions+1)}, "options 最多"},
		{"option 形状非法", map[string]any{"question": "q", "options": []any{42}}, "必须是字符串或"},
		{"option value 非字符串", map[string]any{"question": "q", "options": []any{map[string]any{"value": 1}}}, "options[0].value 必须是字符串"},
		{"option 空字符串", map[string]any{"question": "q", "options": []any{""}}, "options[0] 不能为空字符串"},
		{"option 空对象", map[string]any{"question": "q", "options": []any{map[string]any{}}}, "value/label 不能同时为空"},
		{"option 空白 label", map[string]any{"question": "q", "options": []any{map[string]any{"label": "  "}}}, "value/label 不能同时为空"},
		{"recommended 超量", map[string]any{"question": "q", "recommended": make([]any, askMaxRecommended+1)}, "recommended 最多"},
		{"recommended 元素非字符串", map[string]any{"question": "q", "recommended": []any{1}}, "recommended[0] 必须是字符串"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := p.askUser(context.Background(), c.args, pluginsdk.CallContext{Turn: "t-1"})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v，期望包含 %q", err, c.want)
			}
		})
	}
}

// TestValidateAskArgsNormalizesEmptyValue：显式空 value + 非空 label → value 归一为 label，
// 避免前端按 value 取值得到空串（P3）。
func TestValidateAskArgsNormalizesEmptyValue(t *testing.T) {
	args := map[string]any{
		"question": "q",
		"options":  []any{map[string]any{"value": "  ", "label": "是"}},
	}
	if err := validateAskArgs(args); err != nil {
		t.Fatalf("validateAskArgs: %v", err)
	}
	opt := args["options"].([]any)[0].(map[string]any)
	if got := asString(opt["value"]); got != "是" {
		t.Fatalf("空 value 应归一为 label，got=%q", got)
	}
}

// contractProps 取指定工具的 parameters.properties（缺契约 → 失败）。
func contractProps(t *testing.T, name string) map[string]any {
	t.Helper()
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	for _, tool := range tools {
		if tool.Name != name {
			continue
		}
		props, _ := tool.Schema["properties"].(map[string]any)
		if props == nil {
			t.Fatalf("%s 缺少 parameters.properties", name)
		}
		return props
	}
	t.Fatalf("未找到 %s 契约", name)
	return nil
}

// TestSettingsInterpreterMap 解释器配置 → runtime 表（node 键映射到 js runtime）。
func TestSettingsInterpreterMap(t *testing.T) {
	s := settings{
		Shell:    `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		NodePath: `C:\node\node.exe`,
		JavaPath: `C:\jdk\bin\java.exe`,
	}
	m := s.interpreterMap()
	if m["shell"] == "" || m["js"] != `C:\node\node.exe` || m["java"] != `C:\jdk\bin\java.exe` {
		t.Fatalf("interpreterMap = %v", m)
	}
	if _, ok := m["python"]; ok {
		t.Fatal("未配置的解释器不应入表")
	}
}
