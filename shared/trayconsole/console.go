// Package trayconsole 提供 Windows **托盘图标 + 控制台式输出窗口**（零第三方依赖）。
//
// 用途：常驻插件进程（如 vfts / codegraph indexer）以 GUI 子系统启动时没有控制台，
// 需要一个可观察的输出面：
//   - 启动即隐藏（只留托盘图标，不打扰用户）；
//   - 单击 / 双击托盘图标 → 显示输出窗口（只读、等宽字体、自动滚到底）；
//   - 关闭窗口 = 收起回托盘（进程继续运行）；
//   - 右键托盘 → 打开输出窗口 / 清空输出 / 隐藏窗口。
//
// `Console` 实现 io.Writer：按行缓存（保留最后 N 行），日志 handler 可直接
// `io.MultiWriter(os.Stderr, console)` 双写（宿主日志 + 托盘窗口）。
//
// 线程约束：Win32 窗口与消息循环必须跑在同一 OS 线程（runtime.LockOSThread），
// 故 Start 在独立 goroutine 内建窗并跑 GetMessage 循环；Write 可从任意 goroutine 调用。
package trayconsole

import (
	"errors"
	"strings"
	"sync"
)

// ErrUnsupported 表示当前平台不提供托盘窗口（非 Windows 构建）。
var ErrUnsupported = errors.New("trayconsole: unsupported on this platform")

// ErrAlreadyRunning 表示同名窗口类已被另一个进程注册（单实例判定命中）。
// 仅在 Options.SingleInstance=true 时可能返回；调用方据此静默退出。
var ErrAlreadyRunning = errors.New("trayconsole: another instance is already running")

// Options 描述一个托盘控制台实例。
type Options struct {
	// Title 是窗口标题（缺省 "ChonkPilot 输出"）。
	Title string
	// Tooltip 是托盘提示文字（缺省 = Title；超过 127 字符截断）。
	Tooltip string
	// Header 是建窗后立即输出的摘要行（如插件名 / pid / 插件桥地址）。
	Header []string
	// MaxLines 是输出窗口保留的最大行数（缺省 5000；超出从最旧行开始删除）。
	MaxLines int
	// ClassName 是窗口类名（缺省 "ChonkPilotTrayConsole"）。
	// **单实例用途**：不同宿主 / 插件须使用不同的类名（见 Indexer 的实例键后缀）。
	ClassName string
	// SingleInstance=true → 单实例判定（**窗口类 + 同名窗口**）：注册窗口类后、
	// 建窗前用 `FindWindowW(<类名>)` 查同名顶层窗口；已存在 → 返回 ErrAlreadyRunning
	// （不建窗 / 不挂托盘）。用于替代文件单实例锁（2026-10-19 用户定稿）。
	SingleInstance bool
}

// Console 是托盘 + 输出窗口实例（实现 io.Writer）。
type Console struct {
	opts Options

	mu      sync.Mutex
	pending []string // 待 UI 线程追加的行
	partial string   // 尚未以 '\n' 结尾的半行
	posted  bool     // 已投递 wmAppend（避免重复投递洪泛）

	st      platformState // 平台相关句柄（Windows：窗口 / 控件 / GDI / 托盘）
	closed  bool
	closeCh chan struct{}
	once    sync.Once
}

const defaultMaxLines = 5000

// normalizeOptions 填充缺省值并归一标题 / 提示。
func normalizeOptions(opts Options) Options {
	if strings.TrimSpace(opts.Title) == "" {
		opts.Title = "ChonkPilot 输出"
	}
	if strings.TrimSpace(opts.Tooltip) == "" {
		opts.Tooltip = opts.Title
	}
	if strings.TrimSpace(opts.ClassName) == "" {
		opts.ClassName = "ChonkPilotTrayConsole"
	}
	if opts.MaxLines <= 0 {
		opts.MaxLines = defaultMaxLines
	}
	return opts
}

// Write 按行缓存输出（实现 io.Writer）。半行暂存，遇到 '\n' 才整行送 UI。
func (c *Console) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return len(p), nil
	}
	s := c.partial + string(p)
	lines := strings.Split(s, "\n")
	c.partial = lines[len(lines)-1]
	for _, ln := range lines[:len(lines)-1] {
		c.pending = append(c.pending, strings.TrimSuffix(ln, "\r"))
	}
	if len(c.partial) > 8192 { // 防御：无换行的超长输出按行截断
		c.pending = append(c.pending, c.partial)
		c.partial = ""
	}
	needPost := len(c.pending) > 0 && !c.posted
	if needPost {
		c.posted = true
	}
	hwnd := c.st.hwnd
	c.mu.Unlock()
	if needPost && hwnd != 0 {
		postAppend(hwnd)
	}
	return len(p), nil
}

// Close 移除托盘图标并销毁窗口（幂等）。
func (c *Console) Close() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		hwnd := c.st.hwnd
		c.mu.Unlock()
		if hwnd != 0 {
			postClose(hwnd)
			<-c.closeCh
		}
	})
}
