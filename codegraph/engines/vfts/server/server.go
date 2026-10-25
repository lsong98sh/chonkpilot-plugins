package server

import (
	"context"
	"encoding/json"
	"fmt"
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
		Name: "vfts_configure",
		Description: "配置某 workdir 的 vfts 全文索引工作区：enabled 标识、参与索引的扩展名 exts、排除规则 skip_dirs" +
			"（gitignore 语法，最高优先级）、是否叠加 gitignore 体系 stack_gitignore（可见性门控由调用方/plugin 负责，引擎仅记录与存档）；" +
			"docs 段（可选）接入「文档转换服务」（Office/PDF → 文本）：docs 开关、doc_endpoint（如 http://127.0.0.1:7317）、" +
			"doc_token（X-Chonk-Token，仅内存、不落盘）、doc_max_bytes（文档单文件上限，默认 50MiB）、" +
			"doc_text_max_bytes（转换文本上限，默认 2MiB）、doc_cache_dir（转换缓存目录，默认 <workdir>/.chonkpilot/vfts/doc_text）。" +
			"docs 各字段为「键存在即覆盖（含空值）」：doc_endpoint/doc_token 下发空串 = 服务不可用（清掉旧值）。" +
			"文档类扩展名（docx/xlsx/pptx/pdf）独立成组，仅在 docs 开启时参与索引。",
		Props: map[string]any{
			"workdir":            map[string]any{"type": "string", "description": "项目根目录（绝对路径）"},
			"enabled":            map[string]any{"type": "boolean", "description": "是否启用（缺省不改）"},
			"exts":               map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "参与索引的扩展名（如 [\".go\",\".txt\",\".md\"]；缺省不改，替换式；不含文档类）"},
			"skip_dirs":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "用户排除规则（gitignore 语法，每项一条；优先级最高；缺省不改）"},
			"stack_gitignore":    map[string]any{"type": "boolean", "description": "是否额外应用各级 .gitignore / .git/info/exclude / 全局 ignore（缺省不改）"},
			"docs":               map[string]any{"type": "boolean", "description": "是否开启文档索引（Office/PDF，需转换服务；缺省不改）"},
			"doc_endpoint":       map[string]any{"type": "string", "description": "文档转换服务地址（如 http://127.0.0.1:7317；空 = 未配置服务）"},
			"doc_token":          map[string]any{"type": "string", "description": "转换服务鉴权 token（X-Chonk-Token；仅内存，不落 meta.json）"},
			"doc_max_bytes":      map[string]any{"type": "integer", "description": "文档类单文件上限字节（默认 50MiB）"},
			"doc_text_max_bytes": map[string]any{"type": "integer", "description": "单文件转换文本上限字节（默认 2MiB）"},
			"doc_cache_dir":      map[string]any{"type": "string", "description": "转换缓存目录（默认 <workdir>/.chonkpilot/vfts/doc_text）"},
		},
		Required: []string{"workdir"},
		Fn:       toolConfigure,
	},
	{
		Name: "vfts_index",
		Description: "对某 workdir 建/更新 FTS 全文索引（同步，直到完成并落盘到 <workdir>/.chonkpilot/vfts/）：" +
			"不传 files/remove 时全量重建（索引源码 + .txt + .md 等纯文本，按 gitignore 语义排除，含 .chonkpilot；" +
			"非文档类单文件 >8MB 或二进制跳过、文档类 >doc_max_bytes 跳过（需 docs 已开启且转换服务可用）；重复调用为幂等重建）；" +
			"传 files/remove 时按文件增量（仅处理这些文件）。" +
			"返回 mode 与计数（added/updated/removed/removedChunks/newChunks），以及逐文件 indexed[{path,key,doc_ids,chunks}]（供调用方回写清单）。",
		Props: map[string]any{
			"workdir":         map[string]any{"type": "string", "description": "项目根目录"},
			"exts":            map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "本次参与索引的扩展名（缺省沿用配置/默认集）"},
			"skip_dirs":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "用户排除规则（gitignore 语法，最高优先级）"},
			"stack_gitignore": map[string]any{"type": "boolean", "description": "是否额外应用各级 .gitignore / .git/info/exclude / 全局 ignore（缺省沿用配置）"},
			"files":           map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "增量：仅索引这些文件；每项 {path(绝对/相对路径), key(可选), doc_ids(该文件旧块主键数组，先删后插；新文件缺省为空)}"},
			"remove":          map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "增量：删除这些文件的旧块；每项 {key(可选), doc_ids(该文件旧块主键数组)}"},
		},
		Required: []string{"workdir"},
		Fn:       toolIndex,
	},
	{
		Name:        "vfts_status",
		Description: "查询某 workdir 的 vfts 工作区状态：state(未初始化/indexing/ready/error)、进度、索引文件数/文档块数、参与扩展名与跳过目录；文档索引态：docsEnabled / docsService(running|absent) / docsSkipped / docsFailed / docsParserVersion。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
		},
		Required: []string{"workdir"},
		Fn:       toolStatus,
	},
	{
		Name: "vfts_dict_get",
		Description: "查看**系统级** jieba 词典（全工作区共用一份，无项目级/用户级覆盖）：返回 dict_dir（系统词典目录）、" +
			"user_dict_path（自定义词文件）、user_dict（自定义词全文）、word_count（词条数）、base_dicts（内嵌基础词典文件名清单）。",
		Props:    map[string]any{},
		Required: []string{},
		Fn:       toolDictGet,
	},
	{
		Name: "vfts_dict_set",
		Description: "编辑**系统级** jieba 自定义词（覆写 user_dict.txt，每行一词、`#` 注释行忽略）。" +
			"词典变更后**必须重新索引**（vfts_index 全量重建）才生效。返回 {ok, word_count}。",
		Props: map[string]any{
			"user_dict": map[string]any{"type": "string", "description": "自定义词全文（每行一词；`#` 开头为注释）"},
		},
		Required: []string{"user_dict"},
		Fn:       toolDictSet,
	},
	// ---- 查询类（hot=true）----
	{
		Name: "vfts_query",
		Description: "在某 workdir 的 vfts 全文索引中检索：match 为自然语言匹配串，query 为布尔/高级表达式" +
			"（二者至少给其一，query 不支持字段前缀）；返回命中文件 + 片段/行号（topK，按相关度）。",
		Props: map[string]any{
			"workdir": map[string]any{"type": "string", "description": "项目根目录"},
			"match":   map[string]any{"type": "string", "description": "自然语言匹配串（如：全文索引）"},
			"query":   map[string]any{"type": "string", "description": "布尔/高级表达式（如：fox AND brown；不支持 content: 字段前缀）"},
			"topK":    map[string]any{"type": "integer", "description": "返回命中文件数上限（默认 20）"},
			"path":    map[string]any{"type": "string", "description": "文件路径子串过滤（可选）"},
		},
		Required: []string{"workdir"},
		Hot:      true,
		Fn:       toolQuery,
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

// ToolNames 工具清单（自检/展示用）。
func ToolNames() []string {
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

// getStringPtr 取字符串参数并区分「键未下发」（nil）与「下发空串」（&""）。
func getStringPtr(args map[string]any, k string) *string {
	if v, ok := args[k].(string); ok {
		return &v
	}
	return nil
}

// getInt64Ptr 取整数参数并区分「键未下发」（nil）与「下发 0」（&0）。
func getInt64Ptr(args map[string]any, k string) *int64 {
	if f, ok := args[k].(float64); ok {
		n := int64(f)
		return &n
	}
	return nil
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

// toStrings 任意 JSON 数组 → 字符串切片（忽略非字符串项）。
func toStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// getIncrementalFiles 解析 files 参数：每项可为字符串（仅路径）或 {path,key,doc_ids} 对象。
func getIncrementalFiles(args map[string]any, k string) []IncrementalFile {
	arr, ok := args[k].([]any)
	if !ok {
		return nil
	}
	out := make([]IncrementalFile, 0, len(arr))
	for _, x := range arr {
		switch v := x.(type) {
		case string:
			out = append(out, IncrementalFile{Path: v})
		case map[string]any:
			out = append(out, IncrementalFile{
				Path:   getString(v, "path"),
				Key:    getString(v, "key"),
				DocIDs: toStrings(v["doc_ids"]),
			})
		}
	}
	return out
}

// getIncrementalRemoves 解析 remove 参数：每项 {key,doc_ids}。
func getIncrementalRemoves(args map[string]any, k string) []IncrementalRemove {
	arr, ok := args[k].([]any)
	if !ok {
		return nil
	}
	out := make([]IncrementalRemove, 0, len(arr))
	for _, x := range arr {
		if v, ok := x.(map[string]any); ok {
			out = append(out, IncrementalRemove{Key: getString(v, "key"), DocIDs: toStrings(v["doc_ids"])})
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
				"message": "索引仍在处理中，请稍后重试",
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
			"message": "vfts 全文索引仍在构建中，请稍后重试或先 vfts_index",
		}, w, false
	case "error":
		msg := s.Err
		if msg == "" && err != nil {
			// EnsureReady 的瞬时错误（如集合被另一进程占用）不改 meta → 用原始错误文本，
			// 让 executor / 插件按 lock/timeout 判定并做有界重试。
			msg = err.Error()
		}
		return map[string]any{"status": "error", "state": "error", "message": msg}, w, false
	default:
		return map[string]any{
			"status": "not_initialized", "state": st,
			"message": "该项目尚未初始化 vfts 全文索引，请先 vfts_index {workdir}",
		}, w, false
	}
}

// ---- 管理工具实现 ----

func toolConfigure(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	if err := w.Configure(getBoolPtr(args, "enabled"), getStrings(args, "exts"), getStrings(args, "skip_dirs"),
		getBoolPtr(args, "stack_gitignore"), docsConfigFromArgs(args)); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "workdir": w.Dir, "meta": w.Meta()}, nil
}

// docsConfigFromArgs 从工具参数构建文档接入配置；无任何 docs 字段 → nil（不改）。
// 各字段用指针承载「键是否下发」：下发空串/0 即覆盖（清旧值），与 [Configure] 的
// 「键存在即覆盖（含空值）」语义一致。
func docsConfigFromArgs(args map[string]any) *DocsConfig {
	cfg := &DocsConfig{
		Enabled:      getBoolPtr(args, "docs"),
		Endpoint:     getStringPtr(args, "doc_endpoint"),
		Token:        getStringPtr(args, "doc_token"),
		MaxBytes:     getInt64Ptr(args, "doc_max_bytes"),
		TextMaxBytes: getInt64Ptr(args, "doc_text_max_bytes"),
		CacheDir:     getStringPtr(args, "doc_cache_dir"),
	}
	if cfg.Enabled == nil && cfg.Endpoint == nil && cfg.Token == nil && cfg.MaxBytes == nil &&
		cfg.TextMaxBytes == nil && cfg.CacheDir == nil {
		return nil
	}
	return cfg
}

func toolIndex(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	files := getIncrementalFiles(args, "files")
	removes := getIncrementalRemoves(args, "remove")
	start := time.Now()
	var res *IndexResult
	if len(files) == 0 && len(removes) == 0 {
		res, err = w.Initialize(getStrings(args, "exts"), getStrings(args, "skip_dirs"), getBoolPtr(args, "stack_gitignore"))
	} else {
		// 增量沿用现有配置（exts/skip_dirs 传 nil 表示不改）
		res, err = w.Incremental(files, removes)
	}
	if err != nil {
		return nil, err
	}
	s := w.Status()
	return map[string]any{
		"ok": true, "workdir": w.Dir, "mode": res.Mode,
		"files": s.IndexedFiles, "chunks": s.ChunkCount,
		"added": res.Added, "updated": res.Updated, "removed": res.Removed,
		"removedChunks": res.RemovedChunks, "newChunks": res.Chunks,
		"indexed":     res.Indexed,
		"tokenizer":   s.Tokenizer,
		"docsService": s.DocsService,
		"docsSkipped": s.DocsSkipped,
		"docsFailed":  s.DocsFailed,
		"elapsedMs":   time.Since(start).Milliseconds(),
		"store":       w.Store,
	}, nil
}

func toolStatus(_ context.Context, args map[string]any) (any, error) {
	w, err := needWS(args)
	if err != nil {
		return nil, err
	}
	_ = w.ReloadMeta()
	return w.Status(), nil
}

// ---- 查询工具实现 ----

func toolQuery(_ context.Context, args map[string]any) (any, error) {
	resp, w, ok := readyGate(args)
	if !ok {
		return resp, nil
	}
	match := getString(args, "match")
	expr := getString(args, "query")
	if match == "" && expr == "" {
		return nil, fmt.Errorf("需要 match 或 query 至少其一")
	}
	topK := getInt(args, "topK", 20)
	hits, err := w.Query(match, expr, topK, getString(args, "path"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"workdir": w.Dir, "total": len(hits), "hits": hits}, nil
}
