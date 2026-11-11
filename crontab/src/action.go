package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/lsong98sh/chonkpilot-util/strx"
)

// 动作类型（三种）。
//
//   - run：执行一条 shell 命令（scriptrun）；
//   - notify：系统通知；
//   - agent：以完整智能体（全工具面）执行一段 prompt（如定时代码审查）。
const (
	ActionRun    = "run"
	ActionNotify = "notify"
	ActionAgent  = "agent"
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

// AgentParam 是 `action.type = agent` 的参数：以完整智能体（全工具面）执行 prompt。
//
// 执行归属 = 该任务的子会话（crontab-<task_id>）；结果正文写入 RunRecord.OutputTail，
// 并默认弹完成通知。timeout_sec 省略 / 0 = 插件配置 default-timeout-sec。
type AgentParam struct {
	Prompt      string `json:"prompt"`
	Agent       string `json:"agent,omitempty"`        // 场景 agent 名（可选；空 = 默认智能体）
	Title       string `json:"title,omitempty"`        // 任务节点 / 通知标题（可选）
	DisplayName string `json:"display_name,omitempty"` // 显示名（可选；默认「定时任务」）
	TimeoutSec  int    `json:"timeout_sec,omitempty"`  // 0 = 插件配置默认超时
	Notify      any    `json:"notify,omitempty"`       // 省略 = true（完成通知）；false = 不通知；字符串 = 自定义文案
}

// ActionSpec 是解析后的动作（互斥：Run / Notify / Agent 只有一组非 nil）。
type ActionSpec struct {
	Type   string
	Run    *RunParam
	Notify *NotifyParam
	Agent  *AgentParam
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
// 各层均拒绝未声明字段（契约 additionalProperties=false 的运行时兜底）。
func ParseActionRaw(raw json.RawMessage) (*ActionSpec, error) {
	var head struct {
		Type  string          `json:"type"`
		Param json.RawMessage `json:"param"`
	}
	if err := decodeStrict(raw, &head); err != nil {
		return nil, fmt.Errorf("action 必须是对象 {type, param}: %w", err)
	}
	typ := strings.ToLower(strings.TrimSpace(head.Type))
	switch typ {
	case ActionRun:
		var p RunParam
		if err := decodeStrict(head.Param, &p); err != nil {
			return nil, fmt.Errorf("run 的 param 必须是对象 {command, notify?}: %w", err)
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
			if err := decodeStrict(head.Param, &obj); err != nil {
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
	case ActionAgent:
		var p AgentParam
		if err := decodeStrict(head.Param, &p); err != nil {
			return nil, fmt.Errorf("agent 的 param 必须是对象 {prompt, agent?, title?, display_name?, timeout_sec?, notify?}: %w", err)
		}
		p.Prompt = strings.TrimSpace(p.Prompt)
		if p.Prompt == "" {
			return nil, errors.New("agent 的 param.prompt 不能为空")
		}
		p.Agent = strings.TrimSpace(p.Agent)
		p.Title = strings.TrimSpace(p.Title)
		p.DisplayName = strings.TrimSpace(p.DisplayName)
		if p.TimeoutSec < 0 || p.TimeoutSec > 86400 {
			return nil, fmt.Errorf("agent 的 timeout_sec 必须在 0~86400（0 = 插件默认），收到 %d", p.TimeoutSec)
		}
		if p.Notify != nil {
			if _, _, ok := notifySetting(p.Notify, true); !ok {
				return nil, errors.New("agent 的 notify 必须是布尔值或字符串")
			}
		}
		return &ActionSpec{Type: ActionAgent, Agent: &p}, nil
	default:
		return nil, fmt.Errorf("不支持的 action.type: %q（可选 run / notify / agent）", head.Type)
	}
}

// decodeStrict 解码 JSON 并拒绝结构体未声明的字段。
func decodeStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
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
	case ActionAgent:
		param, err := json.Marshal(s.Agent)
		if err != nil {
			return Action{}, err
		}
		return Action{Type: ActionAgent, Param: param}, nil
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
	case ActionAgent:
		title := strings.TrimSpace(s.Agent.Title)
		if title == "" {
			title = s.Agent.Prompt
			if i := strings.IndexAny(title, "\r\n"); i >= 0 {
				title = title[:i]
			}
		}
		return strx.TruncRunes("智能体："+strings.TrimSpace(title), limit)
	}
	return ""
}

// NotifySetting 解析 run.notify（false/true/自定义文本；缺省 = false）。
func (s *ActionSpec) NotifySetting() (enabled bool, text string) {
	if s == nil || s.Run == nil {
		return false, ""
	}
	on, text, _ := notifySetting(s.Run.Notify, false)
	return on, text
}

// AgentNotifySetting 解析 agent.notify（false/true/自定义文本；缺省 = true）。
func (s *ActionSpec) AgentNotifySetting() (enabled bool, text string) {
	if s == nil || s.Agent == nil {
		return false, ""
	}
	on, text, _ := notifySetting(s.Agent.Notify, true)
	return on, text
}

// notifySetting 解析动作的 notify 设置：nil → defaultOn；bool → 值；字符串 → 开启 + 自定义文案。
// ok=false 表示类型非法（仅对象/数组等）。
func notifySetting(v any, defaultOn bool) (enabled bool, text string, ok bool) {
	switch x := v.(type) {
	case nil:
		return defaultOn, "", true
	case bool:
		return x, "", true
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return defaultOn, "", true
		}
		return true, t, true
	}
	return false, "", false
}

// isHTTPURL 判定是否是 http(s) 绝对网址。
func isHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
