//go:build !windows

package fileops

// lockProbeSupported 非 Windows 平台不做进程探活（C-26）：探活恒回落纯时间规则，
// 避免误把活锁判为已死（误删）或把死锁判为存活（久等）。
const lockProbeSupported = false

// processAlive 非 Windows 占位实现（lockProbeSupported=false 时不会被调用）。
func processAlive(pid int) bool { return false }
