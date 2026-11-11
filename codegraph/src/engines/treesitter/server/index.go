package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-util/extx"
	ignore "github.com/lsong98sh/chonkpilot-util/ignore"
	bolt "go.etcd.io/bbolt"
)

var maxFileBytes = 8 << 20 // 8MB

type srcEntry struct {
	path  string
	lang  string
	mtime int64
	size  int64
}

// Configure 设置 enabled / exts / skip_dirs / stack_gitignore（引擎侧同步状态，可见性门控由 plugin 完成）。
// exts / skipDirs / stackGitignore 传 nil 表示不改；传空切片 = 清空
// （exts 空 → 回落全部受支持语言；skip_dirs = 用户排除规则（gitignore 语法），空 → 无用户规则）。
func (w *Workspace) Configure(enabled *bool, exts, skipDirs []string, stackGitignore *bool) error {
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
	w.mu.Unlock()
	return w.saveMeta()
}

// normalizeExts 归一扩展名列表（去空、补点、小写、去重、排序）。
func normalizeExts(exts []string) []string {
	return extx.Normalize(exts)
}

// newExtSet 扩展名集合（小写、补点归一）。
func newExtSet(exts []string) map[string]bool {
	return extx.Set(exts)
}

// supportedExts 引擎支持的全部语言扩展名（默认集 = 各语言 ext 并集）。
func supportedExts() []string {
	out := make([]string, 0, len(extLang))
	for e := range extLang {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// extSet 生效的扩展名集合（未配置 → 全部受支持语言）。
func (w *Workspace) extSet() map[string]bool {
	w.mu.Lock()
	exts := w.meta.Exts
	w.mu.Unlock()
	if len(exts) == 0 {
		exts = supportedExts()
	}
	return newExtSet(exts)
}

// Status 返回工作区状态视图（工具/plugin 查询用）。
type Status struct {
	Workdir        string   `json:"workdir"`
	State          string   `json:"state"`
	Enabled        bool     `json:"enabled"`
	ProgressDone   int      `json:"progressDone"`
	ProgressTotal  int      `json:"progressTotal"`
	Err            string   `json:"err,omitempty"`
	LastIndexedAt  int64    `json:"lastIndexedAt"`
	SkipDirs       []string `json:"skipDirs,omitempty"`
	StackGitignore bool     `json:"stackGitignore,omitempty"`
	Exts           []string `json:"exts,omitempty"`
	IndexedFiles   int      `json:"indexedFiles"`
	IndexedSymbols int      `json:"indexedSymbols"`
	Loaded         bool     `json:"loaded"` // 当前进程内存已载入索引
}

func (w *Workspace) Status() Status {
	w.mu.Lock()
	m := w.meta
	loaded := w.ix != nil
	var files, syms int
	if loaded {
		files = len(w.ix.Files)
		syms = len(w.ix.syms)
	}
	w.mu.Unlock()
	exts := m.Exts
	if len(exts) == 0 {
		exts = supportedExts()
	}
	return Status{Workdir: w.Dir, State: m.State, Enabled: m.Enabled,
		ProgressDone: m.ProgressDone, ProgressTotal: m.ProgressTotal, Err: m.Err,
		LastIndexedAt: m.LastIndexedAt, SkipDirs: append([]string{}, m.SkipDirs...),
		StackGitignore: m.StackGitignore,
		Exts:           append([]string{}, exts...),
		IndexedFiles:   files, IndexedSymbols: syms, Loaded: loaded}
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

// collectSourceFiles 扫描受支持源码文件清单（含 stat）。
// 排除走 ignore.WalkDir（gitignore 语义：目录命中忽略即不下降，文件命中即跳过）。
func (w *Workspace) collectSourceFiles() ([]srcEntry, error) {
	exts := w.extSet()
	var out []srcEntry
	err := ignore.WalkDir(w.Dir, w.excludeOptions(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !exts[ext] {
			return nil
		}
		lang, ok := LangForExt(ext)
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > int64(maxFileBytes) {
			return nil
		}
		// store 一律相对 workdir（'/' 分隔）
		rel := strings.TrimPrefix(filepath.ToSlash(p), filepath.ToSlash(w.Dir)+"/")
		out = append(out, srcEntry{path: rel, lang: lang,
			mtime: info.ModTime().UnixNano(), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func (w *Workspace) parseEntry(e srcEntry) (*FileInfo, error) {
	abs := filepath.FromSlash(joinPath(w.Dir, e.path))
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	// 交给 tree-sitter 前统一转 UTF-8（编码识别实现 = chonkpilot-util/encodingx）。
	fi, err := parseFile(e.lang, e.path, indexSource(raw))
	if err != nil {
		return nil, err
	}
	fi.Mtime = e.mtime
	fi.Size = e.size
	return fi, nil
}

// Initialize 全量索引（同步，直到完成并落盘；进度写入 meta 供跨进程 status 读取）。
// exts / skipDirs / stackGitignore 传非 nil 时先更新配置（exts 空切片 = 回落全部受支持语言）。
func (w *Workspace) Initialize(exts, skipDirs []string, stackGitignore *bool) error {
	if exts != nil || skipDirs != nil || stackGitignore != nil {
		if err := w.Configure(nil, exts, skipDirs, stackGitignore); err != nil {
			return err
		}
	}
	// 标记 indexing
	w.mu.Lock()
	w.meta.State = "indexing"
	w.meta.Err = ""
	w.meta.ProgressDone = 0
	w.mu.Unlock()
	if err := w.saveMeta(); err != nil {
		return err
	}

	entries, err := w.collectSourceFiles()
	if err != nil {
		w.markError(fmt.Sprintf("scan: %v", err))
		return err
	}
	total := len(entries)
	w.mu.Lock()
	w.meta.ProgressTotal = total
	w.mu.Unlock()

	next := newIndex()
	for i, e := range entries {
		fi, perr := w.parseEntry(e)
		if perr == nil && fi != nil {
			next.AddFile(fi)
		}
		if (i+1)%100 == 0 {
			w.mu.Lock()
			w.meta.ProgressDone = i + 1
			w.mu.Unlock()
			_ = w.saveMeta()
		}
	}
	w.mu.Lock()
	w.ix = next
	w.meta.State = "ready"
	w.meta.ProgressDone = total
	w.meta.ProgressTotal = total
	w.meta.LastIndexedAt = time.Now().UnixNano()
	w.mu.Unlock()
	if err := w.SaveIndex(); err != nil {
		w.markError(fmt.Sprintf("save index: %v", err))
		return err
	}
	return w.saveMeta()
}

func (w *Workspace) markError(msg string) {
	w.mu.Lock()
	w.meta.State = "error"
	w.meta.Err = msg
	w.mu.Unlock()
	_ = w.saveMeta()
}

// Clear 清除该 workdir 的索引产物：删除落盘索引库 index.db（及遗留 index.json）+ 内存索引置空，
// 状态回「未初始化」。配置存档（enabled/exts/skip_dirs）不是索引产物，予以保留；索引可经 Initialize 重建。
// 与 EnsureReady/Reconcile 共用 recMu 单飞（避免与并发查询的差分/自愈互踩）。
func (w *Workspace) Clear() error {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	w.mu.Lock()
	w.ix = nil
	w.meta.State = ""
	w.meta.ProgressDone = 0
	w.meta.ProgressTotal = 0
	w.meta.Err = ""
	w.meta.LastIndexedAt = 0
	w.mu.Unlock()
	if err := w.removeIndexArtifacts(); err != nil {
		return err
	}
	return w.saveMeta()
}

// cloneIndex 浅拷贝索引（文件条目指针复用）。
func cloneIndex(ix *Index) *Index {
	n := newIndex()
	for _, fi := range ix.Files {
		n.AddFile(fi)
	}
	return n
}

// Reconcile 自愈增量：基线快检（mtime/size）→ 差异增删改 → 落盘。
// 返回 changed 表示发生过更新。未初始化返回 ErrNotInitialized。
var ErrNotInitialized = fmt.Errorf("索引未初始化（请先 codegraph_initialize）")

func (w *Workspace) Reconcile() (changed bool, err error) {
	ok, err := w.LoadIndex()
	if err != nil {
		return false, err
	}
	if !ok || w.ix == nil {
		return false, ErrNotInitialized
	}
	entries, err := w.collectSourceFiles()
	if err != nil {
		return false, err
	}
	w.mu.Lock()
	old := w.ix
	meta := w.meta
	w.mu.Unlock()

	// 快检：数量一致 + 每个文件 mtime/size 一致
	snap := map[string]srcEntry{}
	for _, e := range entries {
		snap[e.path] = e
	}
	if len(old.Files) == len(snap) {
		clean := true
		for p, fi := range old.Files {
			e, ok := snap[p]
			if !ok || e.mtime != fi.Mtime || e.size != fi.Size {
				clean = false
				break
			}
		}
		if clean {
			if meta.State != "ready" {
				w.mu.Lock()
				w.meta.State = "ready"
				w.meta.Err = ""
				w.meta.LastIndexedAt = time.Now().UnixNano()
				w.mu.Unlock()
				return false, w.saveMeta()
			}
			return false, nil
		}
	}

	next := cloneIndex(old)
	updated := 0
	var upserts []*FileInfo // 新增/修改（bolt 单文件 put，覆盖旧条目）
	var removed []string    // 已删除（bolt 单文件 delete）
	for p, e := range snap {
		fi, ok := old.Files[p]
		if ok && fi.Mtime == e.mtime && fi.Size == e.size {
			continue // 未变，直接复用
		}
		nf, perr := w.parseEntry(e)
		if perr != nil || nf == nil {
			continue
		}
		if ok {
			next.RemoveFile(p)
		}
		next.AddFile(nf)
		upserts = append(upserts, nf)
		updated++
	}
	for p := range old.Files {
		if _, ok := snap[p]; !ok {
			next.RemoveFile(p)
			removed = append(removed, p)
			updated++
		}
	}
	if updated > 0 {
		w.mu.Lock()
		w.ix = next
		w.meta.State = "ready"
		w.meta.Err = ""
		w.meta.LastIndexedAt = time.Now().UnixNano()
		w.mu.Unlock()
		if err := w.saveIndexDelta(upserts, removed); err != nil {
			w.markError(fmt.Sprintf("save index: %v", err))
			return true, err
		}
		_ = w.saveMeta()
	}
	return updated > 0, nil
}

// EnsureReady 查询前的统一就绪处理：载入索引 + 自愈快检。
// state 返回 ready/indexing/not_initialized/error。
// recMu 单飞：同一 workdir 多个并发查询只执行一次差分，其余串行等待后直接复用。
//
// 注意：本方法会经 Reconcile 写索引（重建/自愈），**只供写路径/管理工具**使用；
// 查询类工具请用 EnsureReadyReadOnly。
func (w *Workspace) EnsureReady() (string, error) {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	_, err := w.LoadIndex()
	if err != nil {
		return "error", err
	}
	w.mu.Lock()
	st := w.meta.State
	hasIx := w.ix != nil
	w.mu.Unlock()
	if !hasIx {
		return "not_initialized", ErrNotInitialized
	}
	if st == "error" {
		return "error", fmt.Errorf("索引出错：%s", w.Meta().Err)
	}
	if st == "indexing" {
		return "indexing", nil
	}
	if _, err := w.Reconcile(); err != nil {
		return "not_initialized", err
	}
	return "ready", nil
}

// LoadIndexReadOnly 只读恢复索引：不删除遗留/损坏文件、不改写 meta（查询工具专用）。
//
// 与 LoadIndex 的差异：损坏/旧格式只视为「未初始化」，绝不落盘清理；bolt 写锁超时仍上抛
// （瞬时错误，供上层有界重试）。
func (w *Workspace) LoadIndexReadOnly() (bool, error) {
	w.mu.Lock()
	loaded := w.ix != nil
	w.mu.Unlock()
	if loaded {
		return true, nil
	}
	ix, present, err := w.loadIndexFromDB()
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return false, err
		}
		if errors.Is(err, bolt.ErrInvalid) || errors.Is(err, errCorruptDB) {
			return false, nil
		}
		return false, err
	}
	if !present {
		return false, nil
	}
	w.mu.Lock()
	w.ix = ix
	w.mu.Unlock()
	return true, nil
}

// EnsureReadyReadOnly 查询专用就绪门：只读载入 + 状态判定，**不做 Reconcile、不写盘**。
func (w *Workspace) EnsureReadyReadOnly() (string, error) {
	w.recMu.Lock()
	defer w.recMu.Unlock()
	_, err := w.LoadIndexReadOnly()
	if err != nil {
		return "error", err
	}
	w.mu.Lock()
	st := w.meta.State
	hasIx := w.ix != nil
	w.mu.Unlock()
	if !hasIx {
		return "not_initialized", ErrNotInitialized
	}
	if st == "error" {
		return "error", fmt.Errorf("索引出错：%s", w.Meta().Err)
	}
	if st == "indexing" {
		return "indexing", nil
	}
	return "ready", nil
}

// Imports 文件级依赖视图（原始 import 目标，键为绝对路径；fileFilter 为空=全部）。
func (w *Workspace) Imports(fileFilter string) map[string][]string {
	w.mu.Lock()
	ix := w.ix
	w.mu.Unlock()
	rows := map[string][]string{}
	if ix == nil {
		return rows
	}
	fr := w.norm(fileFilter)
	for p, fi := range ix.Files {
		if fileFilter != "" && p != fr {
			continue
		}
		rows[w.abs(p)] = append([]string{}, fi.Imports...)
	}
	return rows
}

// DependencyGraphPage 文件级依赖的分页视图（键为绝对路径）。
//
// fileFilter 为空 = 全部已索引文件；非空时按 workdir 相对路径大小写不敏感全等匹配，
// 也接受路径段后缀（如 "main.go" 命中 "src/main.go"）。includeEmpty=false 时跳过无 import
// 的文件。返回 total = 符合条件文件数（未截断），totalEdges = 这些文件的总依赖边数；
// fileMatched 表示 fileFilter 是否命中任一已索引文件。
func (w *Workspace) DependencyGraphPage(fileFilter string, limit, offset int, includeEmpty bool) (deps map[string][]string, total, totalEdges int, fileMatched bool) {
	w.mu.Lock()
	ix := w.ix
	w.mu.Unlock()
	deps = map[string][]string{}
	if ix == nil {
		return deps, 0, 0, false
	}
	filter := strings.ToLower(strings.TrimSpace(w.norm(fileFilter)))
	fileMatched = fileFilter == ""
	var rels []string
	for p, fi := range ix.Files {
		if filter != "" {
			lp := strings.ToLower(p)
			if lp != filter && !strings.HasSuffix(lp, "/"+filter) {
				continue
			}
			fileMatched = true
		}
		if len(fi.Imports) == 0 && !includeEmpty {
			continue
		}
		rels = append(rels, p)
	}
	sort.Strings(rels)
	total = len(rels)
	for _, p := range rels {
		totalEdges += len(ix.Files[p].Imports)
	}
	if offset >= len(rels) {
		return deps, total, totalEdges, fileMatched
	}
	end := len(rels)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	for _, p := range rels[offset:end] {
		deps[w.abs(p)] = append([]string{}, ix.Files[p].Imports...)
	}
	return deps, total, totalEdges, fileMatched
}

// FindCycles 返回全部可解析的简单环（相对路径，按最小节点+邻接顺序枚举，带自环）。
// 大图下环数可能指数级；对外工具应使用 FindCyclesPage 限制数量。
func (w *Workspace) FindCycles() [][]string {
	cycles, _ := w.FindCyclesPage(0, 0, true)
	return cycles
}

// FindCyclesPage 确定性枚举简单环并分页。
//
//   - 路径为 workdir 相对路径；闭环在末尾重复起点（如 ["a.go","b.go","a.go"]）。
//   - 每个环只从其字典序最小节点出发枚举（其余节点 >= 起点），避免旋转重复。
//   - includeSelf=true 时包含文件 import 自己的自环。
//   - limit<=0 表示不限（仅在内部/测试使用）；否则最多枚举 offset+limit+1 条，
//     返回 truncated 表示是否还有下一页。
func (w *Workspace) FindCyclesPage(limit, offset int, includeSelf bool) (cycles [][]string, truncated bool) {
	w.mu.Lock()
	ix := w.ix
	w.mu.Unlock()
	if ix == nil {
		return [][]string{}, false
	}
	indexed := map[string]bool{}
	nodes := make([]string, 0, len(ix.Files))
	for p := range ix.Files {
		indexed[p] = true
		nodes = append(nodes, p)
	}
	sort.Strings(nodes)
	order := make(map[string]int, len(nodes))
	for i, p := range nodes {
		order[p] = i
	}
	adj := map[string][]string{}
	for _, p := range nodes {
		fi := ix.Files[p]
		seen := map[string]bool{}
		for _, imp := range fi.Imports {
			t := resolveImport(fi.Lang, p, imp, w.Dir, indexed)
			if t == "" || seen[t] {
				continue
			}
			if t == p && !includeSelf {
				continue
			}
			seen[t] = true
			adj[p] = append(adj[p], t)
		}
		sort.Strings(adj[p])
	}

	capN := 0 // 0 = 不限
	if limit > 0 {
		capN = offset + limit + 1 // 多枚举 1 条用于判断 truncated
	}
	var out [][]string
	var path []string
	inPath := map[string]bool{}
	stopped := false

	var dfs func(start string, startIdx int, u string)
	dfs = func(start string, startIdx int, u string) {
		if stopped {
			return
		}
		for _, v := range adj[u] {
			if order[v] < startIdx {
				continue // 每个环只从最小节点出发
			}
			if v == start {
				if len(path) >= 1 {
					cyc := append(append([]string{}, path...), start)
					out = append(out, cyc)
					if capN > 0 && len(out) >= capN {
						stopped = true
						return
					}
				}
				continue
			}
			if inPath[v] {
				continue
			}
			inPath[v] = true
			path = append(path, v)
			dfs(start, startIdx, v)
			path = path[:len(path)-1]
			delete(inPath, v)
			if stopped {
				return
			}
		}
	}

	for si, start := range nodes {
		if stopped {
			break
		}
		inPath[start] = true
		path = []string{start}
		dfs(start, si, start)
		path = path[:0]
		delete(inPath, start)
	}

	if limit > 0 {
		if len(out) > offset+limit {
			truncated = true
			out = out[:offset+limit]
		}
		if offset > 0 {
			if offset >= len(out) {
				out = [][]string{}
			} else {
				out = out[offset:]
			}
		}
	}
	if out == nil {
		out = [][]string{}
	}
	return out, truncated
}
