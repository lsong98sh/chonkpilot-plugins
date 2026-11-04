//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lsong98sh/chonkpilot-util/winproc"
)

//go:embed toast.ps1
var toastScript string

//go:embed msgbox.ps1
var msgboxScript string

// toastAppID 是系统通知使用的 AppUserModelID。
const toastAppID = "ChonkPilot"

// systemNotifier 用内嵌 PowerShell 脚本调用 WinRT Toast（零第三方依赖）。
type systemNotifier struct {
	logf     func(format string, args ...any)
	fallback func() string // 读取当前 notify-fallback（log | window）
	once     sync.Once
	dir      string
	err      error
}

// newSystemNotifier 构造 Windows 系统通知实现。
func newSystemNotifier(logf func(format string, args ...any), fallback func() string) Notifier {
	return &systemNotifier{logf: logf, fallback: fallback}
}

// Notify 弹一条系统通知；失败按配置兜底（log / MessageBox）。
func (n *systemNotifier) Notify(title, body, clickURL string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if err := n.ensureScripts(); err != nil {
		return err
	}
	err := runPowerShell(filepath.Join(n.dir, "toast.ps1"),
		"-Title", title, "-Body", body, "-Url", strings.TrimSpace(clickURL), "-AppID", toastAppID)
	if err == nil {
		return nil
	}
	if n.logf != nil {
		n.logf("crontab: 系统通知失败（%v）", err)
	}
	if n.fallback != nil && n.fallback() == fallbackWindow {
		if werr := runPowerShell(filepath.Join(n.dir, "msgbox.ps1"),
			"-Title", title, "-Body", body); werr == nil {
			return nil
		} else if n.logf != nil {
			n.logf("crontab: 兜底窗口也失败（%v）", werr)
		}
	}
	return err
}

// ensureScripts 首次调用把内嵌脚本写到插件数据目录（UTF-8 BOM，避免 PowerShell 5.1 中文乱码）。
func (n *systemNotifier) ensureScripts() error {
	n.once.Do(func() {
		n.dir = filepath.Join(os.TempDir(), "chonkpilot-crontab")
		if err := os.MkdirAll(n.dir, 0o700); err != nil {
			n.err = err
			return
		}
		n.err = writePS1(filepath.Join(n.dir, "toast.ps1"), toastScript)
		if n.err == nil {
			n.err = writePS1(filepath.Join(n.dir, "msgbox.ps1"), msgboxScript)
		}
	})
	return n.err
}

// writePS1 写脚本文件（带 UTF-8 BOM）。
func writePS1(path, script string) error {
	return os.WriteFile(path, []byte("\ufeff"+script), 0o600)
}

// runPowerShell 以隐藏窗口执行脚本（同步等待结束）。
func runPowerShell(script string, args ...string) error {
	argv := append([]string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-ExecutionPolicy", "Bypass", "-File", script}, args...)
	cmd := exec.Command("powershell", argv...)
	cmd.SysProcAttr = winproc.SysProcAttr()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
