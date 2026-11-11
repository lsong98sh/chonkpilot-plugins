// vfts_query.go —— 插件自注册的 `vfts_query` 工具（工具表面**不带 workdir**）。
//
// 2B-vfts §3.5：`vfts_query` 的 `workdir` 由插件从 MCP 请求的 `_meta.chonkpilot.work_dir`
// 注入（gateway `contextmeta.go` 已透传），缺失回退「该 instance 已登记的工作区」，仍缺失回退
// 「当前唯一实例的工作区」，都取不到 → 明确报错（不猜路径）。
//
// `session_id` 缺省取 `_meta.chonkpilot.session`，回退 `top_session`。
// 管理工具（vfts_configure / vfts_index / vfts_status / vfts_session_*）仍带 workdir（LLM 不可见）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	codegraph "github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/treesitter/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// vftsQueryToolName 与引擎同名（gateway 以 namespace="-" 原样暴露给 LLM）。
const vftsQueryToolName = "vfts_query"

// registerVftsQuery 把插件的 `vfts_query` 注册进插件自带 MCP server（hot=true）。
// schema / description / `_meta` 全部来自内嵌 `vfts_query.tool.md`。
func registerVftsQuery(srv *mcp.Server, cfg *configManager, tool contract.Tool) {
	schema := tool.Schema
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	t := &mcp.Tool{Name: vftsQueryToolName, Description: tool.Description, InputSchema: schema}
	t.Meta = mcp.Meta(contract.MetaFor(tool))
	t.Meta["hot"] = true
	srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if req != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errResultOf(fmt.Sprintf("参数解析失败: %v", err)), nil
			}
		}
		out, err := cfg.vftsQuery(ctx, req, args)
		if err != nil {
			return errResultOf(err.Error()), nil
		}
		b, jerr := json.MarshalIndent(out, "", "  ")
		if jerr != nil {
			return errResultOf("json 编码失败: " + jerr.Error()), nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
	})
}

// errResultOf 工具错误应答（isError=true，与引擎侧一致）。
func errResultOf(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}

// callMetaNS 取 `_meta.chonkpilot` 子对象（gateway contextmeta 的命名空间）。
func callMetaNS(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	ns, _ := meta["chonkpilot"].(map[string]any)
	return ns
}

// vftsMaxTopK 单次查询返回上限（file=文件数，session=消息条数）。
const vftsMaxTopK = 100

