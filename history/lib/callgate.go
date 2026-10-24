// callgate.go：gateway 工具面——4 个文件历史工具的注册/注销与执行回调（onToolCall），
// 以及 gateway **前置打点钩子**主题（pre_hook_subject）的声明。
//
// 注册/注销走 gateway 方法面 tools/register|unregister（mcp-tools-register/unregister），
// payload 字段名对齐 mcpgateway.regMsg：name/description/schema/handler_subject/owner/hot/category
// + **pre_hook_subject**（本插件新增的可选字段：gateway 在执行**任何**工具前向该相对主题发一次
// 同步前置钩子请求；未声明钩子的部署**零开销**，见 mcpgateway.doCall）。
package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chonkpilot/chonkpilot-lib/mq"
)

// gatewayTool 注册到 gateway 的文件历史工具定义。
type gatewayTool struct {
	name        string
	description string
	props       map[string]any
}

// schemaJSON 序列化 JSON Schema 文本（{"type":"object","properties":{...}}）。
func (t *gatewayTool) schemaJSON() string {
	schema := map[string]any{"type": "object", "properties": t.props}
	b, _ := json.Marshal(schema)
	return string(b)
}

// toProp `to` 参数（三态：相对步 / turn-start / 绝对 commit id）。
func toProp(desc string) map[string]any {
	return map[string]any{
		"type": []string{"integer", "string"},
		"description": desc + "（负整数 = 相对步：-1 最近一步、-2 再上一步…；\"turn-start\" = 当前轮起点；" +
			"或绝对 commit id。缺省 -1）",
	}
}

// historyTools 对外工具全集（category=self；启用且 workdir 有 .git 才注册）。
var historyTools = []gatewayTool{
	{
		name: "history_status",
		description: "查看本项目的文件检查点链与状态：每步的相对编号（-1=最近一步）、时间、工具名、来源会话、" +
			"文件数与 +n/−m，以及 mode/脏标记/失败次数。检查点是执行工具前自动打点的快照（独立 ref，不在你的分支上）。",
		props: map[string]any{},
	},
	{
		name: "history_diff",
		description: "查看某一步检查点相对其前一步的 diff 文本（超 32KB 截断并标注）。用于了解某步改了什么、" +
			"再决定用文件工具改回或 history_restore。",
		props: map[string]any{
			"to":   toProp("目标检查点"),
			"path": map[string]any{"type": "string", "description": "只显示该文件的 diff（workdir 相对路径；可选）"},
		},
	},
	{
		name:        "history_show",
		description: "读取某一步检查点里某个文件的内容（用于对照/复制回写；超 32KB 截断并标注）。",
		props: map[string]any{
			"to":   toProp("目标检查点"),
			"path": map[string]any{"type": "string", "description": "文件路径（workdir 相对）"},
		},
	},
	{
		name: "history_restore",
		description: "把某一步检查点里的**单个文件**写回工作区（删除恢复 / 仅撤本会话自己的单文件改动）。" +
			"强制校验：该文件工作区当前内容必须等于链头检查点里的内容，或该文件已被删除；否则拒绝" +
			"（本会话之外被改动过 → 请用 history_diff 查看后用文件工具自行修改）。path 必填、单文件、禁止批量。",
		props: map[string]any{
			"path": map[string]any{"type": "string", "description": "要写回的文件（workdir 相对路径；必填）"},
			"to":   toProp("来源检查点"),
		},
	},
}

// registerAll 逐个向 gateway 注册 4 个工具（同主题 promise：await v.Result/v.Err）。
// 每个工具声明 pre_hook_subject → gateway 在执行任意工具前先同步打点。
func (h *History) registerAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), dataTimeout)
	defer cancel()
	var errs []string
	for _, t := range historyTools {
		payload := map[string]any{
			"name":             t.name,
			"description":      t.description,
			"schema":           t.schemaJSON(),
			"handler_subject":  toolCallSubject,
			"owner":            "history",
			"hot":              true,
			"category":         "self",
			"pre_hook_subject": preHookSubject,
		}
		v := h.deps.Bus.Emit(ctx, subjectToolRegister, payload).Wait()
		if e := v.Err(); e != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.name, e))
			continue
		}
		if v.Result == nil {
			errs = append(errs, t.name+": gateway 未应答（未就绪？）")
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// unregisterAll 逐个注销（kind=tool；gateway handleUnregister → unregisterTool）。
func (h *History) unregisterAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), dataTimeout)
	defer cancel()
	var errs []string
	for _, t := range historyTools {
		payload := map[string]any{"name": t.name, "kind": "tool"}
		v := h.deps.Bus.Emit(ctx, subjectToolUnregister, payload).Wait()
		if e := v.Err(); e != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", t.name, e))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// toolCallMsg 是 gateway regProv 发往本插件回调主题的载荷（{tool, args, context}）。
type toolCallMsg struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	Ctx  *struct {
		Turn       string `json:"turn"`
		Session    string `json:"session"`
		InstanceID string `json:"instance_id"`
		ToolCallID string `json:"tool_call_id"`
		TopSession string `json:"top_session"`
	} `json:"context"`
}

