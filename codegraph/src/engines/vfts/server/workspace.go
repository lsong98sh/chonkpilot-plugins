// Package server 实现 chonkpilot-vfts 引擎 lib：
// 基于 zvec（C-API）FTS 全文索引的 workdir 工作区，索引落 <workdir>/.chonkpilot/vfts/，
// 提供 configure / index / query / status 四个工具面。
//
// 阶段 1 只做 FTS（零 embedding 依赖）：索引内容 = 纯文本（源码 + .txt + .md）。
// 向量语义检索（HNSW）后置为阶段 2。
package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	zvec "github.com/zvec-ai/zvec-go"
)

// Meta 工作区元信息（meta.json），跨进程可读（status 与就绪门控用）。
type Meta struct {
	Enabled        bool     `json:"enabled"`
	State          string   `json:"state"` // "" 未初始化 | indexing | ready | error
	ProgressDone   int      `json:"progressDone"`
	ProgressTotal  int      `json:"progressTotal"`
	Err            string   `json:"err,omitempty"`
	LastIndexedAt  int64    `json:"lastIndexedAt"`
	SkipDirs       []string `json:"skipDirs,omitempty"`       // 用户排除规则（gitignore 语法，最高优先级）
	StackGitignore bool     `json:"stackGitignore,omitempty"` // 是否叠加各级 .gitignore / info/exclude / 全局 ignore
	Exts           []string `json:"exts,omitempty"`
	// Encoding 遗留文本编码（D-73）："" / "auto" = 自动识别；其余 = 按指定编码解码
	// （utf-8 / gb18030 / gbk / shift_jis / big5 / euc-kr）。
	Encoding   string `json:"encoding,omitempty"`
	FileCount  int    `json:"fileCount"`
	ChunkCount int    `json:"chunkCount"`
	Tokenizer  string `json:"tokenizer"`
	// Docs 文档转换接入（Office/PDF → 文本）的配置与最近一次索引的运行态。
	// 注意：**不含 token**（token 仅内存持有，见 Workspace.docsToken，避免密文落盘）。
	Docs DocsMeta `json:"docs,omitempty"`
	// NextPK 是增量索引已分配的最大文档主键（纯数字字符串，跨调用单调递增，
	// 避免与全量重建的 1..N 主键冲突）。全量重建会重置集合，故同步重置为 N。
	NextPK int64 `json:"nextPK,omitempty"`
}

// Workspace 引擎内部工作区：Dir=项目根；索引与元信息落在 Store（文件索引）+ SessionStore（会话索引）。
type Workspace struct {
	Dir   string // 规范绝对路径（'/' 分隔）
	Store string // Dir/.chonkpilot/vfts
	// SessionStore 会话内容索引落盘目录（Dir/.chonkpilot/vfts-session；与文件索引完全独立，
	// 文件索引全量重建不会清空它会话索引）。
	SessionStore string

	mu    sync.Mutex // 保护 meta
	recMu sync.Mutex // 就绪/重建单飞：同一 workdir 不平行执行
	zmu   sync.Mutex // zvec 集合操作串行 + 保护集合的生命周期
	// coll 是集合句柄的**单原子指针**（由 zmu 临界区内的 setColl 唯一写入）：「是否已载入」
	// 由 coll.Load() != nil 派生（见 isLoaded）。Status / EnsureReady 据此读取载入态，无需等待
	// Initialize 的整段长 zmu 临界区；单原子写入消除「先改 coll 再改标志」两段式带来的瞬时误报。
	coll atomic.Pointer[zvec.Collection]
	meta Meta

	// 会话索引（SESSION-FTS）：与文件索引独立的一把锁 + 一套集合/状态，互不阻塞。
	smu      sync.Mutex
	sessColl atomic.Pointer[zvec.Collection]
	sess     sessionStateFile
	// docsToken 转换服务鉴权 token（仅内存，绝不落盘/落日志；每次 vfts_configure 下发）。
	docsToken string
}

const (
	metaName = "meta.json"
	collDir  = "collection"
	// tokenizer FTS 分词器固定为 jieba（中文按词切分，提升中文召回；基础词典内嵌于
	// third_party/jieba 并物化到系统级目录，见 dict.go）。字典/分词器变更后旧索引失效。
	tokenizer = "jieba"
)

var wsMu sync.Mutex
var wsReg = map[string]*Workspace{} // wsKey(绝对路径) → Workspace（进程内常驻复用）

// wsKey 返回进程内 Workspace 的注册键（CODEGRAPH-IDXDB-1）：
// Windows 路径大小写不敏感 → 统一小写归一，避免同一 workdir 因大小写差异被建成**两个**
// Workspace（两套内存索引共写同一个落盘目录 → 上报 status 与落盘集合不一致）。
func wsKey(abs string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}

// StoreDir 返回工作区落盘目录。
func StoreDir(dir string) string {
	return filepath.ToSlash(filepath.Join(dir, ".chonkpilot", "vfts"))
}

// collPath zvec 集合目录（OS 原生分隔符，供 C-API 使用）。
func (w *Workspace) collPath() string {
	return filepath.Join(filepath.FromSlash(w.Store), collDir)
}

// setColl 记录集合句柄（**须持 zmu**；coll 赋值的唯一入口）。atomic.Pointer 写入即原子发布，
// 「是否已载入」由句柄非空派生，无「先改句柄再改标志」的两段式窗口。
func (w *Workspace) setColl(c *zvec.Collection) {
	w.coll.Store(c)
}

