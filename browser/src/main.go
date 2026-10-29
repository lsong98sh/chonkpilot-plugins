// chonkpilot-browser-run —— 浏览器插件（browser_run 工具面）。
//
// 形态：manifest 插件（桥接层）+ 既有常驻引擎（chonkpilot-browser-plugin.exe，WebView2）。
// 复用层：pluginsdk（工具/配置/状态/RPC）+ pluginsdk/engineclient（引擎 UDS 桥）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk/engineclient"
)

const pluginVersion = "1.0.0"

type browserPlugin struct {
	p     *pluginsdk.Plugin
	cfg   *configManager
	eng   *engineclient.Client
	mu    sync.Mutex
	ctxID map[string]string // instance_id → context_id
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID: "browser", Name: "浏览器", Version: pluginVersion,
		Features: map[string]any{"mcp": "http", "ui": true},
	})
	if err != nil {
		slog.Error("browser: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	b := &browserPlugin{
		p:   p,
		cfg: newConfigManager(p),
		eng: engineclient.New(engineclient.Options{
			Root:       os.Getenv(pluginsdk.EnvRoot),
			Exe:        "chonkpilot-browser-plugin.exe",
			SocketName: "browser.sock",
			Logf:       p.Logger().Info,
		}),
		ctxID: map[string]string{},
	}
	p.Tool("browser_run", browserToolDesc, browserSchema(), b.run)
	p.HotTool("browser_run")
	p.OnRequest("status", pluginsdk.StatusHandler(pluginVersion, b.status))
	p.OnRequest("contexts", b.contexts)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/browser/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := b.cfg.Init(context.Background()); err != nil {
		slog.Warn("browser: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range b.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("browser: 运行结束", "err", err)
	}
}

const browserToolDesc = "驱动本机浏览器（WebView2）。action=start/stop 管理会话；其余 action 在当前 context 上执行原子操作：" +
	"navigate(url) / click(selector) / fill(selector,text) / wait(selector|ms) / eval(expr) / text(selector?) / html / " +
	"title / url / screenshot(path?) / console_dump / reload / back / forward / set_html(html) / debug_open。首次调用会自动开会话。"

func browserSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":     map[string]any{"type": "string", "description": "动作名（start/stop/navigate/click/fill/wait/eval/text/html/title/url/screenshot/console_dump/reload/back/forward/set_html/debug_open/ping）"},
			"context_id": map[string]any{"type": "string", "description": "可选：显式指定会话 context（缺省按当前实例复用）"},
			"purpose":    map[string]any{"type": "string", "description": "start / 自动开会话时的用途说明"},
			"args":       map[string]any{"type": "object", "description": "动作参数（如 {url} / {selector,text} / {expr} / {path}）"},
		},
		"required": []string{"action"},
	}
}

func (b *browserPlugin) run(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	action := strings.ToLower(strings.TrimSpace(pluginsdk.Str(args["action"])))
	if action == "" {
		return nil, errors.New("action 必填")
	}
	if err := b.eng.Ensure(ctx); err != nil {
		return nil, err
	}
	switch action {
	case "ping", "status":
		return b.eng.Call(ctx, "ping", nil)
	case "start":
		return b.start(ctx, cx, pluginsdk.Str(args["purpose"]))
	case "stop":
		id := pluginsdk.Str(args["context_id"])
		if id == "" {
			id = b.take(cx.InstanceID)
		}
		if id == "" {
			return map[string]any{"ok": true, "stopped": false, "reason": "无活动 context"}, nil
		}
		res, err := b.eng.Call(ctx, "stop", map[string]any{"context_id": id})
		if err != nil {
			return nil, err
		}
		res["context_id"] = id
		return res, nil
	}

	id := pluginsdk.Str(args["context_id"])
	if id == "" {
		id = b.get(cx.InstanceID)
	}
	if id == "" {
		res, err := b.start(ctx, cx, pluginsdk.Str(args["purpose"]))
		if err != nil {
			return nil, err
		}
		id = pluginsdk.Str(res["context_id"])
	}
	opArgs, _ := args["args"].(map[string]any)
	if opArgs == nil {
		opArgs = map[string]any{}
	}
	// screenshot 未给 path → 落配置的默认目录（LLM 参数可覆盖）。
	if action == "screenshot" && pluginsdk.Str(opArgs["path"]) == "" {
		if dir := b.cfg.Settings().ScreenshotDir; dir != "" {
			opArgs["path"] = filepath.Join(dir, fmt.Sprintf("browser-%d.png", time.Now().UnixMilli()))
		}
	}
	res, err := b.eng.Call(ctx, "operation", map[string]any{"context_id": id, "action": action, "args": opArgs})
	if err != nil {
		return nil, err
	}
	res["context_id"] = id
	return res, nil
}

func (b *browserPlugin) start(ctx context.Context, cx pluginsdk.CallContext, purpose string) (map[string]any, error) {
	cfg := b.cfg.Settings()
	if purpose == "" {
		purpose = "llm"
	}
	res, err := b.eng.Call(ctx, "start", map[string]any{
		"purpose":  purpose,
		"headless": cfg.Headless,
		"debug":    cfg.Debug,
		"width":    cfg.Width,
		"height":   cfg.Height,
	})
	if err != nil {
		return nil, err
	}
	if id := pluginsdk.Str(res["context_id"]); id != "" && cx.InstanceID != "" {
		b.mu.Lock()
		b.ctxID[cx.InstanceID] = id
		b.mu.Unlock()
	}
	return res, nil
}

func (b *browserPlugin) get(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxID[instanceID]
}

func (b *browserPlugin) take(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.ctxID[instanceID]
	delete(b.ctxID, instanceID)
	return id
}

func (b *browserPlugin) status(ctx context.Context) map[string]any {
	ping, err := b.eng.Call(ctx, "ping", nil)
	out := map[string]any{"healthy": err == nil, "socket": b.eng.Socket()}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["engine"] = ping
	}
	b.mu.Lock()
	out["contexts"] = len(b.ctxID)
	b.mu.Unlock()
	return out
}

func (b *browserPlugin) contexts(_ context.Context, _ map[string]any) (any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := make([]map[string]string, 0, len(b.ctxID))
	for instance, id := range b.ctxID {
		list = append(list, map[string]string{"instance_id": instance, "context_id": id})
	}
	return map[string]any{"contexts": list}, nil
}
