package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// 索引落盘 = **bbolt 单文件库**（纯 Go、无 CGO）：<workDir>/.chonkpilot/codegraph/index.db。
// 工作区元信息 meta.json 不属索引本体（插件跨进程轮询索引进度用）→ 维持独立 JSON 文件，见 graph.go。
//
// bucket 设计（键一律为 store 相对 '/' 路径）：
//   - meta    ：`schema` = 索引库格式版本（bucket 结构/值编码变更时的兼容判定）
//   - files   ：文件条目元数据（path/lang/mtime/size/imports/hasErr）→ 依赖图 + 增量基线快检
//   - symbols ：该文件的符号数组（[]Symbol，保序；含 Calls 调用图）
//
// 不另设 refs/deps bucket：全部查询都经内存 Index（syms/byName/byCall）派生，重构 FileInfo 需
// 跨 bucket 拼装徒增风险且无功能收益；**单文件条目即最小同步单元** → 增量更新 = 单文件 put/delete，
// 由 bolt 事务保证原子性（替代旧「整体重写 index.json」）。
const (
	dbName = "index.db"
	// indexLegacyName 旧版 JSON 索引文件名（已废弃）：检测到即删除并要求一次全量重建。
	indexLegacyName = "index.json"

	bucketMeta    = "meta"
	bucketFiles   = "files"
	bucketSymbols = "symbols"

	keySchema = "schema"
	// schemaV1 当前索引库格式版本（bucket 结构/值编码变更时递增）。
	schemaV1 = "codegraph-index/1"

	// dbOpenTimeout bolt 打开/等待写锁的上限（异常持锁时快速失败，不无限阻塞）：
	// 1.5s 与查询侧「有界等待 2-3s → 正在索引，不可用」口径对齐（2026-10-19 用户定稿）。
	dbOpenTimeout = 1500 * time.Millisecond
)

// fileMeta 文件条目元数据（files bucket 的值，键 = 相对路径；符号单独存 symbols bucket）。
type fileMeta struct {
	Lang    string   `json:"lang"`
	Mtime   int64    `json:"mtime"`
	Size    int64    `json:"size"`
	Imports []string `json:"imports"`
	HasErr  bool     `json:"hasErr"`
}

// errNoIndexSchema 索引库存在但无 schema 标记（空库/半成品）。
var errNoIndexSchema = errors.New("index.db 缺少 schema 标记")

// errCorruptDB 索引库损坏/不可解析（含 bolt 读页 panic）。索引为派生数据 → 可直接丢弃重建。
var errCorruptDB = errors.New("索引库损坏")

// dbPath 索引库路径。
func (w *Workspace) dbPath() string { return filepath.Join(w.Store, dbName) }

// dbExists 索引库文件是否存在。
func (w *Workspace) dbExists() bool {
	fi, err := os.Stat(w.dbPath())
	return err == nil && !fi.IsDir()
}

