// chonkpilot-remote-plugin —— Remote 插件（桌面侧）：主动出站连接 Remote Server，
// 上报设备 / 工作区，把 Server 的设备 RPC 转发成 `plugin/v1/*`，并把桌面事件归一为产品事件回推。
//
// 本插件**不注册任何 MCP 工具**（本期只做通道 + UI）；配置页 / 状态栏徽标由 webui（embed）自持。
// 协议见 docs/spec/10-architecture/26-remote.md §4。
package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const (
	pluginID        = "remote"
	pluginVersion   = "1.0.0"
	protocolVersion = "remote/1"
	uiAssetRoot     = "webui"
)

//go:embed all:webui
var webuiFS embed.FS

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:       pluginID,
		Name:     "远程接入",
		Version:  pluginVersion,
		Features: map[string]any{"ui": true},
	})
	if err != nil {
		slog.Error("remote: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()
	log := p.Logger()

	cfg := newConfigManager(log)
	if err := cfg.Init(context.Background()); err != nil {
		log.Warn("remote: 读取配置失败（按未配置启动）", "err", err)
	}
	dev := newDeviceStore(pluginsdk.DefaultDataDir(), pluginVersion, log)
	ws := newWorkspaceStore()
	run := newRunningSet()
	asks := newAskCache()
	c := newClient(p, log, cfg, dev, ws, run, asks)
	cfg.onChange = c.NotifyChange

	p.Subscribe("instance-register", func(_ context.Context, _ string, payload []byte) {
		c.onInstanceRegister(payload)
	})
	p.Subscribe("instance-exit", func(_ context.Context, _ string, payload []byte) {
		c.onInstanceExit(payload)
	})
	for _, topic := range forwardedTopics {
		topic := topic
		p.Subscribe(topic, func(_ context.Context, _ string, payload []byte) {
			c.onBusEvent(topic, payload)
		})
	}

	for name, fn := range cfg.Actions() {
		p.OnRequest(name, fn)
	}
	p.OnRequest("test", c.handleTest)
	p.OnRequest("status", c.handleStatus)
	p.OnRequest("badge", c.handleBadge)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.run(ctx)

	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		log.Warn("remote: 运行结束", "err", err)
	}
	cancel()
}

// uiAssetGet 返回 embed 内 webui 资源（宿主经 UDS `plugin/v1/ui.asset.get` 取字节）。
func uiAssetGet(_ context.Context, payload map[string]any) (any, error) {
	raw, _ := payload["path"].(string)
	rel, ok := normalizeUIAssetPath(raw)
	if !ok {
		return nil, fmt.Errorf("invalid_path: %q", raw)
	}
	b, err := fs.ReadFile(webuiFS, uiAssetRoot+"/"+rel)
	if err != nil {
		return nil, fmt.Errorf("not_found: %s", rel)
	}
	return map[string]any{"data": b, "size": len(b)}, nil
}

// normalizeUIAssetPath 把宿主传入路径归一为 webui 根内相对路径（越界一律拒绝）。
func normalizeUIAssetPath(p string) (string, bool) {
	s := strings.TrimSpace(p)
	if s == "" || strings.ContainsAny(s, `\:`) || strings.HasPrefix(s, "/") {
		return "", false
	}
	s = strings.TrimPrefix(s, uiAssetRoot+"/")
	if s == "" || s == "." || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "", false
	}
	clean := path.Clean(s)
	if clean != s || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
