// open_exclusive.go — 写工具的独占打开辅助（平台无关部分）。
//
// 2026-11-09 批处理调度改造：独占打开拆为「单次尝试」版本
// （openExclusiveExistingOnce / openExclusiveWriteOnce），内部不再 sleep/重试；
// 共享/锁冲突统一映射为 errFileInUse，由 batchlock.go 的调度器在其他文件处理完后
// 按 50/100/200ms 重试 3 轮。delete 的持有式句柄不受影响（见 exclusive_*.go）。
package filetool

import (
	"errors"
	"os"
)

// 独占打开相关的稳定错误（write/replace/patch 直接映射到 failed 文案）。
var (
	errTargetNotExist = errors.New("target file does not exist")
	errTargetExists   = errors.New("target exists (overwrite=false)")
	errIsDir          = errors.New("is a directory")
)

// isDirPath 判断 path 当前是否为目录（不存在/不可访问 → false）。
// 独占打开对目录会返回 ACCESS_DENIED/EISDIR，先判目录以保留 "is a directory" 文案。
func isDirPath(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
