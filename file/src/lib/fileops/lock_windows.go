//go:build windows

package fileops

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockProbeSupported 平台是否支持进程探活（C-26）：Windows 用 OpenProcess 探活。
const lockProbeSupported = true

// processAlive 探测指定 pid 的进程是否存活（OpenProcess 探活，句柄即关）。
// ACCESS_DENIED = 进程存在但无权打开 → 视为存活（保守不突破，由硬上限兜底）；
// 其余打开失败（典型为进程不存在）→ 视为已死，允许立即突破。
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err == nil {
		windows.CloseHandle(h)
		return true
	}
	return errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
