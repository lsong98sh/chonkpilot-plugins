package server

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	ignore "github.com/lsong98sh/chonkpilot-plugins/shared/ignore"
	zvec "github.com/zvec-ai/zvec-go"
)

// 索引规模约束（对齐 codegraph 的 maxFileBytes 语义）。
var maxFileBytes = 8 << 20 // 8MB

const (
	maxChunkChars = 4000 // 单块最大字符数
	maxChunkLines = 80   // 单块最大行数
)

// ErrNotInitialized 未初始化（查询门控用）。
var ErrNotInitialized = fmt.Errorf("全文索引未初始化（请先 vfts_index）")

// Configure 设置 enabled / exts / skip_dirs / stack_gitignore / docs（引擎侧同步状态，可见性门控由 plugin 完成）。
// exts / skipDirs / stackGitignore / docs 传 nil 表示不改
// （skip_dirs = 用户排除规则（gitignore 语法，最高优先级），空 → 无用户规则；
//
//	docs 各字段 = 指针语义「键存在即覆盖（含空串/零值）」：Endpoint 下发空串即清空旧值
//	（服务不可用 → docsAvailable() 为 false），Token 下发空串即清空内存 token；
//	token 仅内存持有、不落 meta.json）。
func (w *Workspace) Configure(enabled *bool, exts, skipDirs []string, stackGitignore *bool, docs *DocsConfig) error {
	w.mu.Lock()
	if enabled != nil {
		w.meta.Enabled = *enabled
	}
	if exts != nil {
		w.meta.Exts = normalizeExts(exts)
	}
	if skipDirs != nil {
		w.meta.SkipDirs = append([]string{}, skipDirs...)
	}
	if stackGitignore != nil {
		w.meta.StackGitignore = *stackGitignore
	}
	if docs != nil {
		if docs.Enabled != nil {
			w.meta.Docs.Enabled = *docs.Enabled
		}
		if docs.Endpoint != nil {
			w.meta.Docs.Endpoint = strings.TrimRight(*docs.Endpoint, "/")
		}
		if docs.MaxBytes != nil {
			w.meta.Docs.MaxBytes = *docs.MaxBytes
		}
		if docs.TextMaxBytes != nil {
			w.meta.Docs.TextMaxBytes = *docs.TextMaxBytes
		}
		if docs.CacheDir != nil {
			w.meta.Docs.CacheDir = *docs.CacheDir
		}
		if docs.Token != nil {
			w.docsToken = *docs.Token
		}
		w.meta.Docs.Service = docsServiceOf(w.meta.Docs)
	}
	w.mu.Unlock()
	return w.saveMeta()
}

