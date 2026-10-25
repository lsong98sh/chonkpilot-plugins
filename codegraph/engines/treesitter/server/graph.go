// Package server 实现 chonkpilot-codegraph 引擎 lib：
// 多语言(tree-sitter 官方 grammar)符号索引，按 workdir 组织工作区并持久化到
// <workdir>/.chonkpilot/codegraph/（索引本体 = bbolt 单文件 index.db，工作区元信息 = meta.json），
// 支持增量自愈刷新。查询工具显式带 workdir 参数；进程可退化为"每次调用拉起"或由宿主长驻复用。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	bolt "go.etcd.io/bbolt"
)

// Symbol 一个被索引的定义符号。
type Symbol struct {
	ID         string `json:"id"`
	File       string `json:"file"`
	Kind       string `json:"kind"` // func/method/class/type/interface/enum/trait/constructor/...
	Lang       string `json:"lang"`
	Name       string `json:"name"`
	Line       int    `json:"line"`
	EndLine    int    `json:"endLine"`
	Complexity int    `json:"complexity"` // 圈复杂度（决策点启发式，>=1；仅 func/method/constructor）
	Signature  string `json:"signature"`
	// Calls 该符号内直接调用的被调名（最后一段，去重 + 排序；仅 func/method/constructor）。
	// 语义为调用点文本的**名字级启发式**（无类型/重载解析），如 `a.b.Foo()` → "Foo"。
	Calls []string `json:"calls,omitempty"`
}

// FileInfo 单文件索引条目。
type FileInfo struct {
	Path    string   `json:"path"`
	Lang    string   `json:"lang"`
	Mtime   int64    `json:"mtime"`
	Size    int64    `json:"size"`
	Symbols []Symbol `json:"symbols"`
	Imports []string `json:"imports"`
	HasErr  bool     `json:"hasErr"`
}

// Index 一个 workdir 的符号索引（在内存中不可变追加；reconcile 时整体替换）。
// 落盘 = bbolt 单文件 index.db（bucket: meta/files/symbols），见 store.go。
type Index struct {
	Files  map[string]*FileInfo
	syms   []Symbol
	byName map[string][]int // 小写名 → syms 下标
	byCall map[string][]int // 小写被调名（最后一段）→ 调用方 syms 下标（Calls 反向索引）
}

func newIndex() *Index {
	return &Index{Files: map[string]*FileInfo{}, byName: map[string][]int{}, byCall: map[string][]int{}}
}

func (ix *Index) AddFile(fi *FileInfo) {
	ix.Files[fi.Path] = fi
	for _, s := range fi.Symbols {
		s.ID = symbolID(fi.Path, s.Line, s.Name, s.Kind)
		ix.syms = append(ix.syms, s)
		idx := len(ix.syms) - 1
		ix.byName[strings.ToLower(s.Name)] = append(ix.byName[strings.ToLower(s.Name)], idx)
		for _, c := range s.Calls {
			k := strings.ToLower(strings.TrimSpace(c))
			if k == "" {
				continue
			}
			ix.byCall[k] = append(ix.byCall[k], idx)
		}
	}
}

func (ix *Index) RemoveFile(path string) {
	if _, ok := ix.Files[path]; !ok {
		return
	}
	delete(ix.Files, path)
	// 重建 syms/byName/byCall（文件级删除低频，简单重建）
	syms := make([]Symbol, 0, len(ix.syms))
	byName := map[string][]int{}
	byCall := map[string][]int{}
	for _, s := range ix.syms {
		if s.File == path {
			continue
		}
		idx := len(syms)
		syms = append(syms, s)
		byName[strings.ToLower(s.Name)] = append(byName[strings.ToLower(s.Name)], idx)
		for _, c := range s.Calls {
			k := strings.ToLower(strings.TrimSpace(c))
			if k == "" {
				continue
			}
			byCall[k] = append(byCall[k], idx)
		}
	}
	ix.syms = syms
	ix.byName = byName
	ix.byCall = byCall
}

func (ix *Index) AllSymbols() []Symbol {
	out := make([]Symbol, len(ix.syms))
	copy(out, ix.syms)
	return out
}

func symbolID(file string, line int, name, kind string) string {
	return file + ":" + itoa(line) + ":" + name + ":" + kind
}

// ---- Workspace：一个 workdir 的持久化工作区 ----

