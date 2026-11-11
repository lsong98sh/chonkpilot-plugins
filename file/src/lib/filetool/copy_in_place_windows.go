//go:build windows

package filetool

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// copyFileAtomic 在 Windows 上以“目标文件独占打开 + 就地复制”完成单个文件复制。
//
// 顺序：os.Open(src) → openExclusiveWriteOnce(dst, overwrite) → io.Copy →
// Truncate(n) → Sync → Chmod → SetFileTime（句柄设置 mtime）→ Close。
//
//   - 不使用临时文件 + rename：放弃崩溃原子性，换取复制期间对目标的独占
//     （dwShareMode=0，其他应用无法同时打开目标）；
//   - 目标原本不存在且复制失败 → 删除本次新建的目标（不留空/半截新文件）；
//   - 目标原本存在且复制已开始后失败 → 返回 partialCopyError，由调用方标记 partial；
//   - openExclusiveWriteOnce 的 errFileInUse / errTargetExists / errIsDir 原样
//     返回并标记为“未写入目标”。
func copyFileAtomic(src, dst string, srcInfo os.FileInfo, overwrite bool) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		// 源打不开时不创建目标（避免留下空目标）。
		return 0, markCopyNoWrite(err)
	}
	defer in.Close()

	f, existed, err := openExclusiveWriteOnce(dst, overwrite)
	if err != nil {
		return 0, markCopyNoWrite(err)
	}

	n, werr := copyStreamToOpenFile(f, in)
	if werr == nil {
		if err := f.Chmod(srcInfo.Mode().Perm()); err != nil {
			werr = err
		}
	}
	if werr == nil {
		ft := windows.NsecToFiletime(srcInfo.ModTime().UnixNano())
		if err := windows.SetFileTime(windows.Handle(f.Fd()), nil, nil, &ft); err != nil {
			werr = err
		}
	}
	if cerr := f.Close(); cerr != nil && werr == nil {
		werr = cerr
	}
	if werr != nil {
		if existed {
			// 用户已有目标可能已被部分覆盖：绝不删除，交给调用方标 partial。
			return n, markCopyPartial(werr)
		}
		// 本次新建的目标：删除空/半截文件，恢复到调用前（不存在）。
		if rerr := os.Remove(dst); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return n, fmt.Errorf("%w; failed to remove new target: %v", markCopyNoWrite(werr), rerr)
		}
		return n, markCopyNoWrite(werr)
	}
	return n, nil
}

// copyStreamToOpenFile 把源流写入已独占打开的目标，并截断到实际写入长度后 Sync。
// 返回实际写入字节数（含失败时的部分写入）。
func copyStreamToOpenFile(f *os.File, in io.Reader) (int64, error) {
	n, err := io.Copy(f, in)
	if err != nil {
		return n, err
	}
	if err := f.Truncate(n); err != nil {
		return n, err
	}
	if err := f.Sync(); err != nil {
		return n, err
	}
	return n, nil
}