// normalizeExts 归一扩展名列表（去空、补点、小写、去重、排序）。
func normalizeExts(exts []string) []string {
	set := newExtSet(exts)
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// extSet 生效的扩展名集合（未配置 → 默认集）。
func (w *Workspace) extSet() map[string]bool {
	w.mu.Lock()
	exts := w.meta.Exts
	w.mu.Unlock()
	if len(exts) == 0 {
		exts = defaultExts()
	}
	return newExtSet(exts)
}

// excludeOptions 构造遍历排除配置：内置强制 / 默认排除恒生效，stack_gitignore 决定是否叠加
// 各级 .gitignore / .git/info/exclude / 全局 ignore；skip_dirs = 用户规则（最高优先级）。
func (w *Workspace) excludeOptions() ignore.Options {
	w.mu.Lock()
	defer w.mu.Unlock()
	return ignore.Options{
		StackGitignore: w.meta.StackGitignore,
		UserRules:      append([]string{}, w.meta.SkipDirs...),
	}
}

// fileEntry 扫描到的待索引文件。
type fileEntry struct {
	path  string // 相对 workdir（'/' 分隔）
	size  int64
	mtime int64
}

// collectFiles 扫描受支持文件清单（含 stat）。
// 排除走 ignore.WalkDir（gitignore 语义：目录命中忽略即不下降，文件命中即跳过）。
//
// 扩展名分两组：
//   - 非文档类 = 生效 exts（配置集或默认集），单文件上限 maxFileBytes（8MB）；
//   - 文档类（docExts）= **独立一组**，仅当 docs 开启时参与，单文件上限 docMaxBytes（默认 50MB）。
//     文档类**始终**从非文档类集合中剔除（即便被写进 vfts.exts），避免"用户自定义 exts 丢文档支持"
//     的歧义口径 —— 文档支持只由 docs 开关决定。
//
// 返回值 skippedDocs = 因「docs 开启但转换服务不可用」**整批跳过**的文档文件数（不写文件名 chunk）。
func (w *Workspace) collectFiles() (entries []fileEntry, skippedDocs int, err error) {
	exts := w.extSet()
	docs := w.docsMeta()
	for _, e := range docExts() { // 文档类不经普通文本通道
		delete(exts, e)
	}
	var docSet map[string]bool
	if docs.Enabled {
		docSet = newExtSet(docExts())
	}
	docLimit := docMaxBytes(docs)
	root := filepath.FromSlash(w.Dir)
	var out []fileEntry
	err = ignore.WalkDir(root, w.excludeOptions(), func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		isDoc := docSet[ext]
		limit := int64(maxFileBytes)
		if isDoc {
			limit = docLimit
		} else if !exts[ext] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Size() > limit {
			return nil
		}
		if isDoc && !w.docsAvailable() {
			skippedDocs++ // 服务不可用：整批跳过（不收集 → 不写文件名 chunk）
			return nil
		}
		rel := relOf(root, p)
		out = append(out, fileEntry{path: rel, size: info.Size(), mtime: info.ModTime().UnixNano()})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, skippedDocs, nil
}

// chunksOf 读取单文件并切分为文本块（二进制/超限/读失败返回错误或空）。
// 文档类文件不走本函数（见 preConvertDocs / chunksOfDoc）。
func (w *Workspace) chunksOf(e fileEntry) ([]chunk, error) {
	abs := filepath.FromSlash(joinPath(w.Dir, e.path))
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if isBinary(data) {
		return nil, fmt.Errorf("binary file skipped")
	}
	return splitChunks(e.path, string(data), nil), nil
}

// preConvertDocs 对文档类文件做**并发转换**（worker 池）阶段：返回按 entries 下标索引的分块结果。
// 非文档 / docs 未开启 / 无文档文件 → 返回 nil（调用方回落 chunksOf 普通文本通道）。
// **仅转换阶段并发**；zvec 写入仍由调用方串行执行（不破坏现有写入模型）。
func (w *Workspace) preConvertDocs(entries []fileEntry) map[int]docChunkOut {
	if !w.docsMeta().Enabled {
		return nil
	}
	var idx []int
	for i, e := range entries {
		if isDocExt(filepath.Ext(e.path)) {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	return w.convertDocs(entries, idx, defaultDocWorkers)
}

// setState 更新 meta 状态并落盘。
func (w *Workspace) setState(state string) {
	w.mu.Lock()
	w.meta.State = state
	w.mu.Unlock()
	_ = w.saveMeta()
}

func (w *Workspace) markError(msg string) {
	w.mu.Lock()
	w.meta.State = "error"
	w.meta.Err = msg
	w.mu.Unlock()
	_ = w.saveMeta()
}

// IndexedFile 单文件的索引结果（插件回写 file_list 清单用）。
type IndexedFile struct {
	Path   string   `json:"path"`          // 相对 workdir（'/' 分隔）
	Key    string   `json:"key,omitempty"` // 插件侧唯一 key（原样透传）
	DocIDs []string `json:"doc_ids"`       // 该文件在 zvec 中的文档主键（按文件删除用）
	Chunks int      `json:"chunks"`        // 该文件的块数
}

// IndexResult 一次索引（全量/增量）的处理计数与逐文件结果。
// 依据 zvec-go v0.7.0：文档主键为字符串（SetPK(string)），按文件删除 = Delete(pks []string)。
type IndexResult struct {
	Mode          string        `json:"mode"`          // full | incremental
	Added         int           `json:"added"`         // 新增（原无块 → 有块）文件数
	Updated       int           `json:"updated"`       // 更新（先删旧块再重建）文件数
	Removed       int           `json:"removed"`       // 删除文件数（remove 项含旧块）
	RemovedChunks int           `json:"removedChunks"` // 删除的旧块数
	Chunks        int           `json:"chunks"`        // 本次新写入块数
	Indexed       []IndexedFile `json:"indexed"`
}

// IncrementalFile 待增量索引的文件：先按 doc_ids 删除旧块，再重新切块写入。
type IncrementalFile struct {
	Path   string   `json:"path"`
	Key    string   `json:"key,omitempty"`
	DocIDs []string `json:"doc_ids,omitempty"`
}

// IncrementalRemove 待删除的文件（仅按 doc_ids 删旧块，不重新索引）。
type IncrementalRemove struct {
	Key    string   `json:"key,omitempty"`
	DocIDs []string `json:"doc_ids,omitempty"`
}

// Initialize 全量重建 FTS 索引（同步，直到完成并落盘）。
// 重复调用为幂等重建：先关闭并清空集合目录，再重新建索引。
// exts / skipDirs / stackGitignore 传非 nil 时先更新配置。返回逐文件 doc_ids（插件重建清单用）。
func (w *Workspace) Initialize(exts, skipDirs []string, stackGitignore *bool) (*IndexResult, error) {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	if exts != nil || skipDirs != nil || stackGitignore != nil {
		if err := w.Configure(nil, exts, skipDirs, stackGitignore, nil); err != nil {
			return nil, err
		}
	}
	w.mu.Lock()
	w.meta.State = "indexing"
	w.meta.Err = ""
	w.meta.ProgressDone = 0
	w.mu.Unlock()
	if err := w.saveMeta(); err != nil {
		return nil, err
	}

	entries, skippedDocs, err := w.collectFiles()
	if err != nil {
		w.markError(fmt.Sprintf("scan: %v", err))
		return nil, err
	}
	// 文档类并发转换（worker 池；zvec 写入仍串行）
	docsOut := w.preConvertDocs(entries)
	failedDocs := 0
	total := len(entries)
	w.mu.Lock()
	w.meta.ProgressTotal = total
	w.mu.Unlock()

	// 关闭旧集合并清空落盘目录（全量重建）
	w.zmu.Lock()
	defer w.zmu.Unlock()
	if c := w.coll.Load(); c != nil {
		_ = c.Close()
		w.setColl(nil)
	}
	if err := os.RemoveAll(w.collPath()); err != nil {
		w.markError(fmt.Sprintf("clean store: %v", err))
		return nil, err
	}
	// zvec CreateAndOpen 要求目标路径不存在：仅保证父目录存在，不预建集合目录。
	if err := os.MkdirAll(filepath.FromSlash(w.Store), 0o755); err != nil {
		w.markError(fmt.Sprintf("mkdir store: %v", err))
		return nil, err
	}
	coll, err := createCollection(w.collPath())
	if err != nil {
		w.markError(fmt.Sprintf("create collection: %v", err))
		return nil, err
	}

	files, chunks := 0, 0
	seq := 0
	indexed := make([]IndexedFile, 0, len(entries))
	for i, e := range entries {
		cs, degraded := chunkOfEntry(w, docsOut, i, e)
		if degraded {
			failedDocs++
		}
		var ids []string
		if len(cs) > 0 {
			ids = make([]string, 0, len(cs))
			for j := range cs {
				seq++
				cs[j].pk = strconv.Itoa(seq)
				ids = append(ids, cs[j].pk)
			}
			if err := insertChunks(coll, cs); err != nil {
				_ = coll.Close()
				w.markError(fmt.Sprintf("insert %s: %v", e.path, err))
				return nil, err
			}
			files++
			chunks += len(cs)
		}
		indexed = append(indexed, IndexedFile{Path: e.path, DocIDs: ids, Chunks: len(cs)})
		if (i+1)%50 == 0 {
			w.mu.Lock()
			w.meta.ProgressDone = i + 1
			w.mu.Unlock()
			_ = w.saveMeta()
		}
	}
	if err := coll.Flush(); err != nil {
		_ = coll.Close()
		w.markError(fmt.Sprintf("flush: %v", err))
		return nil, err
	}
	w.setColl(coll)

	w.mu.Lock()
	w.meta.State = "ready"
	w.meta.Err = ""
	w.meta.ProgressDone = total
	w.meta.ProgressTotal = total
	w.meta.FileCount = files
	w.meta.ChunkCount = chunks
	w.meta.Tokenizer = tokenizer
	w.meta.LastIndexedAt = time.Now().UnixNano()
	w.meta.NextPK = int64(seq) // 全量重建后主键序列重置为 N
	w.meta.Docs.Service = docsServiceOf(w.meta.Docs)
	w.meta.Docs.Skipped = skippedDocs
	w.meta.Docs.Failed = failedDocs
	w.mu.Unlock()
	if err := w.saveMeta(); err != nil {
		return nil, err
	}
	return &IndexResult{Mode: "full", Added: files, Chunks: chunks, Indexed: indexed}, nil
}

// chunkOfEntry 取单个文件的分块：文档类用并发转换结果（docsOut），其余走普通文本通道。
// degraded=true 表示该文档转换失败、已降级为「仅文件名」。
func chunkOfEntry(w *Workspace, docsOut map[int]docChunkOut, i int, e fileEntry) (cs []chunk, degraded bool) {
	if o, ok := docsOut[i]; ok {
		return o.chunks, o.degraded
	}
	cs, err := w.chunksOf(e)
	if err != nil {
		return nil, false
	}
	return cs, false
}

// Incremental 按文件增量：先按 files.doc_ids ∪ removes.doc_ids 删除旧块，再对 files
// 逐文件重新切块写入（主键用单调递增的纯数字串，避免与既有主键冲突）。
// 集合不存在时新建（等价首次索引）；不传 files/remove 请改用 Initialize（全量重建）。
func (w *Workspace) Incremental(files []IncrementalFile, removes []IncrementalRemove) (*IndexResult, error) {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	w.mu.Lock()
	w.meta.State = "indexing"
	w.meta.Err = ""
	w.meta.ProgressDone = 0
	w.meta.ProgressTotal = len(files)
	w.mu.Unlock()
	if err := w.saveMeta(); err != nil {
		return nil, err
	}

	w.zmu.Lock()
	defer w.zmu.Unlock()
	coll, err := w.ensureCollLocked()
	if err != nil {
		w.markError(fmt.Sprintf("open collection: %v", err))
		return nil, err
	}

	// 1) 收集待删旧块主键（更新文件旧块 + 删除文件旧块，去重）
	delIDs := make([]string, 0, len(files)+len(removes))
	seen := map[string]bool{}
	addDel := func(ids []string) {
		for _, id := range ids {
			if id != "" && !seen[id] {
				seen[id] = true
				delIDs = append(delIDs, id)
			}
		}
	}
	for _, f := range files {
		addDel(f.DocIDs)
	}
	for _, r := range removes {
		addDel(r.DocIDs)
	}
	removedChunks := len(delIDs)
	if removedChunks > 0 {
		if err := deleteChunks(coll, delIDs); err != nil {
			w.markError(fmt.Sprintf("delete old chunks: %v", err))
			return nil, err
		}
	}

	// 2) 逐文件重建块（文档类先并发转换，随后串行写 zvec）
	incEntries := make([]fileEntry, 0, len(files))
	for _, f := range files {
		rel := w.norm(f.Path)
		incEntries = append(incEntries, fileEntry{path: rel})
	}
	docsOut := w.preConvertDocs(incEntries)
	failedDocs := 0
	res := &IndexResult{Mode: "incremental", RemovedChunks: removedChunks, Indexed: make([]IndexedFile, 0, len(files))}
	base := time.Now().UnixNano()
	if w.Meta().NextPK > base {
		base = w.Meta().NextPK
	}
	seq := base
	fileDelta := 0
	for fi, f := range files {
		rel := incEntries[fi].path
		if rel == "" {
			continue
		}
		cs, degraded := chunkOfEntry(w, docsOut, fi, incEntries[fi])
		if degraded {
			failedDocs++
		}
		var ids []string
		if len(cs) > 0 {
			ids = make([]string, 0, len(cs))
			for j := range cs {
				seq++
				cs[j].pk = strconv.FormatInt(seq, 10)
				ids = append(ids, cs[j].pk)
			}
			if err := insertChunks(coll, cs); err != nil {
				w.markError(fmt.Sprintf("insert %s: %v", rel, err))
				return nil, err
			}
		}
		res.Indexed = append(res.Indexed, IndexedFile{Path: rel, Key: f.Key, DocIDs: ids, Chunks: len(cs)})
		res.Chunks += len(cs)
		if len(f.DocIDs) > 0 {
			res.Updated++
			if len(ids) == 0 {
				fileDelta-- // 内容变成空/二进制 → 该文件不再有块
			}
		} else if len(ids) > 0 {
			res.Added++
			fileDelta++
		}
	}
	if err := coll.Flush(); err != nil {
		w.markError(fmt.Sprintf("flush: %v", err))
		return nil, err
	}

	// 3) 删除文件计数（按 remove 项含旧块计），并更新聚合计数
	for _, r := range removes {
		if len(r.DocIDs) > 0 {
			res.Removed++
			fileDelta--
		}
	}

	w.mu.Lock()
	w.meta.State = "ready"
	w.meta.Err = ""
	w.meta.ProgressDone = len(files)
	w.meta.ProgressTotal = len(files)
	if w.meta.FileCount+fileDelta >= 0 {
		w.meta.FileCount += fileDelta
	}
	if w.meta.ChunkCount+res.Chunks-removedChunks >= 0 {
		w.meta.ChunkCount += res.Chunks - removedChunks
	}
	w.meta.Tokenizer = tokenizer
	w.meta.LastIndexedAt = time.Now().UnixNano()
	w.meta.Docs.Service = docsServiceOf(w.meta.Docs)
	w.meta.Docs.Failed = failedDocs
	if seq > w.meta.NextPK {
		w.meta.NextPK = seq
	}
	w.mu.Unlock()
	if err := w.saveMeta(); err != nil {
		return nil, err
	}
	return res, nil
}

// ensureCollLocked 确保集合已打开（须持 zmu）：内存有则复用；盘上有则打开；否则新建。
func (w *Workspace) ensureCollLocked() (*zvec.Collection, error) {
	if c := w.coll.Load(); c != nil {
		return c, nil
	}
	if _, err := os.Stat(w.collPath()); err == nil {
		coll, err := openCollection(w.collPath())
		if err != nil {
			return nil, err
		}
		w.setColl(coll)
		return coll, nil
	}
	// zvec CreateAndOpen 要求目标路径不存在：仅保证父目录存在。
	if err := os.MkdirAll(filepath.FromSlash(w.Store), 0o755); err != nil {
		return nil, err
	}
	coll, err := createCollection(w.collPath())
	if err != nil {
		return nil, err
	}
	w.setColl(coll)
	return coll, nil
}

// EnsureReady 查询前的统一就绪处理：确认集合可用并返回状态。
// state 返回 ready/indexing/not_initialized/error。
//
// 快路径（已载入）：直接读 meta 状态返回，**不取 recMu/zmu** —— Initialize/Incremental 全程持
// recMu（含长 collectFiles/建索引/写入），若快路径也取 recMu 则重建期间查询门被整段阻塞，违背
// 「免阻塞查询门」；仅在**未载入**时才取 recMu（就绪/重建单飞）做懒加载/校验落盘集合。
func (w *Workspace) EnsureReady() (string, error) {
	if w.isLoaded() {
		return w.readState()
	}
	w.recMu.Lock()
	defer w.recMu.Unlock()
	w.zmu.Lock()
	if w.coll.Load() == nil { // 等锁期间可能已被载入 → 二次确认，避免重复打开
		if _, err := os.Stat(w.collPath()); err != nil {
			w.zmu.Unlock()
			return "not_initialized", ErrNotInitialized
		}
		coll, err := openCollection(w.collPath())
		if err != nil {
			w.zmu.Unlock()
			return "error", fmt.Errorf("打开 vfts 集合失败: %w", err)
		}
		w.setColl(coll)
	}
	w.zmu.Unlock()
	return w.readState()
}

// readState 读取 meta 状态并映射为查询门控结果。
func (w *Workspace) readState() (string, error) {
	w.mu.Lock()
	st := w.meta.State
	errMsg := w.meta.Err
	w.mu.Unlock()
	switch st {
	case "error":
		return "error", fmt.Errorf("索引出错：%s", errMsg)
	case "indexing":
		return "indexing", nil
	case "ready":
		return "ready", nil
	default:
		return "not_initialized", ErrNotInitialized
	}
}

// Query 执行 FTS 检索，返回文件级命中（去重后按相关度，最多 topK 个文件）。
// match = 自然语言匹配串；expr = 布尔/高级表达式（二者至少其一）。
func (w *Workspace) Query(match, expr string, topK int, pathFilter string) ([]ftsHit, error) {
	if topK <= 0 {
		topK = 20
	}
	w.zmu.Lock()
	defer w.zmu.Unlock()
	coll := w.coll.Load()
	if coll == nil {
		return nil, ErrNotInitialized
	}
	// 多取文档块，再做文件级去重，保证 topK 个文件有候选
	rawTopK := topK * 5
	if rawTopK > 500 {
		rawTopK = 500
	}
	hits, err := searchChunks(coll, match, expr, rawTopK)
	if err != nil {
		return nil, err
	}
	filter := strings.ToLower(w.norm(pathFilter))
	seen := map[string]bool{}
	out := make([]ftsHit, 0, topK)
	for _, h := range hits {
		if filter != "" && !strings.Contains(strings.ToLower(h.Path), filter) {
			continue
		}
		if seen[h.Path] {
			continue
		}
		seen[h.Path] = true
		h.Path = w.abs(h.Path)
		out = append(out, h)
		if len(out) >= topK {
			break
		}
	}
	return out, nil
}
