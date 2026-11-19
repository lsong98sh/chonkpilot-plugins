// chonkpilot-session —— 会话工具插件（SESSION-QUERY-1 / SESSION-STATUS-1，2026-11-15）。
//
// 工具集：
//   - session_search   跨会话 / 跨用户检索消息正文（宿主 plugin/v1/session.search）
//   - session_list     枚举会话（宿主 plugin/v1/session.browse）
//   - session_get      会话元数据（宿主 plugin/v1/session.read，view=meta）
//   - session_history  分页读原文（宿主 plugin/v1/session.read，view=history）
//   - session_status   持久状态 + 最终回复（宿主 plugin/v1/session.status；可等待）
//   - session_create / send / rename / open（宿主 session.run / 框架 session.rename / session.open）
//
// 口径：
//   - **self / top 由注入上下文决定**：`_meta.chonkpilot.session` / `.top_session` 原样转发给
//     宿主，工具参数里的 `session_id` 只表达"目标"；省略 = 本会话由宿主解析。
//   - **work_dir / instance_id 不出现在工具 schema**：从 `CallContext` 注入。
//   - 跨用户能力（targets / 模式 / 授权 / 审计）全部在宿主，本插件零策略。
//
// 契约源：本仓 `session/tools/session/*.tool.md`，经 `//go:embed` 打进插件二进制（插件自己是
// MCP server；不进主仓 capability 目录）。
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// toolsFS 是工具契约（`*.tool.md`）的 embed 源：插件自持，产物不带契约目录。
//
//go:embed all:tools
var toolsFS embed.FS

// toolsRoot 是 embed 内契约目录名（`tools/session/`）。
const toolsRoot = "tools/session"

const (
	// 只读查询的桥超时：search / history 可能扫多会话，list / get 通常很快。
	queryTimeout = 60 * time.Second
	metaTimeout  = 30 * time.Second
	// sessionRunMaxWait 是 create / send 单次桥调用的等待上限（与契约 timeout 对齐）。
	sessionRunMaxWait = 600
	// sessionRunAckTimeout 是异步（wait_sec=0）时等"受理"应答的上限。
	sessionRunAckTimeout = 30 * time.Second
	// sessionRunWaitBuffer 是 wait_sec 到期与桥超时之间的余量：宿主先返回 running。
	sessionRunWaitBuffer = 15 * time.Second
)

// capCaller 是宿主能力的窄接口（*pluginsdk.Plugin 实现；测试注入 fake）。
type capCaller interface {
	CallIntoWithTimeout(ctx context.Context, timeout time.Duration, method string, args, out any) error
}

// sessionPlugin 是插件主体（只做工具适配，不含业务策略）。
type sessionPlugin struct {
	p    *pluginsdk.Plugin
	call capCaller
}

// embeddedManifest 是插件 manifest（构建期嵌入 exe，随 `plugin.hello` 上报；宿主不再读盘）。
//
//go:embed manifest.json
var embeddedManifest []byte

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		Manifest: embeddedManifest,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("session: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	t := &sessionPlugin{p: p, call: p}

	contracts, err := loadContracts()
	if err != nil {
		slog.Error("session: 加载工具契约失败", "err", err)
		os.Exit(1)
	}
	if missing := registerTools(p, t, contracts); len(missing) > 0 {
		slog.Error("session: 契约缺少实现", "tools", strings.Join(missing, ","))
		os.Exit(1)
	}

	p.OnRequest("status", t.info)
	p.OnRequest("ping", pluginsdk.PingHandler(p.Version()))
	// 自持 webui（配置页）：宿主经 UDS 逐文件拉取。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)
	// 配置动作（load / save / reset）：页面唯一写入口。
	for name, fn := range t.configActions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("session: 运行结束", "err", err)
	}
}

