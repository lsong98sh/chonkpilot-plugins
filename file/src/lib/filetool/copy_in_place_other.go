//go:build !windows

package filetool

import (
	"io"
	"os"
	"path/filepath"
)

// copyFileAtomic 在非 Windows 平台保留“同目录临时文件 + io.Copy + rename”原子写。
//
//   - 不做整文件读入内存，返回实际写入字节数；
//   - rename 仍是原子的，不会留下半截目标；失败时目标保持原状（不会 partial）；
//   - 保留源文件 POSIX mode 与 mtime；
//   - 任一步失败都会删除临时文件，并以 copyNoWriteError 标记“目标未被改动”。
func copyFileAtomic(src, dst string, srcInfo os.FileInfo, overwrite bool) (int64, error) {
	if !overwrite {
		// Lstat：broken symlink 也算“已存在”，overwrite=false 时不得替换链接本身。
		if _, err := os.Lstat(dst); err == nil {
			return 0, markCopyNoWrite(errTargetExists)
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, markCopyNoWrite(err)
	}
	defer in.Close()

	f, err := os.CreateTemp(filepath.Dir(dst), ".ck-copy-*")
	if err != nil {
		return 0, markCopyNoWrite(err)
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	n, err := io.Copy(f, in)
	if err != nil {
		return n, markCopyNoWrite(err)
	}
	if err := f.Chmod(srcInfo.Mode().Perm()); err != nil {
		return n, markCopyNoWrite(err)
	}
	if err := f.Close(); err != nil {
		return n, markCopyNoWrite(err)
	}
	mt := srcInfo.ModTime()
	if err := os.Chtimes(tmp, mt, mt); err != nil {
		return n, markCopyNoWrite(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return n, markCopyNoWrite(err)
	}
	cleanup = false
	return n, nil
}
