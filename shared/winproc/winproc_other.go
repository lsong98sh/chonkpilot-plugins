//go:build !windows

// Package winproc 提供 spawn 外部进程时的进程创建属性。
// 目的：console 子系统 exe（executor / 引擎 exe 等）被 GUI 或服务进程 spawn 时，
// 抑制瞬时/可见的控制台窗口（黑窗），且不影响 stdin/stdout/stderr 管道通信。
package winproc

import (
	"os"
	"syscall"
)

// SysProcAttr 非 Windows 平台无「控制台窗口」概念，返回 nil（不设置进程属性），
// 保证交叉编译与测试不受影响。
func SysProcAttr() *syscall.SysProcAttr {
	return nil
}

// EnsureKillOnCloseJob 非 Windows 平台为 no-op（Job Object 为 Windows 专有机制；C-40）。
func EnsureKillOnCloseJob() {}

// KillTree 非 Windows 平台仅终止指定进程（无 taskkill /T 等价树杀；调用方主要用于 Windows）。
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
