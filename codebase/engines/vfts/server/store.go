package server

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	zvec "github.com/zvec-ai/zvec-go"
)

// zvec FTS 集合的字段名与查询实现（阶段 1：仅 FTS，无向量字段）。
const (
	fieldPath    = "path"    // 相对 workdir 的文件路径（'/' 分隔）
	fieldLine    = "line"    // 片段起始行号（1 基）
	fieldContent = "content" // 纯文本内容（FTS 索引字段）
	fieldLoc     = "loc"     // 文档定位（可选：页码 / sheet 名 / slide 序号；非文档类留空）
)

// ftsFilters FTS 词过滤器：lowercase（英文大小写归一；jieba 分词器本身不对 ASCII 做归一）。
var ftsFilters = []string{"lowercase"}

var zvecOnce sync.Once
var zvecInitErr error

// ensureZvec 进程内只初始化 zvec 一次：先物化系统级 jieba 词典并设为进程默认，
// 再 zvec.Initialize（保证任何集合创建/查询前词典目录已就位）。
func ensureZvec() error {
	zvecOnce.Do(func() {
		dir, err := ensureSystemDict()
		if err != nil {
			zvecInitErr = err
			return
		}
		zvec.SetDefaultJiebaDictDir(dir)
		zvecInitErr = zvec.Initialize(nil)
	})
	return zvecInitErr
}

// shutdownZvec 关闭 zvec（幂等；未初始化则跳过）。
func shutdownZvec() {
	if zvecInitErr == nil && zvec.IsInitialized() {
		_ = zvec.Shutdown()
	}
}

// buildSchema 构建 FTS 集合 schema：path + line + content(FTS)；文档主键另经 SetPK 设置。
// 返回的 cleanup 用于释放 schema 与字段/索引参数句柄（AddField 内部已复制）。
func buildSchema() (*zvec.CollectionSchema, func()) {
	schema := zvec.NewCollectionSchema("vfts")

	pathField := zvec.NewFieldSchema(fieldPath, zvec.DataTypeString, false, 0)
	_ = schema.AddField(pathField)

	lineField := zvec.NewFieldSchema(fieldLine, zvec.DataTypeInt32, false, 0)
	_ = schema.AddField(lineField)

	contentField := zvec.NewFieldSchema(fieldContent, zvec.DataTypeString, false, 0)
	// jieba 参数经 extra_params 显式下发（与进程默认同源；系统级词典 + 自定义词）。
	contentParams, err := zvec.NewFTSIndexParams(tokenizer, ftsFilters, jiebaExtraParams())
	if err == nil {
		_ = contentField.SetIndexParams(contentParams)
	}
	_ = schema.AddField(contentField)

	// loc：文档定位（可选字符串，nullable=true 允许缺省；非文档类留空；不进 FTS 索引）。
	locField := zvec.NewFieldSchema(fieldLoc, zvec.DataTypeString, true, 0)
	_ = schema.AddField(locField)

	cleanup := func() {
		if contentParams != nil {
			contentParams.Destroy()
		}
		pathField.Destroy()
		lineField.Destroy()
		contentField.Destroy()
		locField.Destroy()
		schema.Destroy()
	}
	return schema, cleanup
}

