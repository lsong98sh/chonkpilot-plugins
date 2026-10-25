// Package lockfile 提供**跨进程文件锁**原语（复用 `LockFileEx` 模式抽象到共用处，
// 见 llm/server/sessionlock_windows.go 的 per-session 锁），当前用于「同 work-dir 只允许
// 单实例」的启动占用校验（I-74；CLI × GUI 共库冲突（D-45）复用同一原语）。
//
// 语义：非阻塞获取；已被其它进程持有 → `ErrBusy`；进程崩溃由 OS 自动释放（Windows
// LockFileEx 句柄随进程回收）——**不依赖 sticky 锁文件**（文件本身可残留，锁不残留）。
package lockfile

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// ErrBusy 表示目标锁已被其它进程持有（调用方据此给出「已被打开」的明确提示）。
var ErrBusy = errors.New("lockfile: busy")

// ErrUnsupported 表示本平台不提供文件锁（非 Windows stub 返回；调用方据此降级放行）。
// 定义于跨平台文件，供上层（如 procsingleton）在任意平台引用判定。
var ErrUnsupported = errors.New("lockfile: not supported on this platform")

// PrjUsrLockName 是 **prjusr 单例锁**文件名：flash 与桌面单体**共用同名同目录**的锁
// （`<prjusr>/chonkpilot.lock`）→ 同一项目在本机互斥（防两实例同时打开同一项目）。
const PrjUsrLockName = "chonkpilot.lock"

// ProjectDataDirName 是**项目数据根目录名**（`<work_dir>/.chonkpilot`）：flash / 桌面单体 /
// CLI 的 prjusr 单例锁均按其拼接项目数据根（**单一字面量来源**，避免各模块各自维护独立常量、
// 失配后「选目录预检」永不命中，见 I-74 / 62 §3A）。
const ProjectDataDirName = ".chonkpilot"

// PrjUsrLockPath 返回 prjusr 数据根下的单例锁文件路径（`<prjUsrRoot>/chonkpilot.lock`）。
func PrjUsrLockPath(prjUsrRoot string) string {
	return filepath.Join(prjUsrRoot, PrjUsrLockName)
}

// AcquirePrjUsr 非阻塞获取 prjusr 单例锁（flash 与桌面单体共用）；已被占用 → ErrBusy。
// 返回的 Lock 须在进程存活期持有（`defer lock.Release()`）。
func AcquirePrjUsr(prjUsrRoot string) (*Lock, error) {
	return Acquire(PrjUsrLockPath(prjUsrRoot))
}

// PrjUsrFree 试获取即释放：prjusr 单例锁当前是否空闲（供「选目录前置校验」用，不持有）。
// 非 ErrBusy 错误（锁设施故障 / 平台不支持）→ **放行**（true）：占用校验是体验增强，
// 不应因锁设施故障阻断用户操作。
func PrjUsrFree(prjUsrRoot string) bool {
	l, err := AcquirePrjUsr(prjUsrRoot)
	if err != nil {
		return !errors.Is(err, ErrBusy)
	}
	l.Release()
	return true
}

// workDirLocksDir 返回占用锁根目录：`<home>/.chonkpilot/locks`（与 llm server 的
// per-session 锁同根，见 sessionlock_windows.go）；home 不可解析 → 回落系统临时目录。
func workDirLocksDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".chonkpilot", "locks")
}

// WorkDirLockPath 返回 work-dir 占用锁文件路径：
//
//	<home>/.chonkpilot/locks/workdir-<sha1(canonical(workDir))[:16]>.lock
//
// 放用户数据根（不污染项目目录）；用哈希做文件名避免路径长度 / 非法字符问题；
// canonicalPath 由平台文件提供（Windows 折叠大小写：同一目录的不同拼写映射到同一把锁）。
func WorkDirLockPath(workDir string) string {
	sum := sha1.Sum([]byte(canonicalPath(workDir)))
	name := "workdir-" + hex.EncodeToString(sum[:])[:16] + ".lock"
	return filepath.Join(workDirLocksDir(), name)
}

// AcquireWorkDir 非阻塞获取 work-dir 占用锁；已被占用 → ErrBusy。
// 返回的 Lock 须在进程存活期持有（`defer lock.Release()`）。
func AcquireWorkDir(workDir string) (*Lock, error) {
	return Acquire(WorkDirLockPath(workDir))
}

// WorkDirFree 试获取即释放：work-dir 当前是否空闲（供「选目录前置校验」用，不持有）。
// 非 ErrBusy 的 IO 错误（如锁目录不可写）→ **放行**（true）：占用校验是体验增强，
// 不应因锁设施故障阻断用户操作。
func WorkDirFree(workDir string) bool {
	l, err := Acquire(WorkDirLockPath(workDir))
	if err != nil {
		return !errors.Is(err, ErrBusy)
	}
	l.Release()
	return true
}
