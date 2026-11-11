package server

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolSpec 一个 MCP 工具。Hot=false 的管理工具不注入 _meta.hot（LLM 不可见）。
type toolSpec struct {
	Name        string
	Description string
	Props       map[string]any
	Required    []string
	Hot         bool
	Fn          func(ctx context.Context, args map[string]any) (any, error)
}

var toolSpecs = []toolSpec{
	// ---- 管理类（hot=false，plugin 直接调用，不发给 LLM）----
	{
		Name:        "codegraph_configure",
		Description: "配置某 workdir 的 codegraph 工作区：enabled 标识、参与索引的扩展名 exts、排除规则 skip_dirs（gitignore 语法，最高优先级）、是否叠加 gitignore 体系 stack_gitignore（可见性门控由调用方/plugin 负责，引擎仅记录与存档）；mode=clear 时额外清除该 workdir 的索引产物（删除 index.db、状态回未初始化，配置存档保留）。",
		Props: map[string]any{
			"workdir":         map[string]any{"type": "string", "description": "项目根目录（绝对路径）"},
			"enabled":         map[string]any{"type": "boolean", "description": "是否启用（缺省不改）"},
			"exts":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "参与索引的扩展名（如 [\".go\",\".ts\"]；缺省不改，替换式；空数组 = 全部受支持语言）"},
			"skip_dirs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "用户排除规则（gitignore 语法，每项一条；优先级最高；缺省不改）"},
			"stack_gitignore": map[string]any{"type": "boolean", "description": "是否额外应用各级 .gitignore / .git/info/exclude / 全局 ignore（缺省不改）"},
			"mode":            map[string]any{"type": "string", "description": "操作模式：缺省 = 仅存档配置；clear = 清除索引产物（删 index.db、状态回未初始化；enabled/exts/skip_dirs 保留）"},
		},
		Required: []string{"workdir"},
		Fn:       toolConfigure,
	},
	{
		Name:        "codegraph_initialize",
		Description: "对某 workdir 全量建索引（同步，直到完成并落盘到 <workdir>/.chonkpilot/codegraph/）。源码先识别编码并统一转 UTF-8（UTF-8 BOM / UTF-16 / GBK 等）再解析。重复调用为重建。",
		Props: map[string]any{
			"workdir":         map[string]any{"type": "string", "description": "项目根目录"},
			"exts":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "本次参与索引的扩展名（缺省沿用配置/默认集）"},
			"skip_dirs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "用户排除规则（gitignore 语法，最高优先级）"},
			"stack_gitignore": map[string]any{"type": "boolean", "description": "是否额外应用各级 .gitignore / .git/info/exclude / 全局 ignore（缺省沿用配置）"},
		},
		Required: []string{"workdir"},
		Fn:       toolInitialize,
	},
	{
		Name:        "codegraph_status",
		Description: "查询某 workdir 工作区状态：state(disabled/未初始化/indexing/ready/error)、进度、索引规模。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
		},
		Required: []string{"workdir"},
		Fn:       toolStatus,
	},
	{
		Name:        "codegraph_index_paths",
		Description: "按路径增量更新某 workdir 的索引（供常驻 indexer 消费变更队列）：files 重新索引、remove 删除条目；不受支持/被排除/解析失败的文件从索引删除并计入 skipped。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录（绝对路径）"},
			"files":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "需要新增/重新索引的文件绝对路径"},
			"remove":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "需要从索引删除的文件绝对路径"},
		},
		Required: []string{"workdir"},
		Fn:       toolIndexPaths,
	},
	// ---- 查询类（hot=true）----
	{
		Name:        "codegraph_symbol_search",
		Description: "在已索引的某项目内按符号名（子串，忽略大小写）与 kind/文件过滤搜索。kind: func/method/class/type/interface/enum/trait/constructor（大小写不敏感）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"query":   map[string]any{"type": "string", "description": "符号名关键字；省略 = 不按名字过滤"},
			"kind":    map[string]any{"type": "string", "description": "符号类别过滤（大小写不敏感）"},
			"file":    map[string]any{"type": "string", "description": "文件路径子串过滤"},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "返回上限（默认 50，上限 500）"},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "description": "排序后跳过条数（翻页）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolSymbolSearch,
	},
	{
		Name:        "codegraph_get_symbol_info",
		Description: "查询单个符号详情（id 或 file+name 定位；id 优先）：kind/行号/复杂度/签名。多命中返回 symbol+symbols 候选。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"id":      map[string]any{"type": "string", "description": "symbol_search 返回的 id（file:line:name:kind）；提供时忽略 file/name"},
			"file":    map[string]any{"type": "string", "description": "文件路径（精确匹配，大小写不敏感；绝对或相对）"},
			"name":    map[string]any{"type": "string", "description": "符号名（大小写不敏感全名）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolGetSymbolInfo,
	},
	{
		Name:        "codegraph_callers",
		Description: "查「谁调用了它」：按被调名（精确名或最后一段，忽略大小写）找调用方符号；id 优先。名字级启发式（无类型/重载解析，跨语言同名会计入）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"name":    map[string]any{"type": "string", "description": "被调名（如 Foo 或 pkg.Foo）；id 非空时忽略"},
			"id":      map[string]any{"type": "string", "description": "目标符号 id（file:line:name:kind）；与 name 二选一，id 优先"},
			"file":    map[string]any{"type": "string", "description": "只保留该文件内的调用方（路径子串，大小写不敏感）"},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "返回上限（默认 50，上限 500）"},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "description": "排序后跳过条数（翻页）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCallers,
	},
	{
		Name:        "codegraph_callees",
		Description: "查「它调用了谁」：按 id（优先）或 file+name 定位符号，返回其直接调用的目标名（resolved + candidates：0=未索引/外部，1=已解析，>1=同名多处）。名字级启发式（无类型/重载解析）；非函数目标返回 not_callable。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"id":      map[string]any{"type": "string", "description": "符号 id（file:line:name:kind）；提供时忽略 file/name"},
			"file":    map[string]any{"type": "string", "description": "符号所在文件（精确匹配，大小写不敏感；配 name 定位）"},
			"name":    map[string]any{"type": "string", "description": "符号名（大小写不敏感全名；配 file 定位）"},
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "返回上限（默认 50，上限 500）"},
			"offset":  map[string]any{"type": "integer", "minimum": 0, "description": "排序后跳过条数（翻页）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCallees,
	},
	{
		Name:        "codegraph_get_dependency_graph",
		Description: "某项目的文件级依赖：文件 → 静态导入原始串（import/use，未解析）。file 可精确/路径段后缀匹配（大小写不敏感）；默认只返回有导入的文件，支持 limit/offset 分页与 include_empty。",
		Props: map[string]any{
			"workdir":       map[string]any{"type": "string", "description": "项目根目录"},
			"file":          map[string]any{"type": "string", "description": "只返回该文件的依赖（相对/绝对路径，大小写不敏感；也接受路径段后缀）"},
			"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "返回文件上限（默认 50，上限 500）"},
			"offset":        map[string]any{"type": "integer", "minimum": 0, "description": "按文件路径排序后跳过条数（翻页）"},
			"include_empty": map[string]any{"type": "boolean", "description": "是否包含没有导入的文件（默认 false）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolDependencyGraph,
	},
	{
		Name:        "codegraph_find_circular_deps",
		Description: "在可解析为仓库内文件的导入边上枚举简单环（JS/TS 相对、Go module 前缀、Py/Java 包路径、Rust crate 尽力）；路径为 workdir 相对路径，支持 limit/offset 分页与 include_self（默认含自环）。解析是启发式，0 环不代表绝对无循环。",
		Props: map[string]any{
			"workdir":      map[string]any{"type": "string", "description": "项目根目录"},
			"limit":        map[string]any{"type": "integer", "minimum": 1, "maximum": 500, "description": "本页最多返回环数（默认 50，上限 500）"},
			"offset":       map[string]any{"type": "integer", "minimum": 0, "description": "按确定性枚举顺序跳过条数（翻页）"},
			"include_self": map[string]any{"type": "boolean", "description": "是否包含自环（默认 true）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCircularDeps,
	},
	{
		Name:        "codegraph_analyze_complexity",
		Description: "圈复杂度排名：默认只返回 func/method/constructor 中复杂度最高的 topN 个（kind=all 可含全部符号）。复杂度=1+决策点，用于相对排序；file 为大小写不敏感子串过滤；topN 上限 200，返回 total/truncated。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"file":    map[string]any{"type": "string", "description": "文件名/路径子串过滤（大小写不敏感）"},
			"kind":    map[string]any{"type": "string", "enum": []string{"function", "func", "method", "constructor", "all"}, "description": "符号类别（默认 function）"},
			"topN":    map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "返回前 N（默认 20，上限 200）"},
			"minCc":   map[string]any{"type": "integer", "minimum": 1, "description": "最低复杂度（>=，默认 1）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolComplexity,
	},
	{
		Name:        "codegraph_get_module_summary",
		Description: "目录/项目摘要：已索引文件数、语言分布、全部符号数、最高复杂度函数（只含 func/method/constructor，最多 5 条）、解析错误文件数与最近索引时间。path 按路径段边界且大小写不敏感匹配。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"path":    map[string]any{"type": "string", "description": "目录前缀（相对/绝对，大小写不敏感；空=全项目）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolModuleSummary,
	},
}

// ContractTool 是插件从 `.tool.md` 解析出的工具契约面（schema / 描述 / `_meta`）。
// 引擎只负责把它挂到 mcp.Tool 上，不在本包重复解析 md。
type ContractTool struct {
	Name        string
	Description string
	Schema      map[string]any
	Meta        map[string]any
}

// RegisterTools 把全部工具注册进官方 go-sdk server（管理工具标 hot=false）。
// 查询工具的 schema 取本文件内的代码定义（独立引擎可执行文件用）。
func RegisterTools(srv *mcp.Server) error {
	return registerSpecs(srv, true, nil)
}

// RegisterManagementTools 只注册 hot=false 的管理工具（插件进程用；管理工具不纳入 .tool.md）。
func RegisterManagementTools(srv *mcp.Server) error {
	return registerSpecs(srv, false, nil)
}

// RegisterToolsWithContract 用 `.tool.md` 契约注册查询（hot）工具：
// description / inputSchema / `_meta` 全部来自契约；管理工具仍按代码定义注册。
func RegisterToolsWithContract(srv *mcp.Server, contracts []ContractTool) error {
	byName := make(map[string]ContractTool, len(contracts))
	for _, c := range contracts {
		byName[c.Name] = c
	}
	return registerSpecs(srv, true, byName)
}

// registerSpecs 注册工具集。contracts=nil 表示查询工具用代码内 schema；
// 非 nil 时每个 hot 工具必须有契约（缺失即报错，避免漏挂 `_meta`）。
func registerSpecs(srv *mcp.Server, includeHot bool, contracts map[string]ContractTool) error {
	for _, spec := range toolSpecs {
		if spec.Hot && !includeHot {
			continue
		}
		desc := spec.Description
		schema := map[string]any{"type": "object", "properties": spec.Props}
		if len(spec.Required) > 0 {
			schema["required"] = spec.Required
		}
		meta := map[string]any{}
		if spec.Hot {
			meta["hot"] = true
			if contracts != nil {
				c, ok := contracts[spec.Name]
				if !ok {
					return fmt.Errorf("缺少 hot 工具契约（.tool.md）: %s", spec.Name)
				}
				if c.Description != "" {
					desc = c.Description
				}
				if c.Schema != nil {
					schema = c.Schema
				}
				for k, v := range c.Meta {
					meta[k] = v
				}
				meta["hot"] = true
			}
		}
		t := &mcp.Tool{Name: spec.Name, Description: desc, InputSchema: schema}
		t.Meta = mcp.Meta(meta)
		spec := spec
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if req != nil && len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errResult(fmt.Sprintf("参数解析失败: %v", err)), nil
				}
			}
			// 查询类工具：workdir 优先取调用上下文 `_meta.chonkpilot.work_dir`；
			// 显式传参必须绝对且不得越出当前项目（见 ResolveWorkdirFromMeta）。
			if spec.Hot {
				if err := ResolveWorkdirFromMeta(req, args); err != nil {
					return errResult(err.Error()), nil
				}
			}
			out, err := spec.Fn(ctx, args)
			if err != nil {
				return errResult(err.Error()), nil
			}
			return textResult(out)
		})
	}
	return nil
}