// Meta 工作区元信息（meta.json）。
type Meta struct {
	Enabled        bool     `json:"enabled"`
	State          string   `json:"state"` // "" 未初始化 | indexing | ready | error
	ProgressDone   int      `json:"progressDone"`
	ProgressTotal  int      `json:"progressTotal"`
	Err            string   `json:"err,omitempty"`
	LastIndexedAt  int64    `json:"lastIndexedAt"`
	SkipDirs       []string `json:"skipDirs,omitempty"`       // 用户排除规则（gitignore 语法，最高优先级）
	StackGitignore bool     `json:"stackGitignore,omitempty"` // 是否叠加各级 .gitignore / info/exclude / 全局 ignore
	Exts           []string `json:"exts,omitempty"`           // 参与索引的扩展名（空 = 全部受支持语言）
}

// Workspace 引擎内部工作区：Dir=源码根；索引与元信息落在 Store。
type Workspace struct {
	Dir   string // 规范绝对路径（'/' 分隔）
	Store string // Dir/.chonkpilot/codegraph

	mu    sync.Mutex
	recMu sync.Mutex // 差分/重建单飞：同一 workdir 不平行执行 reconcile/initialize
	ix    *Index
	meta  Meta
}

const metaName = "meta.json"

var wsMu sync.Mutex
var wsReg = map[string]*Workspace{} // Dir → Workspace（常驻复用）

// StoreDir 返回工作区落盘目录。
func StoreDir(dir string) string {
	return filepath.ToSlash(filepath.Join(dir, ".chonkpilot", "codegraph"))
}

// Open 获取（进程内复用或从盘恢复）指定 workdir 的工作区。
func Open(dir string) (*Workspace, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("abs %s: %w", dir, err)
	}
	abs = filepath.ToSlash(abs)
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", abs, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", abs)
	}
	wsMu.Lock()
	defer wsMu.Unlock()
	if w, ok := wsReg[abs]; ok {
		return w, nil
	}
	w := &Workspace{Dir: abs, Store: StoreDir(abs)}
	if err := os.MkdirAll(w.Store, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store: %w", err)
	}
	_ = w.loadMeta()
	wsReg[abs] = w
	return w, nil
}

// drop 释放进程内缓存（测试用）。
func dropWorkspace(dir string) {
	wsMu.Lock()
	defer wsMu.Unlock()
	delete(wsReg, dir)
}

func (w *Workspace) lock()   { w.mu.Lock() }
func (w *Workspace) unlock() { w.mu.Unlock() }

func (w *Workspace) Meta() Meta {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.meta
}

// State 快捷：未初始化/索引中/就绪/错误。
func (w *Workspace) State() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.meta.State
}

func (w *Workspace) loadMeta() error {
	b, err := os.ReadFile(filepath.Join(w.Store, metaName))
	if err != nil {
		return err // 首次无 meta 属正常
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("meta.json 解析失败: %w", err)
	}
	w.meta = m
	return nil
}

func (w *Workspace) saveMeta() error {
	w.mu.Lock()
	b, err := json.MarshalIndent(w.meta, "", "  ")
	w.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(w.Store, metaName), b)
}

func (w *Workspace) setMeta(m Meta) {
	w.mu.Lock()
	w.meta = m
	w.mu.Unlock()
}

// LoadIndex 从盘（bbolt index.db）恢复索引；不存在/不可用则返回 false（未就绪）。
// 迁移口径：**不自动迁移**旧 JSON 索引——检测到遗留 index.json（且无 index.db）→ 记日志 +
// 删除旧文件 + 状态回「未初始化」，由调用方（插件编排）执行一次全量重建。
// 库损坏/截断 → 同法兜底（视为未初始化、可重建），不向上抛错（写者持锁的瞬时超时除外）。
func (w *Workspace) LoadIndex() (bool, error) {
	w.mu.Lock()
	loaded := w.ix != nil
	w.mu.Unlock()
	if loaded {
		return true, nil
	}

	if w.legacyJSONExists() && !w.dbExists() {
		log.Printf("[codegraph] %s 检测到旧 JSON 索引（%s）——已弃用（改用 bbolt %s）：删除旧文件并要求全量重建",
			w.Dir, indexLegacyName, dbName)
		_ = os.Remove(filepath.Join(w.Store, indexLegacyName))
		w.resetIndexStateIfReady("索引存储已升级为 bbolt——状态回未初始化，需全量重建")
		return false, nil
	}

	ix, present, err := w.loadIndexFromDB()
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return false, err // 另一写者持锁：瞬时错误，不改状态、不降级
		}
		if !errors.Is(err, bolt.ErrInvalid) && !errors.Is(err, errCorruptDB) {
			return false, err // 非损坏（权限/IO 等）→ 上抛，不静默降级
		}
		log.Printf("[codegraph] %s 索引库损坏（%v）——删除损坏库并视为未初始化，需全量重建", w.Dir, err)
		_ = os.Remove(w.dbPath())
		w.resetIndexStateIfReady("索引库损坏——状态回未初始化，需全量重建")
		return false, nil
	}
	if !present {
		w.resetIndexStateIfReady("索引库缺失——状态回未初始化，需全量重建")
		return false, nil
	}
	w.mu.Lock()
	w.ix = ix
	w.mu.Unlock()
	return true, nil
}

