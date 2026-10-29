// chonkpilot-desktop-run —— 桌面操作插件（desktop_run 工具面）。
//
// 形态：manifest 插件（桥接层）+ 既有常驻引擎（chonkpilot-desktop-plugin.exe，Win32/UIA）。
// 复用层：pluginsdk + pluginsdk/engineclient（与 browser 插件同一套代码）。
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

type desktopPlugin struct {
	p     *pluginsdk.Plugin
	cfg   *configManager
	eng   *engineclient.Client
	mu    sync.Mutex
	ctxID map[string]string
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID: "desktop", Name: "桌面操作", Version: pluginVersion,
		Features: map[string]any{"mcp": "http", "ui": true},
	})
	if err != nil {
		slog.Error("desktop: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	d := &desktopPlugin{
		p:   p,
		cfg: newConfigManager(p),
		eng: engineclient.New(engineclient.Options{
			Root:       os.Getenv(pluginsdk.EnvRoot),
			Exe:        "chonkpilot-desktop-plugin.exe",
			SocketName: "desktop.sock",
			Logf:       p.Logger().Info,
		}),
		ctxID: map[string]string{},
	}
	p.Tool("desktop_run", desktopToolDesc, desktopSchema(), d.run)
	p.HotTool("desktop_run")
	p.OnRequest("status", pluginsdk.StatusHandler(pluginVersion, d.status))
	p.OnRequest("contexts", d.contexts)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/desktop/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := d.cfg.Init(context.Background()); err != nil {
		slog.Warn("desktop: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range d.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("desktop: 运行结束", "err", err)
	}
}

const desktopToolDesc = "操作本机桌面窗口（Win32）。action=start/stop 管理会话；其余 action 在当前 context 上执行原子操作：" +
	"list_windows() / find_window(title|class) / window_info() / focus() / click(x,y) / input(text) / key(keys) / " +
	"screenshot(path?) / move(x,y) / size(w,h) / minimize / maximize / restore。首次调用会自动开会话。"

func desktopSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":     map[string]any{"type": "string", "description": "动作名（start/stop/list_windows/find_window/window_info/focus/click/input/key/screenshot/move/size/minimize/maximize/restore/ping）"},
			"context_id": map[string]any{"type": "string", "description": "可选：显式指定会话 context（缺省按当前实例复用）"},
			"purpose":    map[string]any{"type": "string", "description": "start / 自动开会话时的用途说明"},
			"args":       map[string]any{"type": "object", "description": "动作参数（如 {title}/{x,y}/{text}/{keys}/{path}/{w,h}）"},
		},
		"required": []string{"action"},
	}
}

func (d *desktopPlugin) run(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	action := strings.ToLower(strings.TrimSpace(pluginsdk.Str(args["action"])))
	if action == "" {
		return nil, errors.New("action 必填")
	}
	if err := d.eng.Ensure(ctx); err != nil {
		return nil, err
	}
	switch action {
	case "ping", "status":
		return d.eng.Call(ctx, "ping", nil)
	case "start":
		return d.start(ctx, cx, pluginsdk.Str(args["purpose"]))
	case "stop":
		id := pluginsdk.Str(args["context_id"])
		if id == "" {
			id = d.take(cx.InstanceID)
		}
		if id == "" {
			return map[string]any{"ok": true, "stopped": false, "reason": "无活动 context"}, nil
		}
		res, err := d.eng.Call(ctx, "stop", map[string]any{"context_id": id})
		if err != nil {
			return nil, err
		}
		res["context_id"] = id
		return res, nil
	}

	id := pluginsdk.Str(args["context_id"])
	if id == "" {
		id = d.get(cx.InstanceID)
	}
	if id == "" {
		res, err := d.start(ctx, cx, pluginsdk.Str(args["purpose"]))
		if err != nil {
			return nil, err
		}
		id = pluginsdk.Str(res["context_id"])
	}
	opArgs, _ := args["args"].(map[string]any)
	if opArgs == nil {
		opArgs = map[string]any{}
	}
	if action == "screenshot" && pluginsdk.Str(opArgs["path"]) == "" {
		if dir := d.cfg.Settings().ScreenshotDir; dir != "" {
			opArgs["path"] = filepath.Join(dir, fmt.Sprintf("desktop-%d.png", time.Now().UnixMilli()))
		}
	}
	res, err := d.eng.Call(ctx, "operation", map[string]any{"context_id": id, "action": action, "args": opArgs})
	if err != nil {
		return nil, err
	}
	res["context_id"] = id
	return res, nil
}

func (d *desktopPlugin) start(ctx context.Context, cx pluginsdk.CallContext, purpose string) (map[string]any, error) {
	if purpose == "" {
		purpose = "llm"
	}
	res, err := d.eng.Call(ctx, "start", map[string]any{"purpose": purpose})
	if err != nil {
		return nil, err
	}
	if id := pluginsdk.Str(res["context_id"]); id != "" && cx.InstanceID != "" {
		d.mu.Lock()
		d.ctxID[cx.InstanceID] = id
		d.mu.Unlock()
	}
	return res, nil
}

func (d *desktopPlugin) get(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctxID[instanceID]
}

func (d *desktopPlugin) take(instanceID string) string {
	if instanceID == "" {
		return ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.ctxID[instanceID]
	delete(d.ctxID, instanceID)
	return id
}

func (d *desktopPlugin) status(ctx context.Context) map[string]any {
	ping, err := d.eng.Call(ctx, "ping", nil)
	out := map[string]any{"healthy": err == nil, "socket": d.eng.Socket()}
	if err != nil {
		out["error"] = err.Error()
	} else {
		out["engine"] = ping
	}
	d.mu.Lock()
	out["contexts"] = len(d.ctxID)
	d.mu.Unlock()
	return out
}

func (d *desktopPlugin) contexts(_ context.Context, _ map[string]any) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := make([]map[string]string, 0, len(d.ctxID))
	for instance, id := range d.ctxID {
		list = append(list, map[string]string{"instance_id": instance, "context_id": id})
	}
	return map[string]any{"contexts": list}, nil
}
