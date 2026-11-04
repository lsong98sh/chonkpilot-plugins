package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/lsong98sh/chonkpilot-util/strx"
)

// 动作类型（唯一两种）。
const (
	ActionRun    = "run"
	ActionNotify = "notify"
)

// RunParam 是 `action.type = run` 的参数：一条命令 + 是否完成通知。
type RunParam struct {
	Command string `json:"command"`
	Notify  any    `json:"notify,omitempty"` // false | true | "自定义文案"
}

// NotifyParam 是 `action.type = notify` 的参数（HTML + 可选点击网址）。
type NotifyParam struct {
	HTML     string `json:"html"`
	ClickURL string `json:"click_url,omitempty"`
}

// ActionSpec 是解析后的动作（互斥：Run 或 Notify 只有一组非 nil）。
type ActionSpec struct {
	Type   string
	Run    *RunParam
	Notify *NotifyParam
}

// ParseActionInput 解析工具参数里的 `action`（对象 {type,param}）。
func ParseActionInput(v any) (*ActionSpec, error) {
	if v == nil {
		return nil, errors.New("action 不能为空")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("action 编码失败: %w", err)
	}
	return ParseActionRaw(raw)
}

// ParseActionRaw 解析规范化动作 JSON（`{type, param}`）。
func ParseActionRaw(raw json.RawMessage) (*ActionSpec, error) {
	var head struct {
		Type  string          `json:"type"`
		Param json.RawMessage `json:"param"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("action 必须是对象 {type, param}: %w", err)
	}
	typ := strings.ToLower(strings.TrimSpace(head.Type))
	switch typ {
	case ActionRun:
		var p RunParam
		if err := json.Unmarshal(head.Param, &p); err != nil {
			return nil, errors.New("run 的 param 必须是对象 {command, notify?}")
		}
		p.Command = strings.TrimSpace(p.Command)
		if p.Command == "" {
			return nil, errors.New("run 的 param.command 不能为空")
		}
		return &ActionSpec{Type: ActionRun, Run: &p}, nil
	case ActionNotify:
		p := &NotifyParam{}
		var s string
		if err := json.Unmarshal(head.Param, &s); err == nil {
			p.HTML = strings.TrimSpace(s)
		} else {
			var obj struct {
				HTML     string `json:"html"`
				ClickURL string `json:"click_url"`
			}
			if err := json.Unmarshal(head.Param, &obj); err != nil {
				return nil, errors.New("notify 的 param 必须是 HTML 字符串或 {html, click_url?}")
			}
			p.HTML = strings.TrimSpace(obj.HTML)
			p.ClickURL = strings.TrimSpace(obj.ClickURL)
		}
		if HTMLToText(p.HTML) == "" {
			return nil, errors.New("notify 的 param 不能为空")
		}
		if p.ClickURL != "" && !isHTTPURL(p.ClickURL) {
			return nil, errors.New("notify 的 click_url 必须是 http(s) 网址")
		}
		return &ActionSpec{Type: ActionNotify, Notify: p}, nil
	default:
		return nil, fmt.Errorf("不支持的 action.type: %q（可选 run / notify）", head.Type)
	}
}

// parseStoredAction 解析落库动作（Action{Type, Param} → ActionSpec）。
func parseStoredAction(a Action) (*ActionSpec, error) {
	raw, err := json.Marshal(map[string]any{"type": a.Type, "param": a.Param})
	if err != nil {
		return nil, err
	}
	return ParseActionRaw(raw)
}

// Canonical 返回落库用规范化动作（param 按 type 重新编码）。
func (s *ActionSpec) Canonical() (Action, error) {
	switch s.Type {
	case ActionRun:
		param, err := json.Marshal(s.Run)
		if err != nil {
			return Action{}, err
		}
		return Action{Type: ActionRun, Param: param}, nil
	case ActionNotify:
		var param []byte
		var err error
		if s.Notify.ClickURL == "" {
			param, err = json.Marshal(s.Notify.HTML)
		} else {
			param, err = json.Marshal(s.Notify)
		}
		if err != nil {
			return Action{}, err
		}
		return Action{Type: ActionNotify, Param: param}, nil
	}
	return Action{}, fmt.Errorf("不支持的动作类型: %s", s.Type)
}

// Description 派生任务简述（≤80 字符）。
func (s *ActionSpec) Description() string {
	limit := 60
	switch s.Type {
	case ActionRun:
		first := s.Run.Command
		if i := strings.IndexAny(first, "\r\n"); i >= 0 {
			first = first[:i]
		}
		return strx.TruncRunes("执行："+strings.TrimSpace(first), limit)
	case ActionNotify:
		return strx.TruncRunes("通知："+HTMLToText(s.Notify.HTML), limit)
	}
	return ""
}

// NotifySetting 解析 run.notify（false/true/自定义文本）。
func (s *ActionSpec) NotifySetting() (enabled bool, text string) {
	if s == nil || s.Run == nil {
		return false, ""
	}
	switch v := s.Run.Notify.(type) {
	case bool:
		return v, ""
	case string:
		t := strings.TrimSpace(v)
		if t == "" {
			return false, ""
		}
		return true, t
	}
	return false, ""
}

// isHTTPURL 判定是否是 http(s) 绝对网址。
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
