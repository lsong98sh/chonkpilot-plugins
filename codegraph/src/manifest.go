// manifest.go —— vfts 索引清单回写（D-71：索引状态归插件自持）。
//
// 清单是**派生缓存**：`<workdir>/.chonkpilot/vfts/file_list.json`（与引擎 meta.json / 文档转换
// 缓存同目录，该目录已强制不入索引 / 不入 git）。唯一权威 = 引擎 `vfts_index` 应答的 `indexed`
// （38-检索与索引 SRCH-007-S05：「保留集以引擎实际 Indexed 为准，不残留旧行」）。
//
// 字段与既有清单一致（key/path/size/mtime/md5/doc_ids/chunks/indexed_at）：key = sha1(绝对路径)
// 前 8 字节 → 16 hex（R-11 绝对路径口径）。`md5` 留空——清单只作索引记录，增量判定由调用方
// 经 `vfts_index {files,remove}` 显式驱动（主仓内嵌 hook 的 md5 diff 已随 D-71 退场）。
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	vfts "github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/vfts/server"
)

// vftsManifestRel 清单相对 workdir 的路径。
const vftsManifestRel = ".chonkpilot/vfts/file_list.json"

// manifestRow 是 file_list 表一行（字段与既有清单同口径，snake_case）。
type manifestRow struct {
	Key       string   `json:"key"`
	Path      string   `json:"path"`
	Size      int64    `json:"size"`
	MTime     string   `json:"mtime"`
	MD5       string   `json:"md5"`
	DocIDs    []string `json:"doc_ids"`
	Chunks    int      `json:"chunks"`
	IndexedAt string   `json:"indexed_at"`
}

var manifestSeq atomic.Uint64

// writeVftsManifest 按引擎 `vfts_index` 应答重建清单（仅全量重建；增量应答只含被处理文件，
// 覆写会丢其余行 → 跳过，清单保持上一次全量的记录）。
//
// 失败只记日志、不影响工具调用结果（清单是派生缓存，缺失即下次全量重建）。
func writeVftsManifest(logf func(format string, args ...any), workdir string, out any) {
	m, ok := out.(map[string]any)
	if !ok || workdir == "" {
		return
	}
	if mode, _ := m["mode"].(string); mode != "full" {
		return
	}
	indexed, ok := m["indexed"].([]vfts.IndexedFile)
	if !ok {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows := make([]manifestRow, 0, len(indexed))
	for _, f := range indexed {
		abs := absPathOf(workdir, f.Path)
		if abs == "" {
			continue
		}
		row := manifestRow{
			Key:       manifestKeyOf(abs),
			Path:      abs,
			DocIDs:    f.DocIDs,
			Chunks:    f.Chunks,
			IndexedAt: now,
		}
		if info, err := os.Stat(filepath.FromSlash(abs)); err == nil {
			row.Size = info.Size()
			row.MTime = info.ModTime().UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	b, err := json.Marshal(rows)
	if err != nil {
		logf("codegraph: vfts 清单序列化失败（workdir=%s）：%v", workdir, err)
		return
	}
	if err := writeManifestFile(manifestPath(workdir), b); err != nil {
		logf("codegraph: vfts 清单写入失败（workdir=%s）：%v", workdir, err)
	}
}

// manifestPath 清单绝对路径。
func manifestPath(workdir string) string {
	return filepath.Join(filepath.FromSlash(workdir), filepath.FromSlash(vftsManifestRel))
}

// manifestKeyOf 文件唯一 key = sha1(绝对路径，'/' 分隔) 前 8 字节 → 16 hex。
func manifestKeyOf(abs string) string {
	sum := sha1.Sum([]byte(abs))
	return hex.EncodeToString(sum[:8])
}

// absPathOf 引擎返回的相对路径（'/' 分隔）→ 绝对路径（'/' 分隔；已是绝对路径则归一）。
func absPathOf(workdir, rel string) string {
	if rel == "" {
		return ""
	}
	s := filepath.ToSlash(rel)
	if strings.HasPrefix(s, "/") || (len(s) > 1 && s[1] == ':') {
		return filepath.ToSlash(filepath.Clean(filepath.FromSlash(s)))
	}
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(workdir), filepath.FromSlash(s)))
}

// writeManifestFile 原子落盘（唯一临时名 + rename 重试：与引擎侧 writeFile 同口径，
// 避免 Windows 下并发读句柄持有目标文件导致 rename 失败）。
func writeManifestFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), manifestSeq.Add(1))
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