// isLoaded coll 是否已载入（原子快照，不取 zmu）。
func (w *Workspace) isLoaded() bool { return w.coll.Load() != nil }

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
	if w, ok := wsReg[wsKey(abs)]; ok {
		return w, nil
	}
	w := &Workspace{Dir: abs, Store: StoreDir(abs), SessionStore: SessionStoreDir(abs)}
	if err := os.MkdirAll(filepath.FromSlash(w.Store), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store: %w", err)
	}
	_ = w.loadMeta()
	wsReg[wsKey(abs)] = w
	return w, nil
}

// Drop 释放进程内缓存（测试用）。
func Drop(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	abs = filepath.ToSlash(abs)
	wsMu.Lock()
	defer wsMu.Unlock()
	if w, ok := wsReg[wsKey(abs)]; ok {
		w.zmu.Lock()
		if c := w.coll.Load(); c != nil {
			_ = c.Close()
			w.setColl(nil)
		}
		w.zmu.Unlock()
		w.smu.Lock()
		if c := w.sessColl.Load(); c != nil {
			_ = c.Close()
			w.sessColl.Store(nil)
		}
		w.smu.Unlock()
		delete(wsReg, wsKey(abs))
	}
}

func (w *Workspace) Meta() Meta {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.meta
}

func (w *Workspace) loadMeta() error {
	b, err := os.ReadFile(filepath.Join(filepath.FromSlash(w.Store), metaName))
	if err != nil {
		return err // 首次无 meta 属正常
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("meta.json 解析失败: %w", err)
	}
	// 分词器变更（如 standard → jieba）→ 落盘集合由**旧分词器**建成、不可复用：
	// 复位 state 强制走全量重建（Initialize 会重建集合目录并写回新 tokenizer）。
	if m.Tokenizer != "" && m.Tokenizer != tokenizer {
		m.State = ""
		m.Err = ""
	}
	w.mu.Lock()
	w.meta = m
	w.mu.Unlock()
	return nil
}

// ReloadMeta 从盘重读 meta（跨进程/跨调用刷新状态视图）。
func (w *Workspace) ReloadMeta() error {
	return w.loadMeta()
}

func (w *Workspace) saveMeta() error {
	w.mu.Lock()
	b, err := json.MarshalIndent(w.meta, "", "  ")
	w.mu.Unlock()
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(filepath.FromSlash(w.Store), metaName), b)
}

// vftsFileSeq 并发写同一路径时的临时文件唯一序号（配合 pid 保证不撞名）。
var vftsFileSeq atomic.Uint64

// writeFileAtomic 临时文件 + rename 原子落盘。
//
// 临时名必须唯一：固定 `path+".tmp"` 时并发写者（配置下发 vs 状态/清单回写）会互抢同一
// 临时文件 → Windows 上 rename 报「找不到文件」/「被占用」，甚至把旧内容落位。故带 pid +
// 自增序号，并对 rename 做短暂重试。
func writeFileAtomic(path string, b []byte) error {
	tmp := fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), vftsFileSeq.Add(1))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	var err error
	for i := 0; i < 6; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(20*(i+1)) * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return err
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
	ChunkCount     int      `json:"chunkCount"`
	Tokenizer      string   `json:"tokenizer"`
	Loaded         bool     `json:"loaded"` // 当前进程内存已载入集合
	// 文档转换接入（Office/PDF）运行态：供 plugin 透出到 vfts.status / UI。
	DocsEnabled       bool   `json:"docsEnabled"`
	DocsService       string `json:"docsService,omitempty"`       // running | absent
	DocsSkipped       int    `json:"docsSkipped,omitempty"`       // 最近一次索引因服务不可用整批跳过的文档数
	DocsFailed        int    `json:"docsFailed,omitempty"`        // 最近一次索引转换失败降级为「仅文件名」的文档数
	DocsParserVersion string `json:"docsParserVersion,omitempty"` // 最近一次成功转换上报的解析器版本
}

func (w *Workspace) Status() Status {
	w.mu.Lock()
	m := w.meta
	w.mu.Unlock()
	loaded := w.isLoaded() // 原子快照位：不取 zmu，避免与 Initialize 长临界区耦合而阻塞状态查询
	exts := m.Exts
	if len(exts) == 0 {
		exts = defaultExts()
	}
	return Status{Workdir: w.Dir, State: m.State, Enabled: m.Enabled,
		ProgressDone: m.ProgressDone, ProgressTotal: m.ProgressTotal, Err: m.Err,
		LastIndexedAt: m.LastIndexedAt, SkipDirs: append([]string{}, m.SkipDirs...),
		StackGitignore: m.StackGitignore,
		Exts:           append([]string{}, exts...), IndexedFiles: m.FileCount,
		ChunkCount: m.ChunkCount, Tokenizer: tokenizer, Loaded: loaded,
		DocsEnabled: m.Docs.Enabled, DocsService: docsServiceOf(m.Docs),
		DocsSkipped: m.Docs.Skipped, DocsFailed: m.Docs.Failed,
		DocsParserVersion: m.Docs.ParserVersion}
}

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
	if strings.HasPrefix(strings.ToLower(s), strings.ToLower(w.Dir)+"/") {
		s = s[len(w.Dir)+1:]
	}
	return cleanPath(s)
}

// CloseAll 关闭全部进程内集合并释放 zvec 资源（进程退出前调用）。
func CloseAll() {
	wsMu.Lock()
	for _, w := range wsReg {
		w.zmu.Lock()
		if c := w.coll.Load(); c != nil {
			_ = c.Close()
			w.setColl(nil)
		}
		w.zmu.Unlock()
		w.smu.Lock()
		if c := w.sessColl.Load(); c != nil {
			_ = c.Close()
			w.sessColl.Store(nil)
		}
		w.smu.Unlock()
	}
	wsMu.Unlock()
	shutdownZvec()
}