// validateVftsQueryArgs 严格校验并归一化 vfts_query 参数：
// 未知键/类型错误显式失败；scope/role 归一化并校验；match/query 互斥且至少其一；
// topK 1..100（超出截到 100）。
func validateVftsQueryArgs(args map[string]any) (map[string]any, error) {
	allowed := map[string]bool{
		"workdir": true, "scope": true, "session_id": true, "role": true,
		"match": true, "query": true, "topK": true, "path": true,
	}
	for k := range args {
		if !allowed[k] {
			return nil, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "scope", "session_id", "role", "match", "query", "path"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return nil, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	out := map[string]any{}
	for _, k := range []string{"workdir", "session_id", "path"} {
		if v := strings.TrimSpace(pluginsdk.Str(args[k])); v != "" {
			out[k] = v
		}
	}
	scope := strings.ToLower(strings.TrimSpace(pluginsdk.Str(args["scope"])))
	if scope == "" {
		scope = "file"
	}
	if scope != "file" && scope != "session" {
		return nil, fmt.Errorf("scope 只允许 file / session（收到 %q）", scope)
	}
	out["scope"] = scope
	role := strings.ToLower(strings.TrimSpace(pluginsdk.Str(args["role"])))
	if role != "" && role != "user" && role != "assistant" {
		return nil, fmt.Errorf("role 只允许 user / assistant（收到 %q）", role)
	}
	if role != "" {
		out["role"] = role
	}
	match := strings.TrimSpace(pluginsdk.Str(args["match"]))
	expr := strings.TrimSpace(pluginsdk.Str(args["query"]))
	if match == "" && expr == "" {
		return nil, fmt.Errorf("需要 match 或 query 至少其一")
	}
	if match != "" && expr != "" {
		return nil, fmt.Errorf("match 与 query 互斥（只给其一）")
	}
	if match != "" {
		out["match"] = match
	}
	if expr != "" {
		out["query"] = expr
	}
	topK := 20
	if v, ok, err := vftsIntArg(args, "topK"); err != nil {
		return nil, err
	} else if ok {
		if v < 1 {
			return nil, fmt.Errorf("topK 必须 >= 1")
		}
		topK = v
	}
	if topK > vftsMaxTopK {
		topK = vftsMaxTopK
	}
	out["topK"] = topK
	return out, nil
}

// vftsIntArg 读取整数参数（兼容 int/int64/float64；小数报错）。
func vftsIntArg(args map[string]any, key string) (int, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case float64:
		if v != float64(int64(v)) {
			return 0, false, fmt.Errorf("%s 必须是整数", key)
		}
		return int(v), true, nil
	default:
		return 0, false, fmt.Errorf("%s 必须是整数", key)
	}
}

// vftsQuery 执行一次 vfts_query（workdir 由调用上下文注入；session_id 缺省取上下文会话）。
//
// workdir 规则与 codegraph 查询工具一致（codegraph.ResolveWorkdirFromMeta）：
// args 显式传入必须是当前项目内的绝对路径；未传则取 `_meta.chonkpilot.work_dir`；
// 两者都没有时才回退到已登记实例/唯一工作区。
func (m *configManager) vftsQuery(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (any, error) {
	norm, err := validateVftsQueryArgs(args)
	if err != nil {
		return nil, err
	}
	args = norm
	var meta map[string]any
	if req != nil && req.Params != nil {
		meta = map[string]any(req.Params.Meta)
	}
	ns := callMetaNS(meta)

	var workdir string
	rawWD := strings.TrimSpace(pluginsdk.Str(args["workdir"]))
	metaWD := codegraph.MetaWorkdir(req)
	if rawWD != "" || metaWD != "" {
		if err := codegraph.ResolveWorkdirFromMeta(req, args); err != nil {
			return nil, err
		}
		workdir = strings.TrimSpace(pluginsdk.Str(args["workdir"]))
	} else {
		wd, err := m.workdirFromMeta(ns)
		if err != nil {
			return nil, err
		}
		workdir = wd
	}

	call := map[string]any{"workdir": workdir}
	for _, k := range []string{"scope", "match", "query", "topK", "path", "role"} {
		if v, ok := args[k]; ok && v != nil && v != "" {
			call[k] = v
		}
	}
	if s := strings.TrimSpace(pluginsdk.Str(args["session_id"])); s != "" {
		call["session_id"] = s
	} else if s := strings.TrimSpace(pluginsdk.Str(ns["session"])); s != "" {
		call["session_id"] = s
	} else if s := strings.TrimSpace(pluginsdk.Str(ns["top_session"])); s != "" {
		call["session_id"] = s
	}
	return m.engine(ctx, vftsQueryToolName, call)
}

// workdirFromMeta 从 `_meta.chonkpilot` 解析项目工作区：
//  1. work_dir（gateway 透传的实例工作目录）；
//  2. instance_id → 该实例登记时记录的工作区；
//  3. 当前唯一实例的工作区；
//  4. 都取不到 → 明确报错（**不猜路径**）。
func (m *configManager) workdirFromMeta(ns map[string]any) (string, error) {
	if wd := normalizeWorkdir(pluginsdk.Str(ns["work_dir"])); wd != "" {
		return wd, nil
	}
	if inst := strings.TrimSpace(pluginsdk.Str(ns["instance_id"])); inst != "" {
		if wd := m.workdirOf(inst); wd != "" {
			return wd, nil
		}
	}
	if wd := m.uniqueWorkdir(); wd != "" {
		return wd, nil
	}
	return "", fmt.Errorf("无法确定项目工作区：调用上下文缺少 work_dir / instance_id，且当前没有唯一打开的项目")
}

// uniqueWorkdir 当前已登记实例的唯一工作区（多个不同工作区 → ""）。
func (m *configManager) uniqueWorkdir() string {
	wds := m.allWorkdirs()
	if len(wds) == 1 {
		return wds[0]
	}
	return ""
}
