//go:build !windows

package filetool

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeInPlace 在非 Windows 平台保留“同目录临时文件 + rename”原子写：
// Unix 的就地写会破坏 rename 原子性，且没有真正的独占打开语义。
//
// f 只用于提供目标路径（f.Name()）与保持 flock 生命周期；内容经临时文件
// 完整写入并 Sync 后 rename 覆盖目标。成功返回 len(data)，失败返回 0
// （rename 前失败不会留下半截目标文件）。
func writeInPlace(f *os.File, data []byte) (int, error) {
	path := f.Name()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ck-tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, err
	}
	cleanup = false
	return len(data), nil
}