// MetaWorkdir 从 MCP 调用 `_meta.chonkpilot.work_dir` 取当前项目工作区（已归一为 '/' 绝对路径）。
func MetaWorkdir(req *mcp.CallToolRequest) string {
	if req == nil || req.Params == nil || req.Params.Meta == nil {
		return ""
	}
	ns, _ := req.Params.Meta["chonkpilot"].(map[string]any)
	if ns == nil {
		return ""
	}
	wd := strings.TrimSpace(getString(ns, "work_dir"))
	if wd == "" {
		return ""
	}
	native := filepath.FromSlash(wd)
	if !filepath.IsAbs(native) {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(native))
}

// ResolveWorkdirFromMeta 统一 workdir 解析（codegraph 查询工具与 vfts_query 共用）：
//   - 未传 workdir → 用 `_meta.chonkpilot.work_dir`；
//   - 传了 workdir → 必须绝对路径、不得 `~` 展开；有 meta 时必须落在当前项目内；
//   - 两者都没有 → 明确参数错误。
func ResolveWorkdirFromMeta(req *mcp.CallToolRequest, args map[string]any) error {
	current := MetaWorkdir(req)
	raw := strings.TrimSpace(getString(args, "workdir"))
	if raw == "" {
		if current == "" {
			return fmt.Errorf("缺少 workdir：调用上下文未提供 _meta.chonkpilot.work_dir，且未传 workdir")
		}
		args["workdir"] = current
		return nil
	}
	wd, err := ValidateWorkdir(raw, current)
	if err != nil {
		return err
	}
	args["workdir"] = wd
	return nil
}

