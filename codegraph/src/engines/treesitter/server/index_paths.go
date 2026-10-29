package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// IndexPaths 按路径增量更新索引（入参为绝对路径；相对 workdir 内部归一）。
//
// 语义：
//   - removes 命中的条目直接从索引删除；
//   - files 逐个判定：扩展名不受支持 / 语言不支持 / 超过大小上限 / 命中排除规则 /
//     读取或解析失败 → 从索引删除并计入 skipped；可索引 → 以新条目替换旧条目；
//   - 符号索引（syms/byName/byCall）**整体重建**（AddFile 只追加，直接追加会重复）。
//
// 未初始化（索引库缺失/无 schema）返回 ErrNotInitialized。
func (w *Workspace) IndexPaths(files, removes []string) (indexed, removed, skipped int, err error) {
	w.recMu.Lock()
	defer w.recMu.Unlock()

	ok, err := w.LoadIndex()
	if err != nil {
		return 0, 0, 0, err
	}
	if !ok || w.ixRef() == nil {
		return 0, 0, 0, ErrNotInitialized
	}

	w.mu.Lock()
	base := w.ix
	w.mu.Unlock()

	next := cloneIndex(base)
	exts := w.extSet()
	opts := w.excludeOptions()
	opts.Root = w.Dir // Ignored 需要规则根（WalkDir 的根取入参，不依赖该字段）

	for _, p := range removes {
		rel, ok := w.relInWorkdir(p)
		if !ok {
			continue
		}
		if _, exists := next.Files[rel]; !exists {
			continue
		}
		next.RemoveFile(rel)
		removed++
	}

	seen := make(map[string]bool, len(files))
	for _, p := range files {
		rel, ok := w.relInWorkdir(p)
		if !ok || seen[rel] {
			continue
		}
		seen[rel] = true

		drop := func() {
			if _, exists := next.Files[rel]; exists {
				next.RemoveFile(rel)
			}
			skipped++
		}

		ext := strings.ToLower(filepath.Ext(rel))
		if !exts[ext] || opts.Ignored(rel) {
			drop()
			continue
		}
		lang, ok := LangForExt(ext)
		if !ok {
			drop()
			continue
		}
		info, statErr := os.Stat(filepath.FromSlash(joinPath(w.Dir, rel)))
		if statErr != nil || info.IsDir() || info.Size() > int64(maxFileBytes) {
			drop()
			continue
		}
		fi, perr := w.parseEntry(srcEntry{path: rel, lang: lang,
			mtime: info.ModTime().UnixNano(), size: info.Size()})
		if perr != nil || fi == nil {
			drop()
			continue
		}
		if _, exists := next.Files[rel]; exists {
			next.RemoveFile(rel)
		}
		next.AddFile(fi)
		indexed++
	}

	ix := rebuildIndex(next.Files)
	total := len(ix.Files)
	w.mu.Lock()
	w.ix = ix
	w.meta.State = "ready"
	w.meta.Err = ""
	w.meta.ProgressDone = total
	w.meta.ProgressTotal = total
	w.meta.LastIndexedAt = time.Now().UnixNano()
	w.mu.Unlock()
	if err := w.SaveIndex(); err != nil {
		w.markError(fmt.Sprintf("save index: %v", err))
		return indexed, removed, skipped, err
	}
	return indexed, removed, skipped, w.saveMeta()
}

// rebuildIndex 按路径升序重建索引（AddFile 只追加，不能对既有 Index 重复调用）。
func rebuildIndex(files map[string]*FileInfo) *Index {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	ix := newIndex()
	for _, p := range paths {
		ix.AddFile(files[p])
	}
	return ix
}

// relInWorkdir 把入参（绝对或相对）归一为相对 workdir 的 '/' 路径；
// 越界（workdir 之外）或空路径返回 ok=false。
func (w *Workspace) relInWorkdir(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	if !filepath.IsAbs(filepath.FromSlash(p)) {
		rel := cleanPath(filepath.ToSlash(p))
		if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return rel, true
	}
	rel, err := filepath.Rel(filepath.FromSlash(w.Dir), filepath.FromSlash(p))
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return cleanPath(rel), true
}