// dbUsable 索引库文件是否存在且有内容（空文件 = 未初始化；bolt 只读打开空文件会尝试初始化 → 报错）。
func (w *Workspace) dbUsable() bool {
	fi, err := os.Stat(w.dbPath())
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

// legacyJSONExists 旧 JSON 索引是否存在。
func (w *Workspace) legacyJSONExists() bool {
	fi, err := os.Stat(filepath.Join(w.Store, indexLegacyName))
	return err == nil && !fi.IsDir()
}

// openDB 打开索引库。readOnly=true → 共享锁（多读单写安全）；false → 独占锁 + 自动建目录。
// 每次操作即开即关（事务结束即释放锁），避免 .db 文件锁残留影响重开与跨进程只读访问。
func (w *Workspace) openDB(readOnly bool) (*bolt.DB, error) {
	if !readOnly {
		if err := os.MkdirAll(w.Store, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir store: %w", err)
		}
	}
	db, err := bolt.Open(w.dbPath(), 0o644, &bolt.Options{Timeout: dbOpenTimeout, ReadOnly: readOnly})
	if err != nil {
		return nil, err
	}
	return db, nil
}

// openDBForWrite 打开可写库。库文件为**损坏的非 bolt 文件**（bolt.ErrInvalid）→ 删除后重建
// （索引是派生数据，可整体重建）；其他错误（锁超时/权限/IO）原样上抛，不误删。
func (w *Workspace) openDBForWrite() (*bolt.DB, error) {
	db, err := w.openDB(false)
	if err == nil {
		return db, nil
	}
	if !errors.Is(err, bolt.ErrInvalid) {
		return nil, err
	}
	if rmErr := os.Remove(w.dbPath()); rmErr != nil && !os.IsNotExist(rmErr) {
		return nil, err
	}
	log.Printf("[codegraph] %s 索引库损坏（%v）——已删除并重建", w.Dir, err)
	return w.openDB(false)
}

// loadIndexFromDB 从索引库载入内存索引。
// present=false → 库不存在/为空/无可用 schema（视同未初始化，非错误）。
// 损坏/截断库在 bolt 读页时可能 panic → 统一 recover 转 errCorruptDB，由 LoadIndex 兜底。
func (w *Workspace) loadIndexFromDB() (ix *Index, present bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			ix, present, err = nil, true, fmt.Errorf("%w: %v", errCorruptDB, r)
		}
	}()
	if !w.dbUsable() {
		return nil, false, nil
	}
	db, err := w.openDB(true)
	if err != nil {
		return nil, true, fmt.Errorf("index.db 打开失败: %w", err)
	}
	defer func() { _ = db.Close() }()

	out := newIndex()
	err = db.View(func(tx *bolt.Tx) error {
		mb := tx.Bucket([]byte(bucketMeta))
		if mb == nil || string(mb.Get([]byte(keySchema))) != schemaV1 {
			return errNoIndexSchema
		}
		fb := tx.Bucket([]byte(bucketFiles))
		if fb == nil {
			return nil // 有 schema 无数据 = 空索引（可合法存在）
		}
		sb := tx.Bucket([]byte(bucketSymbols))
		var paths []string
		if err := fb.ForEach(func(k, _ []byte) error {
			paths = append(paths, string(k))
			return nil
		}); err != nil {
			return err
		}
		sort.Strings(paths) // 稳定载入序（= 旧 JSON 按 Path 排序的数组序），保 Calls 截断顺序不变
		for _, p := range paths {
			var fm fileMeta
			if err := json.Unmarshal(fb.Get([]byte(p)), &fm); err != nil {
				return fmt.Errorf("index.db files[%s] 解析失败: %w", p, err)
			}
			fi := &FileInfo{Path: p, Lang: fm.Lang, Mtime: fm.Mtime,
				Size: fm.Size, Imports: fm.Imports, HasErr: fm.HasErr}
			if sb != nil {
				if raw := sb.Get([]byte(p)); len(raw) > 0 {
					if err := json.Unmarshal(raw, &fi.Symbols); err != nil {
						return fmt.Errorf("index.db symbols[%s] 解析失败: %w", p, err)
					}
				}
			}
			out.AddFile(fi)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errNoIndexSchema) {
			return nil, false, nil
		}
		return nil, true, err
	}
	return out, true, nil
}

// saveIndexFull 全量落盘（单事务，原子）：重建三个 bucket 后写入全部文件条目。
// 仅 Initialize（全量重建）路径使用；Reconcile 走 saveIndexDelta（只写变更部分）。
func (w *Workspace) saveIndexFull() error {
	w.mu.Lock()
	ix := w.ix
	var paths []string
	if ix != nil {
		paths = make([]string, 0, len(ix.Files))
		for p := range ix.Files {
			paths = append(paths, p)
		}
		sort.Strings(paths)
	}
	w.mu.Unlock()

	db, err := w.openDBForWrite()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{bucketFiles, bucketSymbols, bucketMeta} {
			if tx.Bucket([]byte(name)) != nil {
				if err := tx.DeleteBucket([]byte(name)); err != nil {
					return err
				}
			}
		}
		mb, err := tx.CreateBucket([]byte(bucketMeta))
		if err != nil {
			return err
		}
		if err := mb.Put([]byte(keySchema), []byte(schemaV1)); err != nil {
			return err
		}
		fb, err := tx.CreateBucket([]byte(bucketFiles))
		if err != nil {
			return err
		}
		sb, err := tx.CreateBucket([]byte(bucketSymbols))
		if err != nil {
			return err
		}
		for _, p := range paths {
			if err := putFileEntry(fb, sb, ix.Files[p]); err != nil {
				return err
			}
		}
		return nil
	})
}