// ValidateWorkdir 校验显式 workdir：绝对路径、拒绝 `~/`，且在 current 非空时不得越出该项目。
func ValidateWorkdir(raw, current string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("workdir 不能为空")
	}
	if strings.HasPrefix(s, "~") {
		return "", fmt.Errorf("workdir 不得使用 ~ 展开（收到 %q）", raw)
	}
	native := filepath.FromSlash(s)
	if !filepath.IsAbs(native) {
		return "", fmt.Errorf("workdir 必须是绝对路径（收到 %q）", raw)
	}
	clean := filepath.Clean(native)
	if current != "" {
		cur := filepath.Clean(filepath.FromSlash(current))
		rel, err := filepath.Rel(cur, clean)
		if err != nil || filepath.IsAbs(rel) || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.HasPrefix(rel, "../") {
			return "", fmt.Errorf("workdir 越出当前项目（%s）", filepath.ToSlash(cur))
		}
	}
	return filepath.ToSlash(clean), nil
}

// Call 进程内直调一个已登记工具（组合插件 / 插件配置页复用；不经过 MCP 传输）。
func Call(ctx context.Context, name string, args map[string]any) (any, error) {
	for _, spec := range toolSpecs {
		if spec.Name == name {
			return spec.Fn(ctx, args)
		}
	}
	return nil, fmt.Errorf("unknown tool: %s", name)
}

// TextToolNames 工具清单（自检/展示用）。
func TextToolNames() []string {
	out := make([]string, 0, len(toolSpecs))
	for _, s := range toolSpecs {
		out = append(out, s.Name)
	}
	return out
}