// loadContracts 读取 embed 内的契约（按名称排序）。
func loadContracts() ([]contract.Tool, error) {
	entries, err := fs.ReadDir(toolsFS, toolsRoot)
	if err != nil {
		return nil, fmt.Errorf("读取契约目录 %s: %w", toolsRoot, err)
	}
	out := make([]contract.Tool, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tool.md") {
			continue
		}
		raw, err := fs.ReadFile(toolsFS, toolsRoot+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		tool, err := contract.Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if tool.Name == "" {
			continue
		}
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// registerTools 按契约注册工具（hot 由契约 `hot: true` 决定）。
func registerTools(p *pluginsdk.Plugin, t *sessionPlugin, tools []contract.Tool) []string {
	handlers := map[string]pluginsdk.ToolFunc{
		"session_search":  t.search,
		"session_list":    t.list,
		"session_get":     t.get,
		"session_history": t.history,
		"session_status":  t.status,
		"session_create":  t.create,
		"session_send":    t.send,
		"session_rename":  t.rename,
		"session_open":    t.open,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			slog.Warn("session: 契约无对应实现，跳过", "tool", tool.Name)
			continue
		}
		p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), fn)
		seen[tool.Name] = true
	}
	var missing []string
	for name := range handlers {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

func (t *sessionPlugin) info(context.Context, map[string]any) (any, error) {
	return map[string]any{
		"ok": true, "id": "session", "version": t.p.Version(),
		"tools": []string{
			"session_search", "session_list", "session_get", "session_history", "session_status",
			"session_create", "session_send", "session_rename", "session_open",
		},
	}, nil
}

// ── 工具实现 ──────────────────────────────

// search 跨会话检索正文。
func (t *sessionPlugin) search(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	in := baseArgs(cx)
	copyArgs(in, args,
		"query", "targets", "roles", "session_id", "since", "limit", "context_runes", "mode")
	return t.callCap(ctx, queryTimeout, "plugin/v1/session.search", in)
}

// list 枚举会话。
func (t *sessionPlugin) list(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	in := baseArgs(cx)
	copyArgs(in, args, "targets", "include_sub", "kind", "since", "limit", "offset")
	return t.callCap(ctx, queryTimeout, "plugin/v1/session.browse", in)
}

// get 读会话元数据（session_id 省略 = 由宿主解析为本会话）。
func (t *sessionPlugin) get(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	in := baseArgs(cx)
	copyArgs(in, args, "session_id", "targets")
	in["view"] = "meta"
	return t.callCap(ctx, metaTimeout, "plugin/v1/session.read", in)
}

// history 分页读会话原文。
func (t *sessionPlugin) history(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	in := baseArgs(cx)
	copyArgs(in, args, "session_id", "targets", "before_turn_id", "target_messages", "target_bytes")
	in["view"] = "history"
	return t.callCap(ctx, queryTimeout, "plugin/v1/session.read", in)
}

// status 查会话持久化状态与最终回复（不依赖调用轮次存活，session_id 省略 = 本会话）。
func (t *sessionPlugin) status(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	waitSec, err := boundedInt(args, "wait_sec", 0, sessionRunMaxWait)
	if err != nil {
		return nil, err
	}
	in := baseArgs(cx)
	copyArgs(in, args, "session_id", "turn_id", "targets")
	in["wait_sec"] = waitSec
	return t.callCap(ctx, runTimeout(waitSec), "plugin/v1/session.status", in)
}

// create 新建顶层会话并自动跑首轮（宿主 plugin/v1/session.run，session_id 缺省）。
func (t *sessionPlugin) create(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	waitSec, err := boundedInt(args, "wait_sec", 0, sessionRunMaxWait)
	if err != nil {
		return nil, err
	}
	in := baseArgs(cx)
	copyArgs(in, args, "title", "prompt", "agent", "scenario_id")
	in["turn"] = cx.Turn
	in["tool_call_id"] = cx.ToolCallID
	in["display_name"] = cx.DisplayName
	in["wait_sec"] = waitSec
	return t.callCap(ctx, runTimeout(waitSec), "plugin/v1/session.run", in)
}

// send 向既有会话追发一轮（宿主 plugin/v1/session.run，session_id 必填）。
func (t *sessionPlugin) send(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	sid := resolveSendTarget(args["session_id"], cx)
	if sid == "" {
		return nil, fmt.Errorf("缺少目标会话（session_id）：请传 session_create / session_list 返回的 id")
	}
	waitSec, err := boundedInt(args, "wait_sec", 0, sessionRunMaxWait)
	if err != nil {
		return nil, err
	}
	in := baseArgs(cx)
	copyArgs(in, args, "prompt")
	in["session_id"] = sid
	in["turn"] = cx.Turn
	in["tool_call_id"] = cx.ToolCallID
	in["display_name"] = cx.DisplayName
	in["wait_sec"] = waitSec
	return t.callCap(ctx, runTimeout(waitSec), "plugin/v1/session.run", in)
}

// rename 改会话标题（框架命令面 plugin/v1/session.rename → data-session-title）。
//
// `data-session-title` 走一树一库路由（必须带 dialog_id），而 LLM 只看到 session_id；
// 这里先经 `session.read(view=meta)` 由宿主反查 dialog_id，再提交改名——不把 dialog_id
// 暴露进工具 schema（2J §2：对话定位属宿主内部）。
func (t *sessionPlugin) rename(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	sid := resolveOwnTarget(args["session_id"], cx)
	if sid == "" {
		return nil, fmt.Errorf("缺少目标会话（session_id）")
	}
	title := strings.TrimSpace(pluginsdk.Str(args["title"]))
	if title == "" {
		return nil, fmt.Errorf("title 不能为空")
	}
	metaIn := baseArgs(cx)
	metaIn["session_id"] = sid
	metaIn["view"] = "meta"
	metaAny, err := t.callCap(ctx, metaTimeout, "plugin/v1/session.read", metaIn)
	if err != nil {
		return nil, err
	}
	meta, _ := metaAny.(map[string]any)
	dialogID := ""
	if inner, ok := meta["session"].(map[string]any); ok {
		dialogID = strings.TrimSpace(pluginsdk.Str(inner["dialog_id"]))
	}
	if dialogID == "" {
		return nil, fmt.Errorf("无法定位会话所在对话: %s", sid)
	}
	in := baseArgs(cx)
	in["session_id"] = sid
	in["id"] = sid // 框架信封（data-session-title）字段名是 `id`
	in["dialog_id"] = dialogID
	in["title"] = title
	return t.callCap(ctx, metaTimeout, "plugin/v1/session.rename", in)
}

// open 打开 / 激活会话窗口（宿主 plugin/v1/session.open）。
func (t *sessionPlugin) open(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	in := baseArgs(cx)
	copyArgs(in, args, "session_id")
	return t.callCap(ctx, metaTimeout, "plugin/v1/session.open", in)
}

// runTimeout 按 wait_sec 计算 session.run 的桥超时（与 task_llm 同口径）。
func runTimeout(waitSec int) time.Duration {
	if waitSec <= 0 {
		return sessionRunAckTimeout
	}
	if waitSec > sessionRunMaxWait {
		waitSec = sessionRunMaxWait
	}
	return time.Duration(waitSec)*time.Second + sessionRunWaitBuffer
}

// boundedInt 读可选整数参数（缺失 = 0）；越界报错（不静默夹取）。
func boundedInt(args map[string]any, key string, min, max int) (int, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, nil
	}
	n := pluginsdk.Int(v)
	if n < min || n > max {
		return 0, fmt.Errorf("%s 必须是 %d..%d 的整数", key, min, max)
	}
	return n, nil
}

