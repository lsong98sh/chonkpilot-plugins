package main

import (
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
	}
	for name, raw := range cases {
		if _, err := ParseActionRaw(json.RawMessage(raw)); err == nil {
			t.Errorf("%s 应报错", name)
		}
	}
}

func TestCanonicalRoundTrip(t *testing.T) {
	for _, raw := range []string{
		`{"type":"run","param":{"command":"echo hi","notify":"完成提示"}}`,
		`{"type":"notify","param":"<b>x</b>"}`,
		`{"type":"notify","param":{"html":"<i>y</i>","click_url":"https://a.b"}}`,
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