// ---- 结果/参数辅助 ----

func textResult(v any) (*mcp.CallToolResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult("json 编码失败: " + err.Error()), nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}, IsError: true}
}

func getString(args map[string]any, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func getInt(args map[string]any, k string, def int) int {
	if f, ok := args[k].(float64); ok {
		return int(f)
	}
	return def
}

func getBoolPtr(args map[string]any, k string) *bool {
	if v, ok := args[k].(bool); ok {
		return &v
	}
	return nil
}

func getStrings(args map[string]any, k string) []string {
	v, ok := args[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(v))
	for _, x := range v {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// needWS 从 args 取 workdir 并打开工作区（写路径：确保落盘目录存在）。
func needWS(args map[string]any) (*Workspace, error) {
	wd := getString(args, "workdir")
	if wd == "" {
		return nil, fmt.Errorf("缺少 workdir（项目根目录绝对路径）")
	}
	return Open(wd)
}

// openExisting 查询路径打开工作区：不创建落盘目录。
func openExisting(args map[string]any) (*Workspace, error) {
	wd := getString(args, "workdir")
	if wd == "" {
		return nil, fmt.Errorf("缺少 workdir（项目根目录绝对路径）")
	}
	return OpenReadOnly(wd)
}

// 查询门在「有待索引项」时的有界等待参数（包级变量便于测试覆写）。
var (
	indexReadyWait = 5 * time.Second
	indexReadyPoll = 100 * time.Millisecond
)

// gateIndexStatus 依据 status.json 拦截查询：
//   - 文件不存在 / 读取失败 → (nil, false)，走既有 EnsureReady 兜底（向后兼容）；
//   - error → 返回结构化 error 应答；
//   - indexing 或 pending>0 → 有界等待（默认 5s / 100ms 轮询），就绪后继续 EnsureReady；
//     超时 → 返回结构化 pending 应答（不视为协议错误）；
//   - ready 且 pending==0 → (nil, false)，继续 EnsureReady。
func gateIndexStatus(workDir string) (any, bool) {
	st, err := ReadIndexStatus(workDir)
	if err != nil || st == nil {
		return nil, false
	}
	if st.State == "error" {
		return map[string]any{"status": "error", "state": "error", "message": st.Message}, true
	}
	if st.State == "ready" && st.Pending <= 0 {
		return nil, false
	}
	if st.State != "indexing" && st.Pending <= 0 {
		return nil, false
	}
	deadline := time.Now().Add(indexReadyWait)
	for {
		if !time.Now().Before(deadline) {
			return map[string]any{
				"status":  "pending",
				"state":   "indexing",
				"pending": st.Pending,
				"message": "codegraph 索引仍在处理中，请稍后重试",
			}, true
		}
		time.Sleep(indexReadyPoll)
		next, rerr := ReadIndexStatus(workDir)
		if rerr != nil || next == nil {
			// 等待期间状态文件消失 / 读失败 → 回退到既有 EnsureReady 判定。
			return nil, false
		}
		st = next
		if st.State == "error" {
			return map[string]any{"status": "error", "state": "error", "message": st.Message}, true
		}
		if st.State == "ready" && st.Pending <= 0 {
			return nil, false
		}
	}
}

// readyGate 查询类统一门：返回 (应答, workspace, 是否就绪)。
// 未就绪时返回结构化应答，不视为协议错误。
//
// 查询路径只读：OpenReadOnly 不建目录、EnsureReadyReadOnly 不做 Reconcile 写盘。
func readyGate(args map[string]any) (any, *Workspace, bool) {
	if resp, blocked := gateIndexStatus(getString(args, "workdir")); blocked {
		return resp, nil, false
	}
	w, err := openExisting(args)
	if err != nil {
		return map[string]any{"status": "error", "message": err.Error()}, nil, false
	}
	st, err := w.EnsureReadyReadOnly()
	if st == "ready" && err == nil {
		return nil, w, true
	}
	s := w.Status()
	switch st {
	case "indexing":
		return map[string]any{
			"status": "pending", "state": "indexing",
			"done": s.ProgressDone, "total": s.ProgressTotal,
			"message": "codegraph 索引仍在构建中，请稍后重试",
		}, w, false
	case "error":
		msg := s.Err
		if msg == "" && err != nil {
			// EnsureReady 的瞬时错误（如索引库被另一进程占用）不改 meta → 用原始错误文本，
			// 让 executor / 插件按 lock/timeout 判定并做有界重试。
			msg = err.Error()
		}
		return map[string]any{"status": "error", "state": "error", "message": msg}, w, false
	default:
		return map[string]any{
			"status": "not_initialized", "state": st,
			"message": "该项目尚未初始化 codegraph 索引，请稍后重试或请用户初始化项目索引",
		}, w, false
	}
}

// ---- 管理工具实现 ----

func toolConfigure(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	if err := w.Configure(getBoolPtr(args, "enabled"), getStrings(args, "exts"), getStrings(args, "skip_dirs"), getBoolPtr(args, "stack_gitignore")); err != nil {
		return nil, err
	}
	// mode=clear：清除索引产物（索引是可重建的派生数据；配置存档保留）→ 状态回「未初始化」。
	if mode := getString(args, "mode"); mode == "clear" {
		if err := w.Clear(); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "workdir": w.Dir, "mode": mode, "meta": w.Meta()}, nil
	}
	return map[string]any{"ok": true, "workdir": w.Dir, "meta": w.Meta()}, nil
}

func toolInitialize(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	if err := w.Initialize(getStrings(args, "exts"), getStrings(args, "skip_dirs"), getBoolPtr(args, "stack_gitignore")); err != nil {
		return nil, err
	}
	s := w.Status()
	return map[string]any{
		"ok": true, "workdir": w.Dir,
		"files": s.IndexedFiles, "symbols": s.IndexedSymbols,
		"langCounts": w.ModuleSummary("")["langCounts"],
		"elapsedMs":  time.Since(start).Milliseconds(),
	}, nil
}

func toolStatus(_ context.Context, args map[string]any) (any, error) {
	w, err := openExisting(args)
	if err != nil {
		return nil, err
	}
	_, _ = w.LoadIndexReadOnly()
	return w.Status(), nil
}

// toolIndexPaths 按路径增量更新索引（管理工具，供 resident indexer 调用）。
func toolIndexPaths(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	indexed, removed, skipped, err := w.IndexPaths(getStrings(args, "files"), getStrings(args, "remove"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "indexed": indexed, "removed": removed, "skipped": skipped}, nil
}

// ---- 查询工具实现 ----

func toolSymbolSearch(_ context.Context, args map[string]any) (any, error) {
	q := getString(args, "query")
	file := getString(args, "file")
	kind, limit, offset, err := validateSymbolSearchArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	syms, total := w.SearchSymbolPage(q, kind, file, limit, offset)
	out := map[string]any{
		"total": total, "returned": len(syms),
		"limit": limit, "offset": offset,
		"truncated":     total > offset+len(syms),
		"symbols":       syms,
		"indexed_files": w.IndexedFileCount(),
	}
	if len(syms) == 0 {
		switch {
		case total > 0 && offset >= total:
			out["note"] = "offset_beyond_total"
		case w.IndexedFileCount() == 0:
			out["note"] = "index_empty"
		case file != "" && !w.HasIndexedFile(file):
			out["note"] = "file_not_indexed"
		default:
			out["note"] = "no_match"
		}
	}
	return out, nil
}

// symbolKinds 是契约声明且索引实际产出的符号类别。
var symbolKinds = map[string]bool{
	"func": true, "method": true, "class": true, "type": true,
	"interface": true, "enum": true, "trait": true, "constructor": true,
}

// validateSymbolSearchArgs 严格校验 symbol_search 参数：
//   - 未知键/类型错误显式失败（MCP schema 之外再兜底一次）；
//   - kind 大小写不敏感、必须是已声明类别；
//   - limit 缺省 50、1..500（超出上限截到 500）；offset >= 0。
func validateSymbolSearchArgs(args map[string]any) (kind string, limit, offset int, err error) {
	allowed := map[string]bool{
		"workdir": true, "query": true, "kind": true,
		"file": true, "limit": true, "offset": true,
	}
	for k := range args {
		if !allowed[k] {
			return "", 0, 0, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "query", "kind", "file"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", 0, 0, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	kind = strings.ToLower(strings.TrimSpace(getString(args, "kind")))
	if kind != "" && !symbolKinds[kind] {
		return "", 0, 0, fmt.Errorf("kind 非法: %q（可选 func/method/class/type/interface/enum/trait/constructor）", getString(args, "kind"))
	}
	limit = 50
	if v, ok, verr := symbolSearchIntArg(args, "limit"); verr != nil {
		return "", 0, 0, verr
	} else if ok {
		if v < 1 {
			return "", 0, 0, fmt.Errorf("limit 必须 >= 1")
		}
		limit = v
		if limit > 500 {
			limit = 500
		}
	}
	if v, ok, verr := symbolSearchIntArg(args, "offset"); verr != nil {
		return "", 0, 0, verr
	} else if ok {
		if v < 0 {
			return "", 0, 0, fmt.Errorf("offset 必须 >= 0")
		}
		offset = v
	}
	return kind, limit, offset, nil
}

// symbolSearchIntArg 读取整数参数（JSON 浮点/整数均可；字符串与其他类型报错）。
func symbolSearchIntArg(args map[string]any, key string) (int, bool, error) {
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

func toolGetSymbolInfo(_ context.Context, args map[string]any) (any, error) {
	id, file, name, err := validateGetSymbolInfoArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	var hits []Symbol
	if id != "" {
		hits = w.FindSymbol(id, "", "") // id 优先：忽略 file/name
	} else {
		hits = w.FindSymbol("", file, name)
	}
	if len(hits) == 0 {
		out := map[string]any{
			"status": "not_found", "workdir": w.Dir,
			"message": "未找到符号，可先用 codegraph_symbol_search 确认符号与文件",
		}
		switch {
		case file != "" && !w.HasIndexedFile(file):
			out["note"] = "file_not_indexed"
			out["message"] = "文件未索引或语言不支持；可用 codegraph_symbol_search 查看已索引文件"
		case id != "":
			out["note"] = "id_not_found"
		default:
			out["note"] = "name_not_found"
		}
		return out, nil
	}
	return map[string]any{
		"matches": len(hits), "symbol": hits[0], "symbols": hits,
		"ambiguous": len(hits) > 1, "workdir": w.Dir,
	}, nil
}

// validateGetSymbolInfoArgs 严格校验 get_symbol_info 参数：
// 未知键/类型错误显式失败；id 非空时优先于 file+name，否则必须 file+name 同时提供。
func validateGetSymbolInfoArgs(args map[string]any) (id, file, name string, err error) {
	allowed := map[string]bool{"workdir": true, "id": true, "file": true, "name": true}
	for k := range args {
		if !allowed[k] {
			return "", "", "", fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "id", "file", "name"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", "", "", fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	id = strings.TrimSpace(getString(args, "id"))
	file = strings.TrimSpace(getString(args, "file"))
	name = strings.TrimSpace(getString(args, "name"))
	if id == "" && (file == "" || name == "") {
		return "", "", "", fmt.Errorf("需要 id 或 file+name（id 优先）")
	}
	return id, file, name, nil
}

// toolCallers 查「谁调用了它」：目标由 name 或 id 指定（id 优先）；file 过滤调用方文件。
func toolCallers(_ context.Context, args map[string]any) (any, error) {
	id, name, file, limit, offset, err := validateCallersArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	var target string
	var targetSyms []Symbol
	if id != "" {
		hits := w.FindSymbol(id, "", "")
		if len(hits) == 0 {
			return map[string]any{
				"status": "not_found", "note": "id_not_found", "workdir": w.Dir,
				"message": "未找到目标符号，可先用 codegraph_symbol_search",
			}, nil
		}
		targetSyms = hits[:1]
		target = hits[0].Name
	} else {
		target = name
		targetSyms = w.FindByName(name)
	}
	callers, total := w.CallersPage(target, file, limit, offset)
	out := map[string]any{
		"workdir": w.Dir, "target": target,
		"target_defined": len(targetSyms) > 0, "target_matches": len(targetSyms),
		"total": total, "returned": len(callers),
		"limit": limit, "offset": offset,
		"truncated": total > offset+len(callers),
		"callers":   callers,
	}
	if len(targetSyms) > 0 {
		out["target_symbol"] = targetSyms[0]
		out["target_ambiguous"] = len(targetSyms) > 1
	}
	if len(callers) == 0 {
		switch {
		case total > 0 && offset >= total:
			out["note"] = "offset_beyond_total"
		case len(targetSyms) == 0:
			out["note"] = "target_not_indexed"
		default:
			out["note"] = "no_callers"
		}
	}
	return out, nil
}

// validateCallersArgs 严格校验 callers 参数：
// 未知键/类型错误显式失败；id 非空时优先于 name；limit 1..500（超出截到 500）、offset>=0。
func validateCallersArgs(args map[string]any) (id, name, file string, limit, offset int, err error) {
	allowed := map[string]bool{
		"workdir": true, "id": true, "name": true,
		"file": true, "limit": true, "offset": true,
	}
	for k := range args {
		if !allowed[k] {
			return "", "", "", 0, 0, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "id", "name", "file"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", "", "", 0, 0, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	id = strings.TrimSpace(getString(args, "id"))
	name = strings.TrimSpace(getString(args, "name"))
	file = strings.TrimSpace(getString(args, "file"))
	if id == "" && name == "" {
		return "", "", "", 0, 0, fmt.Errorf("需要 name 或 id（id 优先）")
	}
	limit = 50
	if v, ok, verr := symbolSearchIntArg(args, "limit"); verr != nil {
		return "", "", "", 0, 0, verr
	} else if ok {
		if v < 1 {
			return "", "", "", 0, 0, fmt.Errorf("limit 必须 >= 1")
		}
		limit = v
		if limit > 500 {
			limit = 500
		}
	}
	if v, ok, verr := symbolSearchIntArg(args, "offset"); verr != nil {
		return "", "", "", 0, 0, verr
	} else if ok {
		if v < 0 {
			return "", "", "", 0, 0, fmt.Errorf("offset 必须 >= 0")
		}
		offset = v
	}
	return id, name, file, limit, offset, nil
}

// toolCallees 查「它调用了谁」：由 id（优先）或 file+name 定位符号。
func toolCallees(_ context.Context, args map[string]any) (any, error) {
	id, file, name, limit, offset, err := validateCalleesArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	var hits []Symbol
	if id != "" {
		hits = w.FindSymbol(id, "", "")
	} else {
		hits = w.FindSymbol("", file, name)
	}
	if len(hits) == 0 {
		out := map[string]any{
			"status": "not_found", "workdir": w.Dir,
			"message": "未找到符号，可先用 codegraph_symbol_search",
		}
		switch {
		case file != "" && !w.HasIndexedFile(file):
			out["note"] = "file_not_indexed"
			out["message"] = "文件未索引或语言不支持；可用 codegraph_symbol_search 查看已索引文件"
		case id != "":
			out["note"] = "id_not_found"
		default:
			out["note"] = "name_not_found"
		}
		return out, nil
	}
	target := hits[0]
	refs, total := w.CalleesOf(target, limit, offset)
	out := map[string]any{
		"workdir": w.Dir, "matches": len(hits),
		"symbol": target, "symbols": hits, "ambiguous": len(hits) > 1,
		"total": total, "returned": len(refs),
		"limit": limit, "offset": offset,
		"truncated": total > offset+len(refs),
		"callees":   refs,
	}
	switch {
	case !callableKind(target.Kind):
		out["note"] = "not_callable"
	case len(refs) == 0 && total > 0 && offset >= total:
		out["note"] = "offset_beyond_total"
	case len(refs) == 0:
		out["note"] = "no_calls"
	}
	return out, nil
}

// callableKind 只有函数类符号在索引中保存 Calls。
func callableKind(kind string) bool {
	return kind == "func" || kind == "method" || kind == "constructor"
}

// validateCalleesArgs 严格校验 callees 参数：
// 未知键/类型错误显式失败；id 非空时优先于 file+name；limit 1..500（超出截到 500）、offset>=0。
func validateCalleesArgs(args map[string]any) (id, file, name string, limit, offset int, err error) {
	allowed := map[string]bool{
		"workdir": true, "id": true, "file": true, "name": true,
		"limit": true, "offset": true,
	}
	for k := range args {
		if !allowed[k] {
			return "", "", "", 0, 0, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "id", "file", "name"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", "", "", 0, 0, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	id = strings.TrimSpace(getString(args, "id"))
	file = strings.TrimSpace(getString(args, "file"))
	name = strings.TrimSpace(getString(args, "name"))
	if id == "" && (file == "" || name == "") {
		return "", "", "", 0, 0, fmt.Errorf("需要 id 或 file+name（id 优先）")
	}
	limit = 50
	if v, ok, verr := symbolSearchIntArg(args, "limit"); verr != nil {
		return "", "", "", 0, 0, verr
	} else if ok {
		if v < 1 {
			return "", "", "", 0, 0, fmt.Errorf("limit 必须 >= 1")
		}
		limit = v
		if limit > 500 {
			limit = 500
		}
	}
	if v, ok, verr := symbolSearchIntArg(args, "offset"); verr != nil {
		return "", "", "", 0, 0, verr
	} else if ok {
		if v < 0 {
			return "", "", "", 0, 0, fmt.Errorf("offset 必须 >= 0")
		}
		offset = v
	}
	return id, file, name, limit, offset, nil
}

func toolDependencyGraph(_ context.Context, args map[string]any) (any, error) {
	file, limit, offset, includeEmpty, err := validateDependencyGraphArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	deps, total, totalEdges, fileMatched := w.DependencyGraphPage(file, limit, offset, includeEmpty)
	edges := 0
	for _, imps := range deps {
		edges += len(imps)
	}
	out := map[string]any{
		"workdir": w.Dir,
		"total":   total, "returned": len(deps), "files": len(deps),
		"limit": limit, "offset": offset,
		"truncated": total > offset+len(deps),
		"edges":     edges, "totalEdges": totalEdges,
		"deps": deps,
	}
	if file != "" {
		out["file"] = file
		out["file_matched"] = fileMatched
		if fileMatched && total > 1 {
			out["ambiguous"] = true
		}
	}
	switch {
	case file != "" && !fileMatched:
		out["note"] = "file_not_indexed"
	case len(deps) == 0 && total > 0:
		out["note"] = "offset_beyond_total"
	case total == 0:
		if includeEmpty {
			out["note"] = "no_indexed_files"
		} else {
			out["note"] = "no_dependencies"
		}
	}
	return out, nil
}

// validateDependencyGraphArgs 严格校验依赖图参数：
// 未知键/类型错误显式失败；limit 1..500（超出截到 500）、offset>=0、include_empty 布尔。
func validateDependencyGraphArgs(args map[string]any) (file string, limit, offset int, includeEmpty bool, err error) {
	allowed := map[string]bool{
		"workdir": true, "file": true, "limit": true, "offset": true, "include_empty": true,
	}
	for k := range args {
		if !allowed[k] {
			return "", 0, 0, false, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "file"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", 0, 0, false, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	if v, ok := args["include_empty"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			return "", 0, 0, false, fmt.Errorf("include_empty 必须是布尔值")
		}
		includeEmpty = b
	}
	file = strings.TrimSpace(getString(args, "file"))
	limit = 50
	if v, ok, verr := symbolSearchIntArg(args, "limit"); verr != nil {
		return "", 0, 0, false, verr
	} else if ok {
		if v < 1 {
			return "", 0, 0, false, fmt.Errorf("limit 必须 >= 1")
		}
		limit = v
		if limit > 500 {
			limit = 500
		}
	}
	if v, ok, verr := symbolSearchIntArg(args, "offset"); verr != nil {
		return "", 0, 0, false, verr
	} else if ok {
		if v < 0 {
			return "", 0, 0, false, fmt.Errorf("offset 必须 >= 0")
		}
		offset = v
	}
	return file, limit, offset, includeEmpty, nil
}

func toolCircularDeps(_ context.Context, args map[string]any) (any, error) {
	limit, offset, includeSelf, err := validateCircularDepsArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	cycles, truncated := w.FindCyclesPage(limit, offset, includeSelf)
	out := map[string]any{
		"workdir":    w.Dir,
		"cycleCount": len(cycles), "returned": len(cycles),
		"limit": limit, "offset": offset, "truncated": truncated,
		"cycles": cycles,
		"hint":   "路径为 workdir 相对路径；只含可解析为仓库内文件的导入边（解析为启发式，0 环不代表绝对没有循环依赖）；原始导入见 codegraph_get_dependency_graph。环数潜在指数级，用 limit/offset 翻页。",
	}
	if len(cycles) == 0 {
		if offset > 0 {
			out["note"] = "offset_beyond_total"
		} else {
			out["note"] = "no_cycles"
		}
	}
	return out, nil
}

// validateCircularDepsArgs 严格校验环检测参数：
// 未知键/类型错误显式失败；limit 1..500（超出截到 500）、offset>=0、include_self 布尔。
func validateCircularDepsArgs(args map[string]any) (limit, offset int, includeSelf bool, err error) {
	allowed := map[string]bool{
		"workdir": true, "limit": true, "offset": true, "include_self": true,
	}
	for k := range args {
		if !allowed[k] {
			return 0, 0, false, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	if v, ok := args["workdir"]; ok && v != nil {
		if _, ok := v.(string); !ok {
			return 0, 0, false, fmt.Errorf("workdir 必须是字符串")
		}
	}
	includeSelf = true
	if v, ok := args["include_self"]; ok && v != nil {
		b, isBool := v.(bool)
		if !isBool {
			return 0, 0, false, fmt.Errorf("include_self 必须是布尔值")
		}
		includeSelf = b
	}
	limit = 50
	if v, ok, verr := symbolSearchIntArg(args, "limit"); verr != nil {
		return 0, 0, false, verr
	} else if ok {
		if v < 1 {
			return 0, 0, false, fmt.Errorf("limit 必须 >= 1")
		}
		limit = v
		if limit > 500 {
			limit = 500
		}
	}
	if v, ok, verr := symbolSearchIntArg(args, "offset"); verr != nil {
		return 0, 0, false, verr
	} else if ok {
		if v < 0 {
			return 0, 0, false, fmt.Errorf("offset 必须 >= 0")
		}
		offset = v
	}
	return limit, offset, includeSelf, nil
}

func toolComplexity(_ context.Context, args map[string]any) (any, error) {
	file, kind, topN, minCc, err := validateComplexityArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	syms, total := w.TopComplexityPage(file, kind, topN, minCc)
	indexed, parseErrors := w.IndexCoverage()
	out := map[string]any{
		"workdir": w.Dir,
		"topN":    topN, "returned": len(syms), "total": total,
		"truncated": total > len(syms),
		"minCc":     minCc, "kind": kind,
		"metric":  "cyclomatic",
		"symbols": syms,
		"coverage": map[string]any{
			"indexed_files":     indexed,
			"parse_error_files": parseErrors,
		},
	}
	if total == 0 {
		if file != "" && !w.HasIndexedFile(file) {
			out["note"] = "file_not_indexed"
		} else {
			out["note"] = "no_matches"
		}
	}
	return out, nil
}

// validateComplexityArgs 严格校验复杂度参数：
// 未知键/类型错误显式失败；topN 1..200（超出截到 200）、minCc>=1、kind 取值受限。
func validateComplexityArgs(args map[string]any) (file, kind string, topN, minCc int, err error) {
	allowed := map[string]bool{
		"workdir": true, "file": true, "kind": true, "topN": true, "minCc": true,
	}
	for k := range args {
		if !allowed[k] {
			return "", "", 0, 0, fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "file", "kind"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", "", 0, 0, fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	file = strings.TrimSpace(getString(args, "file"))
	kind = strings.ToLower(strings.TrimSpace(getString(args, "kind")))
	if kind == "" {
		kind = "function"
	}
	switch kind {
	case "function", "func", "method", "constructor", "all":
	default:
		return "", "", 0, 0, fmt.Errorf("kind 必须是 function/func/method/constructor/all")
	}
	topN = 20
	if v, ok, verr := symbolSearchIntArg(args, "topN"); verr != nil {
		return "", "", 0, 0, verr
	} else if ok {
		if v < 1 {
			return "", "", 0, 0, fmt.Errorf("topN 必须 >= 1")
		}
		topN = v
		if topN > 200 {
			topN = 200
		}
	}
	minCc = 1
	if v, ok, verr := symbolSearchIntArg(args, "minCc"); verr != nil {
		return "", "", 0, 0, verr
	} else if ok {
		if v < 1 {
			return "", "", 0, 0, fmt.Errorf("minCc 必须 >= 1")
		}
		minCc = v
	}
	return file, kind, topN, minCc, nil
}

func toolModuleSummary(_ context.Context, args map[string]any) (any, error) {
	path, err := validateModuleSummaryArgs(args)
	if err != nil {
		return nil, err
	}
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	sum := w.ModuleSummary(path)
	sum["workdir"] = w.Dir
	if sum["files"] == 0 && path != "" {
		sum["note"] = "path_not_indexed"
	}
	return sum, nil
}

// validateModuleSummaryArgs 严格校验摘要参数：只允许 workdir/path，且必须为字符串。
func validateModuleSummaryArgs(args map[string]any) (string, error) {
	for k := range args {
		if k != "workdir" && k != "path" {
			return "", fmt.Errorf("不支持的参数: %s", k)
		}
	}
	for _, k := range []string{"workdir", "path"} {
		if v, ok := args[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "", fmt.Errorf("%s 必须是字符串", k)
			}
		}
	}
	return strings.TrimSpace(getString(args, "path")), nil
}