// resolveSendTarget 解析发送目标：省略 / @top = 当前顶层对话；@self = 执行会话。
func resolveSendTarget(v any, cx pluginsdk.CallContext) string {
	s := strings.TrimSpace(pluginsdk.Str(v))
	switch s {
	case "", "@top":
		if cx.TopSession != "" {
			return cx.TopSession
		}
		return cx.Session
	case "@self":
		if cx.Session != "" {
			return cx.Session
		}
		return cx.TopSession
	}
	return s
}

// resolveOwnTarget 解析"本会话"目标（rename）：省略 / @self = 执行会话；@top = 顶层。
func resolveOwnTarget(v any, cx pluginsdk.CallContext) string {
	s := strings.TrimSpace(pluginsdk.Str(v))
	switch s {
	case "", "@self":
		if cx.Session != "" {
			return cx.Session
		}
		return cx.TopSession
	case "@top":
		if cx.TopSession != "" {
			return cx.TopSession
		}
		return cx.Session
	}
	return s
}

// callCap 调宿主能力并把结果解为 map（桥错误原样透出，保留稳定错误码）。
func (t *sessionPlugin) callCap(ctx context.Context, timeout time.Duration, method string, in map[string]any) (any, error) {
	var out map[string]any
	if err := t.call.CallIntoWithTimeout(ctx, timeout, method, in, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// baseArgs 注入调用上下文（LLM 不可见的 self / top / work_dir / instance）。
func baseArgs(cx pluginsdk.CallContext) map[string]any {
	in := map[string]any{}
	if cx.WorkDir != "" {
		in["work_dir"] = cx.WorkDir
	}
	if cx.InstanceID != "" {
		in["instance_id"] = cx.InstanceID
	}
	if cx.Session != "" {
		in["session"] = cx.Session
	}
	if cx.TopSession != "" {
		in["top_session"] = cx.TopSession
	}
	return in
}

// copyArgs 只透传契约里声明的参数（缺失不写；显式 false/0 保留）。
func copyArgs(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}
