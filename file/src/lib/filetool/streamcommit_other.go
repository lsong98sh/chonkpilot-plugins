//go:build !windows

package filetool

import (
	"os"
)

// commitStreamOutput 在非 Windows 平台把临时输出 rename 覆盖目标：
// out.Sync → out.Close → Chmod(临时文件, 目标 mode) → Rename(临时文件, 目标)。
// rename 是原子的；任一步失败目标保持原状（partial=false，无需回滚）。
func commitStreamOutput(f, out, backup *os.File, outPath string, bytesAfter, bytesBefore int64, mode os.FileMode) (streamCommitOutcome, error) {
	_ = backup
	_ = bytesAfter
	_ = bytesBefore

	if err := runCommitStreamHook("commit"); err != nil {
		return streamCommitOutcome{}, err
	}
	if err := out.Sync(); err != nil {
		return streamCommitOutcome{}, err
	}
	if err := out.Close(); err != nil {
		return streamCommitOutcome{}, err
	}
	if err := os.Chmod(outPath, mode); err != nil {
		return streamCommitOutcome{}, err
	}
	if err := os.Rename(outPath, f.Name()); err != nil {
		return streamCommitOutcome{}, err
	}
	return streamCommitOutcome{}, nil
}
