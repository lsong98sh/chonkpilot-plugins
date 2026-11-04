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

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// vftsQueryToolName 与引擎同名（gateway 以 namespace="-" 原样暴露给 LLM）。
const vftsQueryToolName = "vfts_query"

// vftsQueryDescription 与引擎 vfts_query 同描述（scope/会话分支说明一致）。
const vftsQueryDescription = "在 vfts 全文索引中检索：match 为自然语言匹配串，query 为布尔/高级表达式" +
	"（二者至少给其一，query 不支持字段前缀）。scope=file（默认）检索文件：返回命中文件 + 片段/行号" +
	"（topK，按相关度）；scope=session 检索**会话正文索引**：按 ts 升序返回命中消息" +
	"（session_id/dialog_id/turn_id/message_id/role/ts/snippet/score），snippet 为命中词前后约 240 字符正文。"

// vftsQuerySchema 与引擎同形，**去掉 workdir**（由插件注入）。
var vftsQuerySchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"scope":      map[string]any{"type": "string", "enum": []string{"file", "session"}, "description": "检索范围（默认 file；session = 会话正文索引）"},
		"session_id": map[string]any{"type": "string", "description": "scope=session 时按会话过滤（缺省 = 当前会话）"},
		"role":       map[string]any{"type": "string", "enum": []string{"user", "assistant"}, "description": "scope=session 时按角色过滤（缺省 = 两者）"},
		"match":      map[string]any{"type": "string", "description": "自然语言匹配串（如：全文索引）"},
		"query":      map[string]any{"type": "string", "description": "布尔/高级表达式（如：fox AND brown；不支持 content: 字段前缀）"},
		"topK":       map[string]any{"type": "integer", "description": "返回命中数上限（默认 20）"},
		"path":       map[string]any{"type": "string", "description": "文件路径子串过滤（可选，scope=file）"},
	},
}

// registerVftsQuery 把插件的 `vfts_query` 注册进插件自带 MCP server（hot=true）。
func registerVftsQuery(srv *mcp.Server, cfg *configManager) {
	t := &mcp.Tool{Name: vftsQueryToolName, Description: vftsQueryDescription, InputSchema: vftsQuerySchema}
	t.Meta = mcp.Meta{"hot": true}
	srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if req != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errResultOf(fmt.Sprintf("参数解析失败: %v", err)), nil
			}
		}
		var meta map[string]any
		if req != nil {
			meta = map[string]any(req.Params.Meta)
		}
		out, err := cfg.vftsQuery(ctx, meta, args)
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

// vftsQuery 执行一次 vfts_query（workdir 由调用上下文注入；session_id 缺省取上下文会话）。
func (m *configManager) vftsQuery(ctx context.Context, meta, args map[string]any) (any, error) {
	ns := callMetaNS(meta)
	workdir, err := m.workdirFromMeta(ns)
	if err != nil {
		return nil, err
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
