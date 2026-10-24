// chonkpilot-history —— 文件历史插件（**进程外 exe 形态**，2026-10-24 从宿主编译期内嵌改为外挂）。
//
// 业务实现 **逐字复用** `github.com/chonkpilot/chonkpilot-plugin-history`（原 server 内嵌包）：
// 独立 ref 的检查点链（`commit-tree` + `update-ref refs/chonkpilot/<slug>`、临时 index 在
// `<workdir>/.chonkpilot/history/index`）、四个触发点（gateway 前置打点钩子 / session-complete
// 轮末补点 / filesys.changed 置脏 / instance-register 建 workState）、`history_*` 四个工具的
// 名称与 schema、`history.status` 回写、熔断（fuseThreshold）与 git 缺失降级（摘工具）
// **全部不变** —— 本 main 只做「通道适配」（见 bridgebus）。
//
// 同步前置钩子（触发点 ①）在进程外形态下的落地：登记工具时仍声明 `pre_hook_subject`
// （history-pre-tool-hook），宿主 gateway 在执行**任意**工具前向该主题发一次同步请求；
// 本进程经宿主**既有**能力 `plugin.tool.bind` 绑定该主题（见 pluginsdk.BindSubject），
// 请求被转发为 plugin.tool.call → bridgebus 派发给插件 handler → 打点成功写回结果、
// 失败返回 error → 宿主把该错误写回请求 → gateway **拒绝该次工具调用**（与改造前一致）。
// 超时口径：gateway 侧 CallTimeout=60s 决定拒绝时刻；插件桥单次调用兜底 11min，不先到。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
	"github.com/chonkpilot/chonkpilot-plugin/bridgebus"
	history "github.com/chonkpilot/chonkpilot-plugin-history"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:       "history",
		Name:     "文件历史",
		Version:  pluginVersion,
		Timeout:  120 * time.Second,
		Features: map[string]any{"mq": true},
	})
	if err != nil {
		slog.Error("history: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	local, err := mq.New(mq.Options{})
	if err != nil {
		slog.Error("history: 建本地派发总线失败", "err", err)
		os.Exit(1)
	}
	bus := bridgebus.New(local, bridgebus.Options{
		Read:  p.DataRead,
		Write: p.DataWrite,
		Call:  p.Call,
		Subscribe: func(pattern string, fn func(ctx context.Context, topic string, payload []byte)) {
			p.Subscribe(pattern, fn)
		},
		Bind: func(subject string, fn func(ctx context.Context, payload json.RawMessage) (any, error)) error {
			return p.BindSubject(subject, fn)
		},
	})
	defer bus.Close()

	logf := func(format string, args ...any) { p.Logger().Info(fmt.Sprintf(format, args...)) }

	plug := history.New()
	// git 不存在 → Start 返回 nil（功能禁用、工具摘除；不视为启动失败），与改造前一致。
	if err := plug.Start(plugin.Deps{Bus: bus, Logf: logf}); err != nil {
		slog.Error("history: 启动失败", "err", err)
		os.Exit(1)
	}
	defer plug.Stop()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("history: 运行结束", "err", err)
	}
}
