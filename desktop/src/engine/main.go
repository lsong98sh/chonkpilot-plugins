// chonkpilot-desktop-plugin：desktop 常驻插件（GUI 子系统 + 托盘 + UDS 服务）。
//
// 与 browser 插件同构（同一 pluginhost，只换引擎）：
//   - 独立 exe，不注册为 MCP 工具、不接 gateway；
//   - UDS 三端点：start / stop / operation；
//   - 会话回收只释放会话，不关闭目标窗口。
//
// 脚本侧示例：
//
//	{"endpoint":"start","params":{"purpose":"…","meta":{"title":"记事本"}}}
//	{"endpoint":"operation","params":{"context_id":"ctx-1","action":"screenshot","args":{"path":"C:\\tmp\\a.png"}}}
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lsong98sh/chonkpilot-plugins/desktop/engine/internal/desktopplugin"
	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginhost"
	"github.com/lsong98sh/chonkpilot-plugins/shared/trayconsole"
)

func main() { os.Exit(run()) }

func run() int {
	socket := flag.String("socket", "", "UDS 监听路径（缺省 %LOCALAPPDATA%\\chonkpilot\\plugins\\desktop.sock）")
	flag.Parse()

	logger := buildLogger()
	logf := func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) }

	srv := pluginhost.New(pluginhost.Options{
		SocketPath: *socket,
		Logf:       logf,
		NewEngine:  desktopplugin.New,
	})
	if err := srv.Start(); err != nil {
		logf("start failed: %v", err)
		return 1
	}
	logf("desktop plugin ready on %s", srv.SocketPath())
	defer func() { _ = srv.Close() }()

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	logf("desktop plugin shutting down")
	return 0
}

// buildLogger 输出到 stderr + 托盘窗口 + 日志文件。
func buildLogger() *slog.Logger {
	mw := &multiWriter{}
	mw.add(os.Stderr)

	if ui, err := trayconsole.Start(trayconsole.Options{
		Title:          "ChonkPilot Desktop 插件",
		Tooltip:        "桌面常驻插件（UDS）",
		ClassName:      "ChonkpilotTrayConsole-desktop-plugin",
		SingleInstance: true,
		MaxLines:       2000,
		Header:         []string{"desktop plugin（start/stop/operation over UDS）"},
	}); err == nil {
		mw.add(ui)
	} else if !errors.Is(err, trayconsole.ErrAlreadyRunning) {
		fmt.Fprintf(os.Stderr, "tray console unavailable: %v\n", err)
	}

	if f, err := openLogFile("desktop-plugin.log"); err == nil {
		mw.add(f)
	}
	return slog.New(slog.NewTextHandler(mw, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// multiWriter 组合多个 writer。
type multiWriter struct {
	ws []interface{ Write([]byte) (int, error) }
}

func (m *multiWriter) add(w any) {
	if iw, ok := w.(interface{ Write([]byte) (int, error) }); ok {
		m.ws = append(m.ws, iw)
	}
}

func (m *multiWriter) Write(p []byte) (int, error) {
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