// SaveIndex 全量落盘索引快照（bolt 单事务，原子；Initialize 结束时调用）。
// 增量更新走 saveIndexDelta（Reconcile）。
func (w *Workspace) SaveIndex() error { return w.saveIndexFull() }

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (w *Workspace) ixRef() *Index {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ix
}

func (w *Workspace) indexSnapshot() *Index { return w.ixRef() }

// ---- 路径助手：store 相对，对外绝对 ----

// abs 相对 → workdir 绝对（'/' 分隔）。
func (w *Workspace) abs(p string) string {
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, w.Dir+"/") {
		return p
	}
	if strings.HasPrefix(p, "/") {
		return w.Dir + p
	}
	return w.Dir + "/" + p
}

// norm 把入参（绝对或相对）归一为 store 用的相对形式。
func (w *Workspace) norm(p string) string {
	if p == "" {
		return ""
	}
	s := filepath.ToSlash(p)
	if strings.HasPrefix(s, w.Dir+"/") {
		s = strings.TrimPrefix(s, w.Dir+"/")
	}
	return cleanPath(s)
}

// absSym 返回 File 与 ID 均为绝对路径的符号副本。
func (w *Workspace) absSym(s Symbol) Symbol {
	s.File = w.abs(s.File)
	s.ID = symbolID(s.File, s.Line, s.Name, s.Kind)
	return s
}

// ---- 查询辅助（对已就绪工作区；输入兼容绝对/相对，输出统一绝对）----

