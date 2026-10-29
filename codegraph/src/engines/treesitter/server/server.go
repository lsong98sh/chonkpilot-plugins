package server

import (
	"context"
	"encoding/json"
	"fmt"
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
		Description: "对某 workdir 全量建索引（同步，直到完成并落盘到 <workdir>/.chonkpilot/codegraph/）。重复调用为重建。",
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
		Description: "在已索引的某项目内按符号名（子串/前缀，忽略大小写）与 kind/文件过滤搜索。kind: func/method/class/type/interface/enum/trait/constructor。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"query":   map[string]any{"type": "string", "description": "符号名关键字"},
			"kind":    map[string]any{"type": "string", "description": "符号类别过滤"},
			"file":    map[string]any{"type": "string", "description": "文件路径子串过滤"},
			"limit":   map[string]any{"type": "integer", "description": "返回上限（默认 50）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolSymbolSearch,
	},
	{
		Name:        "codegraph_get_symbol_info",
		Description: "查询单个符号详情（id 或 file+name 定位）：kind/行号/复杂度/签名。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"id":      map[string]any{"type": "string", "description": "symbol_search 返回的 id（file:line:name:kind）"},
			"file":    map[string]any{"type": "string", "description": "文件绝对路径"},
			"name":    map[string]any{"type": "string", "description": "符号名"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolGetSymbolInfo,
	},
	{
		Name:        "codegraph_callers",
		Description: "查「谁调用了它」：在已索引项目内按被调名（精确名或最后一段，忽略大小写，故 pkg.Foo 与 Foo 同目标）找调用方符号。语义为调用点文本的**名字级启发式**（无类型/重载解析，仅覆盖已索引文件中的直接调用）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"name":    map[string]any{"type": "string", "description": "被调名（如 Foo 或 pkg.Foo）"},
			"id":      map[string]any{"type": "string", "description": "目标符号 id（file:line:name:kind）；与 name 二选一，id 优先"},
			"file":    map[string]any{"type": "string", "description": "只保留该文件内的调用方（路径子串过滤）"},
			"limit":   map[string]any{"type": "integer", "description": "返回上限（默认 50）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCallers,
	},
	{
		Name:        "codegraph_callees",
		Description: "查「它调用了谁」：按 id 或 file+name 定位符号，返回其直接调用的目标名（含 resolved 与解析到的 file/line）。语义为调用点文本的**名字级启发式**（无类型/重载解析）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"id":      map[string]any{"type": "string", "description": "符号 id（file:line:name:kind）"},
			"file":    map[string]any{"type": "string", "description": "符号所在文件（配 name 定位）"},
			"name":    map[string]any{"type": "string", "description": "符号名（配 file 定位）"},
			"limit":   map[string]any{"type": "integer", "description": "返回上限（默认 50）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCallees,
	},
	{
		Name:        "codegraph_get_dependency_graph",
		Description: "某项目的文件级依赖：文件 → import/use/require 目标（原始串）。file 过滤可单查。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"file":    map[string]any{"type": "string", "description": "只返回该文件的依赖"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolDependencyGraph,
	},
	{
		Name:        "codegraph_find_circular_deps",
		Description: "在可解析为仓库内文件的导入边上找环（JS/TS 相对、Go module 前缀、Py/Java 包路径、Rust crate 尽力）；解析不到的不参与。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolCircularDeps,
	},
	{
		Name:        "codegraph_analyze_complexity",
		Description: "圈复杂度分析：默认返回项目内复杂度最高 topN 的 func/method；file 限定单文件。复杂度为决策点启发式。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"file":    map[string]any{"type": "string", "description": "只看该文件"},
			"topN":    map[string]any{"type": "integer", "description": "返回前 N（默认 20）"},
			"minCc":   map[string]any{"type": "integer", "description": "最低复杂度（默认 1）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolComplexity,
	},
	{
		Name:        "codegraph_get_module_summary",
		Description: "目录/项目摘要：文件数、语言分布、符号数、最高复杂度函数（基于索引）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"path":    map[string]any{"type": "string", "description": "目录前缀（可选）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolModuleSummary,
	},
}

// RegisterTools 把全部工具注册进官方 go-sdk server（管理工具标 hot=false）。
func RegisterTools(srv *mcp.Server) error {
	for _, spec := range toolSpecs {
		schema := map[string]any{"type": "object", "properties": spec.Props}
		if len(spec.Required) > 0 {
			schema["required"] = spec.Required
		}
		t := &mcp.Tool{Name: spec.Name, Description: spec.Description, InputSchema: schema}
		t.Meta = mcp.Meta{}
		if spec.Hot {
			t.Meta["hot"] = true
		}
		spec := spec
		srv.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return errResult(fmt.Sprintf("参数解析失败: %v", err)), nil
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

// needWS 从 args 取 workdir 并打开工作区。
func needWS(args map[string]any) (*Workspace, error) {
	wd := getString(args, "workdir")
	if wd == "" {
		return nil, fmt.Errorf("缺少 workdir（项目根目录绝对路径）")
	}
	return Open(wd)
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
func readyGate(args map[string]any) (any, *Workspace, bool) {
	if resp, blocked := gateIndexStatus(getString(args, "workdir")); blocked {
		return resp, nil, false
	}
	w, err := needWS(args)
	if err != nil {
		return map[string]any{"status": "error", "message": err.Error()}, nil, false
	}
	st, err := w.EnsureReady()
	if st == "ready" && err == nil {
		return nil, w, true
	}
	s := w.Status()
	switch st {
	case "indexing":
		return map[string]any{
			"status": "pending", "state": "indexing",
			"done": s.ProgressDone, "total": s.ProgressTotal,
			"message": "codegraph 索引仍在构建中，请稍后重试或先 codegraph_initialize",
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
			"message": "该项目尚未初始化 codegraph 索引，请先 codegraph_initialize {workdir}",
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
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	_, _ = w.LoadIndex()
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
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	q := getString(args, "query")
	kind := getString(args, "kind")
	file := getString(args, "file")
	limit := getInt(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	syms := w.SearchSymbol(q, kind, file, limit)
	total := len(syms)
	return map[string]any{"total": total, "symbols": syms}, nil
}

func toolGetSymbolInfo(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	id := getString(args, "id")
	file := getString(args, "file")
	name := getString(args, "name")
	if id == "" && (file == "" || name == "") {
		return nil, fmt.Errorf("需要 id 或 file+name")
	}
	hits := w.FindSymbol(id, file, name)
	if len(hits) == 0 {
		return map[string]any{"status": "not_found", "message": "未找到符号，可先用 codegraph_symbol_search"},
			nil
	}
	return map[string]any{"matches": len(hits), "symbol": hits[0]}, nil
}

// toolCallers 查「谁调用了它」：目标由 name 或 id 指定（id 优先）；file 过滤调用方文件。
func toolCallers(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	target := strings.TrimSpace(getString(args, "name"))
	id := getString(args, "id")
	if target == "" && id == "" {
		return nil, fmt.Errorf("需要 name 或 id")
	}
	if target == "" {
		hits := w.FindSymbol(id, "", "")
		if len(hits) == 0 {
			return map[string]any{"status": "not_found", "message": "未找到目标符号，可先用 codegraph_symbol_search"}, nil
		}
		target = hits[0].Name
	}
	limit := getInt(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	callers := w.Callers(target, getString(args, "file"), limit)
	return map[string]any{"workdir": w.Dir, "target": target, "total": len(callers), "callers": callers}, nil
}

// toolCallees 查「它调用了谁」：由 id 或 file+name 定位符号。
func toolCallees(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	id := getString(args, "id")
	file := getString(args, "file")
	name := getString(args, "name")
	if id == "" && (file == "" || name == "") {
		return nil, fmt.Errorf("需要 id 或 file+name")
	}
	hits := w.FindSymbol(id, file, name)
	if len(hits) == 0 {
		return map[string]any{"status": "not_found", "message": "未找到符号，可先用 codegraph_symbol_search"}, nil
	}
	limit := getInt(args, "limit", 50)
	if limit <= 0 {
		limit = 50
	}
	refs := w.Callees(id, file, name, limit)
	return map[string]any{"workdir": w.Dir, "symbol": hits[0], "total": len(refs), "callees": refs}, nil
}

func toolDependencyGraph(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	rows := w.Imports(getString(args, "file"))
	return map[string]any{"workdir": w.Dir, "files": len(rows), "deps": rows}, nil
}

func toolCircularDeps(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	cycles := w.FindCycles()
	return map[string]any{
		"workdir":    w.Dir,
		"cycleCount": len(cycles),
		"cycles":     cycles,
		"note":       "仅含可解析为仓库内文件的导入边；原始导入见 codegraph_get_dependency_graph",
	}, nil
}

func toolComplexity(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	topN := getInt(args, "topN", 20)
	minCc := getInt(args, "minCc", 1)
	if topN <= 0 {
		topN = 20
	}
	syms := w.TopComplexity(getString(args, "file"), topN, minCc)
	return map[string]any{"workdir": w.Dir, "topN": len(syms), "symbols": syms}, nil
}

func toolModuleSummary(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	sum := w.ModuleSummary(getString(args, "path"))
	sum["workdir"] = w.Dir
	return sum, nil
}
