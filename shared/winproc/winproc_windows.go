//go:build windows

// Package winproc 提供 spawn 外部进程时的进程创建属性。
// 目的：console 子系统 exe（executor / 引擎 exe 等）被 GUI 或服务进程 spawn 时，
// 抑制瞬时/可见的控制台窗口（黑窗），且不影响 stdin/stdout/stderr 管道通信。
package winproc

import (
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"
)

// createNoWindow 为 Windows CREATE_NO_WINDOW（0x08000000）：
// 控制台进程以「无控制台窗口」方式创建，从根源避免控制台被创建/闪现。
const createNoWindow = 0x08000000

// SysProcAttr 返回隐藏子进程控制台窗口的进程创建属性：
// HideWindow（STARTF_USESHOWWINDOW + SW_HIDE）抑制窗口显示，
// CREATE_NO_WINDOW 避免控制台本身被创建——两者并用，覆盖瞬时闪窗。
// 非控制台（GUI 子系统）子进程不受影响；stdin/stdout/stderr 管道通信保持不变。
func SysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// KillTree 终止指定进程及其**全部子孙进程**（Windows：`taskkill /T /F`）。
//
// 用途：取消/超时强杀执行器（直接子进程）时，执行器自行拉起的孙进程
// （如 script 执行器 → cmd.exe → ping.exe）会孤儿化；仅 TerminateProcess 直接子进程不够，
// 必须按进程树终止（run_tool_async J 的「无孤儿」判据）。
func KillTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	c := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	c.SysProcAttr = SysProcAttr()
	return c.Run()
}

// ─── kill-on-close Job Object（孤儿进程回收，C-40）──────────────────────────
//
// 目的：宿主 spawn 的 console 执行器进程被上层取消/超时强杀（TerminateProcess）时，
// 其自行拉起的孙进程（如 chromedp 拉起的 Chrome）若无 Job 约束会孤儿化。让**本进程**
// 加入一个 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 的 Job Object：本进程终止（句柄随进程
// 关闭）→ 该 Job 内所有进程（本进程创建的子/孙进程默认继承 Job）随之被终止。

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procGetCurrentProcess        = kernel32.NewProc("GetCurrentProcess")
)

// jobObjectBasicLimitInformation 对应 Windows JOBOBJECT_BASIC_LIMIT_INFORMATION
// （字段顺序/类型对齐 MSDN；Go 自动补齐 64 位对齐填充）。
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

// ioCounters 对应 Windows IO_COUNTERS。
type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

// jobObjectExtendedLimitInformationStruct 对应 Windows JOBOBJECT_EXTENDED_LIMIT_INFORMATION。
type jobObjectExtendedLimitInformationStruct struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var jobOnce sync.Once

// EnsureKillOnCloseJob 令当前进程加入一个 kill-on-close 的 Job Object（幂等：进程内只建一次；
// 非 Windows 平台为 no-op）。用于防止宿主强杀本进程时其子/孙进程孤儿化（C-40）。
// 创建/关联失败（如已处于不允许嵌套的 Job，或权限不足）→ 静默忽略，退化为无 Job 保障
// （不改变既有行为）。**句柄不显式关闭**：保持打开至进程退出（随之自动关闭 → 触发 kill-on-close）。
func EnsureKillOnCloseJob() {
	jobOnce.Do(func() {
		h, _, _ := procCreateJobObjectW.Call(0, 0)
		if h == 0 {
			return
		}
		var info jobObjectExtendedLimitInformationStruct
		info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
		procSetInformationJobObject.Call(
			h,
			jobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			unsafe.Sizeof(info),
		)
		self, _, _ := procGetCurrentProcess.Call()
		procAssignProcessToJobObject.Call(h, self)
	})
}