// SearchSymbol 按名字/kind/file 过滤。
func (w *Workspace) SearchSymbol(q, kind, file string, limit int) []Symbol {
	ix := w.indexSnapshot()
	if ix == nil {
		return nil
	}
	q = strings.ToLower(q)
	fileRel := w.norm(file)
	var out []Symbol
	for _, s := range ix.AllSymbols() {
		if kind != "" && s.Kind != kind {
			continue
		}
		if file != "" {
			sl := strings.ToLower(s.File)
			if !strings.Contains(sl, strings.ToLower(fileRel)) && !strings.Contains(strings.ToLower(w.abs(s.File)), strings.ToLower(file)) {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(s.Name), q) {
			continue
		}
		out = append(out, w.absSym(s))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}

// FindSymbol 按 id 或 file+name 定位（首个）。id/file 接受绝对或相对。
func (w *Workspace) FindSymbol(id, file, name string) []Symbol {
	ix := w.indexSnapshot()
	if ix == nil {
		return nil
	}
	fileRel := w.norm(file)
	var out []Symbol
	for _, s := range ix.AllSymbols() {
		if id != "" {
			m := s.ID == id || s.ID == w.norm(id)
			if !m {
				continue
			}
		}
		if file != "" && name != "" {
			if s.File != fileRel || !strings.EqualFold(s.Name, name) {
				continue
			}
		}
		out = append(out, w.absSym(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// CalleeRef 一条被调用目标的解析结果（名字级启发式，无类型解析）。
type CalleeRef struct {
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"` // 是否在索引内唯一命中符号
	Kind     string `json:"kind,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	ID       string `json:"id,omitempty"`
}

// Callers 返回调用了 target 的符号（按名字匹配：忽略大小写的全等 或 后缀 `.name`，
// 故 `pkg.Foo` 与 `Foo` 视为同一目标）。file 非空时只保留该文件内的调用方；limit>0 截断。
// 语义为调用点文本的名字级启发式（无类型/重载解析）。
func (w *Workspace) Callers(target, file string, limit int) []Symbol {
	ix := w.indexSnapshot()
	target = strings.TrimSpace(target)
	if ix == nil || target == "" {
		return nil
	}
	idxSet := map[int]bool{}
	for _, k := range callLookupKeys(target) {
		for _, i := range ix.byCall[k] {
			idxSet[i] = true
		}
	}
	idx := make([]int, 0, len(idxSet))
	for i := range idxSet {
		idx = append(idx, i)
	}
	sort.Ints(idx)

	syms := ix.AllSymbols()
	fileRel := w.norm(file)
	var out []Symbol
	for _, i := range idx {
		if i < 0 || i >= len(syms) {
			continue
		}
		s := syms[i]
		if file != "" && !w.fileHit(s.File, fileRel, file) {
			continue
		}
		if !anyCallMatches(s.Calls, target) {
			continue
		}
		out = append(out, w.absSym(s))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Callees 返回某符号（按 id 或 file+name 定位，取首个）直接调用的目标；
// 能唯一解析到已索引符号时回填其 file/line/kind/id（resolved=true，否则 resolved=false）。
func (w *Workspace) Callees(id, file, name string, limit int) []CalleeRef {
	ix := w.indexSnapshot()
	if ix == nil {
		return nil
	}
	hits := w.FindSymbol(id, file, name)
	if len(hits) == 0 {
		return nil
	}
	syms := ix.AllSymbols()
	var out []CalleeRef
	for _, c := range hits[0].Calls {
		ref := CalleeRef{Name: c}
		if ids := ix.byName[strings.ToLower(c)]; len(ids) == 1 && ids[0] < len(syms) {
			s := w.absSym(syms[ids[0]])
			ref.Resolved = true
			ref.Kind = s.Kind
			ref.File = s.File
			ref.Line = s.Line
			ref.ID = s.ID
		}
		out = append(out, ref)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// fileHit 文件过滤：相对路径子串或绝对路径子串命中（与 SearchSymbol 同口径）。
func (w *Workspace) fileHit(sFile, fileRel, fileInput string) bool {
	if strings.Contains(strings.ToLower(sFile), strings.ToLower(fileRel)) {
		return true
	}
	return strings.Contains(strings.ToLower(w.abs(sFile)), strings.ToLower(fileInput))
}

// callLookupKeys 查询目标 → byCall 检索键（本体 + 最后一段，均小写）。
func callLookupKeys(target string) []string {
	t := strings.ToLower(strings.TrimSpace(target))
	if t == "" {
		return nil
	}
	keys := []string{t}
	if tail := callTailName(t); tail != "" && tail != t {
		keys = append(keys, tail)
	}
	return keys
}

// callTailName 取名字最后一段（`.`/`::` 分隔），小写。
func callTailName(name string) string {
	s := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "::", ".")
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// anyCallMatches Calls 中是否存在与 target 匹配的被调名（全等 或 后缀 `.name`）。
func anyCallMatches(calls []string, target string) bool {
	t := strings.ToLower(strings.TrimSpace(target))
	tail := callTailName(t)
	for _, c := range calls {
		cl := strings.ToLower(strings.TrimSpace(c))
		if cl == "" {
			continue
		}
		if cl == t || (tail != "" && cl == tail) {
			return true
		}
		if strings.HasSuffix(cl, "."+t) {
			return true
		}
	}
	return false
}

// TopComplexity 最高复杂度前 topN（>=minCc）。
func (w *Workspace) TopComplexity(file string, topN, minCc int) []Symbol {
	ix := w.indexSnapshot()
	if ix == nil {
		return nil
	}
	fileRel := w.norm(file)
	var out []Symbol
	for _, s := range ix.AllSymbols() {
		if file != "" {
			sl := strings.ToLower(s.File)
			if !strings.Contains(sl, strings.ToLower(fileRel)) && !strings.Contains(strings.ToLower(w.abs(s.File)), strings.ToLower(file)) {
				continue
			}
		}
		if s.Complexity >= minCc {
			out = append(out, w.absSym(s))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Complexity != out[j].Complexity {
			return out[i].Complexity > out[j].Complexity
		}
		return out[i].File < out[j].File
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// ModuleSummary 目录摘要。
func (w *Workspace) ModuleSummary(prefix string) map[string]any {
	ix := w.indexSnapshot()
	lc := map[string]int{}
	ns := 0
	var rels []string
	if ix != nil {
		for p, fi := range ix.Files {
			if prefix != "" {
				pr := w.norm(prefix)
				if !strings.HasPrefix(p, pr) && !strings.HasPrefix(w.abs(p), prefix) {
					continue
				}
			}
			lc[fi.Lang]++
			ns += len(fi.Symbols)
			rels = append(rels, p)
		}
	}
	sort.Strings(rels)
	comps := []Symbol{}
	for _, p := range rels {
		if fi, ok := ix.Files[p]; ok {
			for _, s := range fi.Symbols {
				comps = append(comps, w.absSym(s))
			}
		}
	}
	sort.Slice(comps, func(i, j int) bool { return comps[i].Complexity > comps[j].Complexity })
	if len(comps) > 5 {
		comps = comps[:5]
	}
	return map[string]any{"scope": w.abs(prefix), "files": len(rels), "symbols": ns, "langCounts": lc, "topComplexity5": comps}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
