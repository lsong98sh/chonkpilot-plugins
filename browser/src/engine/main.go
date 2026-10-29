// chonkpilot-browser-plugin：browser 常驻插件（GUI 子系统 + 托盘 + UDS 服务）。
//
// 形态：
//   - 独立 exe，不注册为 MCP 工具、不接 gateway；
//   - 由 supervisor（宿主）拉起，单实例（托盘窗口类判定）；
//   - 对外只暴露 UDS 三端点：start / stop / operation（协议见 internal/pluginproto）。
//
// 用法（脚本侧，Python 示例见 docs/tool-plane §3.9）：
//
//	sock = %LOCALAPPDATA%\chonkpilot\plugins\browser.sock
//	→ {"id":"1","endpoint":"start","params":{"purpose":"…"}} → {"context_id":"ctx-1"}
//	→ {"id":"2","endpoint":"operation","params":{"context_id":"ctx-1","action":"navigate","args":{"url":"…"}}}
//	→ {"id":"3","endpoint":"stop","params":{"context_id":"ctx-1"}}
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lsong98sh/chonkpilot-plugins/browser/engine/internal/browserplugin"
	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginhost"
	"github.com/lsong98sh/chonkpilot-plugins/shared/trayconsole"
)

func main() { os.Exit(run()) }

func run() int {
	socket := flag.String("socket", "", "UDS 监听路径（缺省 %LOCALAPPDATA%\\chonkpilot\\plugins\\browser.sock）")
	debug := flag.Bool("debug", false, "打开 WebView2 DevTools（并允许右键菜单）")
	flag.Parse()

	logger := buildLogger()
	logf := func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }

	srv := pluginhost.New(pluginhost.Options{
		SocketPath: *socket,
		Logf:       logf,
		NewEngine: func(opts pluginhost.ContextOptions) (pluginhost.Engine, error) {
			if *debug {
				opts.Debug = true
			}
			return browserplugin.NewWebViewEngine(opts)
		},
	})
	if err := srv.Start(); err != nil {
		logf("start failed: %v", err)
		return 1
	}
	logf("browser plugin ready on %s", srv.SocketPath())
	defer func() { _ = srv.Close() }()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	logf("browser plugin shutting down")
	return 0
}

// buildLogger 输出到 stderr + 托盘窗口 + 日志文件。
func buildLogger() *slog.Logger {
	writers := []any{}
	writers = append(writers, os.Stderr)

	if ui, err := trayconsole.Start(trayconsole.Options{
		Title:          "ChonkPilot Browser 插件",
		Tooltip:        "浏览器常驻插件（UDS）",
		ClassName:      "ChonkpilotTrayConsole-browser-plugin",
		SingleInstance: true,
		MaxLines:       2000,
		Header:         []string{"browser plugin（start/stop/operation over UDS）"},
	}); err == nil {
		writers = append(writers, ui)
	} else if !errors.Is(err, trayconsole.ErrAlreadyRunning) {
		fmt.Fprintf(os.Stderr, "tray console unavailable: %v\n", err)
	}

	if f, err := openLogFile("browser-plugin.log"); err == nil {
		writers = append(writers, f)
	}
	return slog.New(slog.NewTextHandler(multiWriter(writers), &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// multiWriter 组合多个 io.Writer（避免额外依赖 io.MultiWriter 的可变参数限制）。
func multiWriter(ws []any) *multiW {
	out := &multiW{}
	for _, w := range ws {
		if iw, ok := w.(interface{ Write([]byte) (int, error) }); ok {
			out.ws = append(out.ws, iw)
		}
	}
	return out
}

type multiW struct {
	ws []interface{ Write([]byte) (int, error) }
}

func (m *multiW) Write(p []byte) (int, error) {
	for _, w := range m.ws {
		_, _ = w.Write(p)
	}
	return len(p), nil
}

// openLogFile 打开插件日志文件（追加）。
func openLogFile(name string) (*os.File, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "chonkpilot", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}