// toolReply 构造 gateway regProv 期望的 v.Result（对齐 llm domainmcp 写回形态）。
func toolReply(text string, isErr bool) map[string]any {
	return map[string]any{
		"resultType": "complete",
		"content":    []any{map[string]any{"type": "text", "text": text}},
		"isError":    isErr,
	}
}

// onToolCall 执行 gateway 工具回调：instance → workdir → workState 定位；
// 未启用 / 非 git 仓库 / 未知实例 → 提示文本；否则按工具分派。
func (h *History) onToolCall(_ context.Context, _ string, v *mq.Value) error {
	reply := func(text string, isErr bool) error {
		v.Result = toolReply(text, isErr)
		return nil
	}
	var msg toolCallMsg
	if err := json.Unmarshal(v.Payload, &msg); err != nil {
		return reply("history: 回调载荷解析失败: "+err.Error(), true)
	}
	if msg.Tool == "" {
		return reply("history: 回调载荷缺 tool", true)
	}
	inst := ""
	if msg.Ctx != nil {
		inst = msg.Ctx.InstanceID
	}
	if inst == "" {
		if s, ok := msg.Args["instance_id"].(string); ok {
			inst = s
		}
	}
	if inst == "" {
		return reply("history: 无法定位调用实例（context.instance_id 缺失）", true)
	}
	rec, ok := h.im.Lookup(inst)
	if !ok || rec.WorkDir == "" {
		return reply("history: 未知实例 "+inst, true)
	}
	ws := h.work(rec.WorkDir)
	if ws == nil || !ws.hasGit {
		return reply("history: 该项目不是 git 仓库——文件历史不可用", true)
	}
	if !ws.enabled.Load() {
		return reply("history: 文件历史未启用（项目配置 history.enabled 需为 \"true\"）", true)
	}
	session, top := "", ""
	if msg.Ctx != nil {
		session, top = msg.Ctx.Session, msg.Ctx.TopSession
	}
	root := top
	if root == "" {
		root = session
	}
	h.rememberSession(session, root, rec.WorkDir, inst)
	slug := chainSlug(root)

	text, isErr := h.dispatch(ws, slug, msg.Tool, msg.Args)
	return reply(text, isErr)
}

// dispatch 按工具名分派（返回人类/LLM 可读文本 + isError）。
//
// 持 ws.mu 全程：与本 workdir 的打点（checkpointSync）串行——restore 的
// checkConsistency→WriteFile 与并发打点（git add -A）之间不再有 TOCTOU 窗口；
// diff/show/status 的 git 读同口径串行（baseStatus / resolveTo 的 turn-start 分支
// 已改为“须持 ws.mu”，不在内部重复加锁，避免自锁）。
func (h *History) dispatch(ws *workState, slug, tool string, args map[string]any) (string, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	switch tool {
	case "history_status":
		st, entries := h.buildStatus(ws, slug)
		out := statusOut{chainStatus: st, Checkpoints: entries}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "history: 序列化状态失败: " + err.Error(), true
		}
		return string(b), false

	case "history_diff":
		rel := ""
		if s, ok := args["path"].(string); ok && strings.TrimSpace(s) != "" {
			r, err := safeRel(s)
			if err != nil {
				return "history: " + err.Error(), true
			}
			rel = r
		}
		ckpt, err := h.resolveTo(ws, slug, args["to"])
		if err != nil {
			return "history: " + err.Error(), true
		}
		text, err := h.diffText(ws, ckpt, rel)
		if err != nil {
			return "history: " + err.Error(), true
		}
		if strings.TrimSpace(text) == "" {
			return fmt.Sprintf("（检查点 %s 无差异）", shortID(ckpt)), false
		}
		return text, false

	case "history_show":
		rel, err := safeRel(strArg(args["path"]))
		if err != nil {
			return "history: " + err.Error(), true
		}
		ckpt, err := h.resolveTo(ws, slug, args["to"])
		if err != nil {
			return "history: " + err.Error(), true
		}
		blob, truncated, err := h.showBlob(ws, ckpt, rel, showMaxBytes)
		if err != nil {
			return "history: " + err.Error(), true
		}
		text := string(blob)
		if truncated {
			text += fmt.Sprintf("\n…（文件内容超 %d 字节已截断；大文件请分段查看）", showMaxBytes)
		}
		return text, false

	case "history_restore":
		rel, err := safeRel(strArg(args["path"]))
		if err != nil {
			return "history: " + err.Error(), true
		}
		ckpt, err := h.resolveTo(ws, slug, args["to"])
		if err != nil {
			return "history: " + err.Error(), true
		}
		if err := h.restoreFile(ws, slug, rel, ckpt); err != nil {
			return "history: " + err.Error(), true
		}
		b, _ := json.Marshal(map[string]any{"restored": rel, "from": ckpt})
		return string(b), false
	}
	return "history: 未知工具 " + tool, true
}

// statusOut 是 history_status 的返回（状态字段 + 检查点列表）。
type statusOut struct {
	chainStatus
	Checkpoints []timelineEntry `json:"checkpoints"`
}

// strArg 取字符串参数（非字符串 → ""）。
func strArg(v any) string {
	s, _ := v.(string)
	return s
}
