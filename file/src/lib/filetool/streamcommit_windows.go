//go:build windows

package filetool

import (
	"fmt"
	"io"
	"os"
)

// commitStreamOutput 在 Windows 上把临时输出就地提交到已独占打开的目标：
// f.Seek(0) → io.Copy(f, out) → f.Truncate(bytesAfter) → f.Sync()。
// 失败时用 backup 恢复原字节并截断回 bytesBefore；回滚也失败 → partial。
//
// 代价与既有 writeInPlace 一致：放弃 rename 的崩溃原子性，换取写入期间对目标的独占。
func commitStreamOutput(f, out, backup *os.File, outPath string, bytesAfter, bytesBefore int64, mode os.FileMode) (streamCommitOutcome, error) {
	_ = outPath
	_ = mode

	if err := runCommitStreamHook("commit"); err != nil {
		return rollbackStreamOutput(f, backup, bytesBefore, err)
	}
	if err := copyStreamIntoOpenFile(f, out, bytesAfter); err != nil {
		return rollbackStreamOutput(f, backup, bytesBefore, err)
	}
	return streamCommitOutcome{}, nil
}

// copyStreamIntoOpenFile 把 src 从 0 开始复制到已打开句柄 dst，然后截断到 size 并 Sync。
func copyStreamIntoOpenFile(dst, src *os.File, size int64) error {
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.CopyBuffer(dst, src, make([]byte, streamChunkBytes)); err != nil {
		return err
	}
	if err := dst.Truncate(size); err != nil {
		return err
	}
	return dst.Sync()
}

// rollbackStreamOutput 用 backup 恢复目标原字节；失败 → partial。
func rollbackStreamOutput(f, backup *os.File, bytesBefore int64, cause error) (streamCommitOutcome, error) {
	if err := runCommitStreamHook("rollback"); err != nil {
		return streamCommitOutcome{partial: true},
			fmt.Errorf("%w; rollback failed, target may be incomplete: %v", cause, err)
	}
	if err := copyStreamIntoOpenFile(f, backup, bytesBefore); err != nil {
		return streamCommitOutcome{partial: true},
			fmt.Errorf("%w; rollback failed, target may be incomplete: %v", cause, err)
	}
	return streamCommitOutcome{rolledBack: true}, cause
}
