package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseActionRun(t *testing.T) {
	spec, err := ParseActionInput(map[string]any{
		"type":  "run",
		"param": map[string]any{"command": "npm run build", "notify": true},
	})
	if err != nil {
		t.Fatalf("ParseActionInput: %v", err)
	}
	if spec.Type != ActionRun || spec.Run.Command != "npm run build" {
		t.Fatalf("spec=%+v", spec)
	}
	if on, text := spec.NotifySetting(); !on || text != "" {
		t.Fatalf("notify 设置不符: on=%v text=%q", on, text)
	}
	if got := spec.Description(); got != "执行：npm run build" {
		t.Fatalf("description=%q", got)
	}
}

func TestParseActionNotifyStringAndObject(t *testing.T) {
	spec, err := ParseActionRaw(json.RawMessage(`{"type":"notify","param":"<b>该喝水了</b><br>走两步"}`))
	if err != nil {
		t.Fatalf("notify string: %v", err)
	}
	if spec.Notify.HTML == "" || spec.Notify.ClickURL != "" {
		t.Fatalf("spec=%+v", spec)
	}
	if got := spec.Description(); got != "通知：该喝水了\n走两步" {
		t.Fatalf("description=%q", got)
	}

	spec, err = ParseActionRaw(json.RawMessage(`{"type":"notify","param":{"html":"<p>hi</p>","click_url":"https://example.com"}}`))
	if err != nil {
		t.Fatalf("notify object: %v", err)
	}
	if spec.Notify.ClickURL != "https://example.com" {
		t.Fatalf("click_url=%q", spec.Notify.ClickURL)
	}
}

func TestParseActionErrors(t *testing.T) {
	cases := map[string]string{
		"未知 type":          `{"type":"open","param":{}}`,
		"run 缺 command":    `{"type":"run","param":{}}`,
		"notify 空":         `{"type":"notify","param":"  "}`,
		"click_url 非 http": `{"type":"notify","param":{"html":"x","click_url":"file:///c:/x"}}`,
		// 契约 additionalProperties=false → 未声明字段一律拒绝（不静默丢弃）。
		"action 未知字段":       `{"type":"run","param":{"command":"x"},"extra":1}`,
		"run param 未知字段":    `{"type":"run","param":{"command":"x","extra":1}}`,
		"notify param 未知字段": `{"type":"notify","param":{"html":"x","extra":1}}`,
		"agent param 未知字段":  `{"type":"agent","param":{"prompt":"x","extra":1}}`,
	}
	for name, raw := range cases {
		if _, err := ParseActionRaw(json.RawMessage(raw)); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestParseActionAgentDisplayName(t *testing.T) {
	spec, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{"prompt":"审查","display_name":"巡检"}}`))
	if err != nil {
		t.Fatalf("display_name 是声明字段: %v", err)
	}
	if spec.Agent.DisplayName != "巡检" {
		t.Fatalf("display_name=%q", spec.Agent.DisplayName)
	}
}

func TestParseActionAgent(t *testing.T) {
	spec, err := ParseActionInput(map[string]any{
		"type": "agent",
		"param": map[string]any{
			"prompt":      "审查本次改动",
			"agent":       "reviewer",
			"title":       "每日代码审查",
			"timeout_sec": float64(900),
		},
	})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	if spec.Type != ActionAgent || spec.Agent == nil {
		t.Fatalf("spec=%+v", spec)
	}
	if spec.Agent.Prompt != "审查本次改动" || spec.Agent.Agent != "reviewer" || spec.Agent.TimeoutSec != 900 {
		t.Fatalf("agent param=%+v", spec.Agent)
	}
	if on, text := spec.AgentNotifySetting(); !on || text != "" {
		t.Fatalf("agent notify 缺省应开启: on=%v text=%q", on, text)
	}
	if got := spec.Description(); got != "智能体：每日代码审查" {
		t.Fatalf("description=%q", got)
	}

	if _, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{}}`)); err == nil {
		t.Fatal("agent 缺 prompt 应报错")
	}
	if _, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{"prompt":"x","timeout_sec":-1}}`)); err == nil {
		t.Fatal("agent timeout_sec 负数应报错")
	}
	if _, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{"prompt":"x","notify":{}}}`)); err == nil {
		t.Fatal("agent notify 对象应报错")
	}

	// 默认 notify=true → Canonical → 再解析仍为 true。
	action, err := spec.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	back, err := parseStoredAction(action)
	if err != nil || back.Type != ActionAgent || back.Agent.Prompt != "审查本次改动" {
		t.Fatalf("agent round trip=%+v err=%v", back, err)
	}

	off, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{"prompt":"x","notify":false}}`))
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := off.AgentNotifySetting(); on {
		t.Fatal("agent notify=false 应关闭")
	}
}

// fakeAgentHost 记录 agent.run 入参并返回固定结果。
type fakeAgentHost struct {
	req agentRunReq
	res agentRunResult
	err error
}

func (f *fakeAgentHost) RunAgent(_ context.Context, req agentRunReq) (agentRunResult, error) {
	f.req = req
	return f.res, f.err
}

func TestRunAgentRecordsResultAndNotifies(t *testing.T) {
	fake := &fakeNotifier{}
	host := &fakeAgentHost{res: agentRunResult{
		OK: true, Status: "complete", Text: "审查结论：无阻断问题",
		SessionID: "crontab-ct_a", DialogID: "dlg-1", TurnID: "turn-1",
	}}
	r := &runner{cfg: testConfigManager(), notifier: fake, agents: host, logf: t.Logf}
	spec, err := ParseActionRaw(json.RawMessage(`{"type":"agent","param":{"prompt":"审查","title":"代码审查"}}`))
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "ct_a", Description: "智能体：代码审查", Action: Action{Type: ActionAgent},
		CreatedBy: CreatedBy{WorkDir: t.TempDir(), InstanceID: "ins-1"}}
	rec := r.runAgent(context.Background(), task, spec, 100)

	if !rec.OK || rec.Kind != ActionAgent || rec.SessionID != "crontab-ct_a" || rec.TurnID != "turn-1" {
		t.Fatalf("rec=%+v", rec)
	}
	if rec.OutputTail != "审查结论：无阻断问题" {
		t.Fatalf("output_tail=%q", rec.OutputTail)
	}
	if host.req.Prompt != "审查" || host.req.Title != "代码审查" || host.req.TimeoutSec != 600 {
		t.Fatalf("host req=%+v", host.req)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("默认应弹一次完成通知，got %d", len(fake.calls))
	}
}

func TestCanonicalRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`{"type":"run","param":{"command":"echo hi","notify":"完成提示"}}`,
		`{"type":"notify","param":"<b>x</b>"}`,
		`{"type":"notify","param":{"html":"<i>y</i>","click_url":"https://a.b"}}`,
		`{"type":"agent","param":{"prompt":"审查","notify":false}}`,
	} {
		spec, err := ParseActionRaw(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		action, err := spec.Canonical()
		if err != nil {
			t.Fatalf("Canonical: %v", err)
		}
		back, err := parseStoredAction(action)
		if err != nil {
			t.Fatalf("parseStoredAction: %v", err)
		}
		if back.Type != spec.Type {
			t.Fatalf("round trip type=%q want %q", back.Type, spec.Type)
		}
	}
}

func TestDescriptionLimit(t *testing.T) {
	long := strings.Repeat("a", 200)
	spec, err := ParseActionRaw(json.RawMessage(`{"type":"run","param":{"command":"` + long + `"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(spec.Description())); n > 80 {
		t.Fatalf("description 过长: %d", n)
	}
}