// jiebaExtraParams 组装 jieba 分词器 extra_params（cut_mode=search 为默认，显式写出以锁口径）：
// 系统级词典目录 + 系统级自定义词文件。JSON 组装失败（理论不可达）→ 空串（回落进程默认词典）。
func jiebaExtraParams() string {
	b, err := json.Marshal(map[string]string{
		"jieba_dict_dir": SystemDictDir(),
		"user_dict_path": UserDictPath(),
		"cut_mode":       "search",
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// createCollection 新建集合并打开（覆盖前须由调用方清空目录）。
func createCollection(path string) (*zvec.Collection, error) {
	if err := ensureZvec(); err != nil {
		return nil, err
	}
	schema, cleanup := buildSchema()
	defer cleanup()
	return zvec.CreateAndOpen(path, schema, nil)
}

// openCollection 打开已有集合。
func openCollection(path string) (*zvec.Collection, error) {
	if err := ensureZvec(); err != nil {
		return nil, err
	}
	return zvec.Open(path, nil)
}

// chunk 一个索引文档块（一块 = 一个 zvec doc）。
type chunk struct {
	pk   string // 主键：全量重建时分配的递增序号（zvec 主键不允许多数字符）
	path string // 相对路径
	line int    // 起始行号（1 基）
	text string // 文本内容
	loc  string // 文档定位（可选：页码 / sheet 名 / slide 序号；非文档类空）
}

// insertChunks 分批写入文档块。
func insertChunks(coll *zvec.Collection, chunks []chunk) error {
	const batch = 64
	for i := 0; i < len(chunks); i += batch {
		end := i + batch
		if end > len(chunks) {
			end = len(chunks)
		}
		docs := make([]*zvec.Doc, 0, end-i)
		for _, c := range chunks[i:end] {
			doc := zvec.NewDoc()
			doc.SetPK(c.pk)
			_ = doc.AddStringField(fieldPath, c.path)
			_ = doc.AddInt32Field(fieldLine, int32(c.line))
			_ = doc.AddStringField(fieldContent, c.text)
			if c.loc != "" {
				_ = doc.AddStringField(fieldLoc, c.loc)
			}
			docs = append(docs, doc)
		}
		_, err := coll.Insert(docs)
		for _, d := range docs {
			d.Destroy()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// deleteChunks 按主键批量删除文档块（用于按文件增量替换/删除旧块）。
// 依据 zvec-go v0.7.0：`func (c *Collection) Delete(pks []string) (*WriteResult, error)`
// （C-API `zvec_collection_delete(collection, pks, pk_count, *success, *error)`）。
func deleteChunks(coll *zvec.Collection, pks []string) error {
	const batch = 64
	for i := 0; i < len(pks); i += batch {
		end := i + batch
		if end > len(pks) {
			end = len(pks)
		}
		if _, err := coll.Delete(pks[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// ftsHit 一条 FTS 命中（文件级）。
type ftsHit struct {
	Path    string  `json:"path"`
	Line    int     `json:"line"`
	Snippet string  `json:"snippet"`
	Score   float32 `json:"score"`
	Loc     string  `json:"loc,omitempty"` // 文档定位（页码 / sheet 名 / slide 序号；非文档类空）
}

// searchChunks 执行一次 FTS 查询，返回原始文档块命中（按相关度）。
func searchChunks(coll *zvec.Collection, match, expr string, topK int) ([]ftsHit, error) {
	q := zvec.NewSearchQuery()
	defer q.Destroy()
	if err := q.SetFieldName(fieldContent); err != nil {
		return nil, err
	}
	if err := q.SetTopK(topK); err != nil {
		return nil, err
	}
	// 旧集合可能尚无 loc 字段（schema 变更前的落盘）→ 回退到不含 loc 的输出字段。
	if err := q.SetOutputFields([]string{fieldPath, fieldLine, fieldContent, fieldLoc}); err != nil {
		_ = q.SetOutputFields([]string{fieldPath, fieldLine, fieldContent})
	}

	terms := queryTerms(match, expr)
	fts := zvec.NewFTS()
	defer fts.Destroy()
	if match != "" {
		if err := fts.SetMatchString(match); err != nil {
			return nil, err
		}
	}
	if expr != "" {
		if err := fts.SetQueryString(expr); err != nil {
			return nil, err
		}
	}
	if err := q.SetFTS(fts); err != nil {
		return nil, err
	}

	docs, err := coll.Query(q)
	if err != nil {
		return nil, err
	}
	defer zvec.FreeDocs(docs)

	out := make([]ftsHit, 0, len(docs))
	for _, d := range docs {
		p, _ := d.GetStringField(fieldPath)
		ln, _ := d.GetInt32Field(fieldLine)
		txt, _ := d.GetStringField(fieldContent)
		loc, _ := d.GetStringField(fieldLoc)
		snippet, off := snippetOf(txt, terms, 240)
		out = append(out, ftsHit{Path: p, Line: int(ln) + off, Snippet: snippet, Score: d.GetScore(), Loc: loc})
	}
	return out, nil
}

var termRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// queryTerms 从 match 串与布尔表达式中提取检索词（用于本地片段定位）。
func queryTerms(match, expr string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		low := strings.ToLower(t)
		if low == "and" || low == "or" || low == "not" || seen[low] {
			return
		}
		seen[low] = true
		out = append(out, t)
	}
	if match != "" {
		for _, t := range strings.Fields(match) {
			add(t)
		}
	}
	if expr != "" {
		for _, t := range termRe.FindAllString(expr, -1) {
			add(t)
		}
	}
	return out
}

// snippetOf 在文本块内定位首个含检索词的行，返回（片段, 相对行偏移）。
// 无命中行（布尔跨行/无词）时退回块内首个非空行。
func snippetOf(text string, terms []string, maxChars int) (string, int) {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		low := strings.ToLower(ln)
		for _, t := range terms {
			if strings.Contains(low, strings.ToLower(t)) {
				return truncate(strings.TrimSpace(ln), maxChars), i
			}
		}
	}
	for i, ln := range lines {
		if s := strings.TrimSpace(ln); s != "" {
			return truncate(s, maxChars), i
		}
	}
	return truncate(strings.TrimSpace(text), maxChars), 0
}

// truncate 按字符（rune）截断并加省略号。
func truncate(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "…"
}