// saveIndexDelta 增量落盘（单事务，原子）：只写变更文件 + 删除已移除文件。
func (w *Workspace) saveIndexDelta(changed []*FileInfo, removed []string) error {
	db, err := w.openDBForWrite()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	return db.Update(func(tx *bolt.Tx) error {
		mb, err := tx.CreateBucketIfNotExists([]byte(bucketMeta))
		if err != nil {
			return err
		}
		if err := mb.Put([]byte(keySchema), []byte(schemaV1)); err != nil {
			return err
		}
		fb, err := tx.CreateBucketIfNotExists([]byte(bucketFiles))
		if err != nil {
			return err
		}
		sb, err := tx.CreateBucketIfNotExists([]byte(bucketSymbols))
		if err != nil {
			return err
		}
		for _, p := range removed {
			if err := fb.Delete([]byte(p)); err != nil {
				return err
			}
			if err := sb.Delete([]byte(p)); err != nil {
				return err
			}
		}
		for _, fi := range changed {
			if err := putFileEntry(fb, sb, fi); err != nil {
				return err
			}
		}
		return nil
	})
}

// putFileEntry 写入单文件条目的元数据与符号（键 = fi.Path，同一事务内）。
func putFileEntry(fb, sb *bolt.Bucket, fi *FileInfo) error {
	meta, err := json.Marshal(fileMeta{Lang: fi.Lang, Mtime: fi.Mtime,
		Size: fi.Size, Imports: fi.Imports, HasErr: fi.HasErr})
	if err != nil {
		return err
	}
	if err := fb.Put([]byte(fi.Path), meta); err != nil {
		return err
	}
	syms, err := json.Marshal(fi.Symbols)
	if err != nil {
		return err
	}
	return sb.Put([]byte(fi.Path), syms)
}

// removeIndexArtifacts 删除索引落盘产物（index.db 与遗留 index.json）；不存在不报错。
// 供 Clear（清除索引产物）使用。
func (w *Workspace) removeIndexArtifacts() error {
	if err := os.Remove(w.dbPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除索引产物失败: %w", err)
	}
	if err := os.Remove(filepath.Join(w.Store, indexLegacyName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除遗留索引产物失败: %w", err)
	}
	return nil
}

// resetIndexStateIfReady 当状态为 ready 而索引已不可用（旧 JSON / 库缺失 / 库损坏）时回「未初始化」，
// 使插件侧就绪缓存不再命中 → 触发一次全量重建（配置存档保留）。非 ready 状态不动。
func (w *Workspace) resetIndexStateIfReady(reason string) {
	w.mu.Lock()
	if w.meta.State != "ready" {
		w.mu.Unlock()
		return
	}
	w.meta.State = ""
	w.meta.Err = ""
	w.meta.ProgressDone = 0
	w.meta.ProgressTotal = 0
	w.meta.LastIndexedAt = 0
	w.mu.Unlock()
	log.Printf("[codegraph] %s %s", w.Dir, reason)
	_ = w.saveMeta()
}

// IndexedFiles 已载入内存索引的文件相对路径（升序）；未载入返回 nil。
// 供 `-dump` 只读自检输出命中集合（不被其他查询路径使用）。
func (w *Workspace) IndexedFiles() []string {
	w.mu.Lock()
	ix := w.ix
	w.mu.Unlock()
	if ix == nil {
		return nil
	}
	out := make([]string, 0, len(ix.Files))
	for p := range ix.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
