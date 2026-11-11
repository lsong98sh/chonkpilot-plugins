// chonkpilot-desktop-run —— 桌面操作插件（desktop_run 工具面）。
//
// 形态：manifest 插件（桥接层）+ 既有常驻引擎（chonkpilot-desktop-plugin.exe，Win32/UIA）。
// 复用层：pluginsdk + pluginsdk/engineclient（与 browser 插件同一套代码）。
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk/engineclient"
	"github.com/lsong98sh/chonkpilot-util/pathx"
)

const pluginVersion = "1.0.0"

// toolContractFS 内嵌 desktop_run 的 `.tool.md`（schema / async / timeout / hot 的唯一源）。
//
//go:embed tools/desktop/*.tool.md
var toolContractFS embed.FS

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
	// 契约唯一源：内嵌的 `tools/desktop/desktop_run.tool.md`。
	// `_meta`（async/timeout/on_timeout/category/hot，source=md）由 contract.MetaFor 生成。
	tool, err := loadDesktopContract()
	if err != nil {
		slog.Error("desktop: 解析内嵌工具契约失败", "err", err)
		os.Exit(1)
	}
	p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), d.run)
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

// loadDesktopContract 解析内嵌的 desktop_run `.tool.md`。
func loadDesktopContract() (contract.Tool, error) {
	const p = "tools/desktop/desktop_run.tool.md"
	raw, err := toolContractFS.ReadFile(p)
	if err != nil {
		return contract.Tool{}, fmt.Errorf("读取契约 %s: %w", p, err)
	}
	tool, err := contract.Parse(string(raw))
	if err != nil {
		return contract.Tool{}, fmt.Errorf("%s: %w", p, err)
	}
	if tool.Name == "" {
		return contract.Tool{}, fmt.Errorf("%s: 缺少 name", p)
	}
	return tool, nil
}

func (d *desktopPlugin) run(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	action := strings.ToLower(strings.TrimSpace(pluginsdk.Str(args["action"])))
	if action == "" {
		return nil, errors.New("action 必填")
	}
	opArgs, _ := args["args"].(map[string]any)
	if opArgs == nil {
		opArgs = map[string]any{}
	}
	if err := d.eng.Ensure(ctx); err != nil {
		return nil, err
	}
	switch action {
	case "ping", "status":
		return d.eng.Call(ctx, "ping", nil)
	case "start":
		// start 支持 meta 绑定目标窗口（title/class/hwnd），使「bind at start」可达。
		return d.start(ctx, cx, pluginsdk.Str(args["purpose"]), targetMeta(opArgs, args))
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
		// 需要目标窗口的动作在自动开会话时顺带预绑定，后续同类动作即可省略目标。
		var meta map[string]any
		if requiresTarget(action) {
			meta = targetMeta(opArgs)
		}
		res, err := d.start(ctx, cx, pluginsdk.Str(args["purpose"]), meta)
		if err != nil {
			return nil, err
		}
		id = pluginsdk.Str(res["context_id"])
	}
	if action == "screenshot" || action == "shot" {
		raw := pluginsdk.Str(opArgs["path"])
		if raw == "" {
			if dir := pathx.ExpandHome(d.cfg.Settings().ScreenshotDir); dir != "" {
				raw = filepath.Join(dir, fmt.Sprintf("desktop-%d.png", time.Now().UnixMilli()))
			}
		}
		path, err := resolveSandboxPath(cx, raw)
		if err != nil {
			return nil, err
		}
		if path != "" {
			opArgs["path"] = path
		}
	}
	res, err := d.eng.Call(ctx, "operation", map[string]any{"context_id": id, "action": action, "args": opArgs})
	if err != nil {
		return nil, err
	}
	res["context_id"] = id
	return res, nil
}

// requiresTarget 判断动作是否必须绑定目标窗口（与引擎 desktop.target 的分支一致）。
func requiresTarget(action string) bool {
	switch action {
	case "window_info", "focus", "click", "input", "type", "key",
		"move", "size", "resize", "minimize", "maximize", "restore":
		return true
	}
	return false
}

// targetMeta 从参数里提取 start 绑定用的目标窗口字段（hwnd/title/class）。
// 多个来源按先后顺序取第一个非空值：先 nested args，再顶层 args（防御性）。
func targetMeta(sources ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, src := range sources {
		if src == nil {
			continue
		}
		if _, ok := out["hwnd"]; !ok {
			if v, ok := src["hwnd"]; ok {
				if n, valid := hwndValue(v); valid {
					out["hwnd"] = n
				}
			}
		}
		for _, k := range []string{"title", "class"} {
			if _, ok := out[k]; ok {
				continue
			}
			if s := strings.TrimSpace(pluginsdk.Str(src[k])); s != "" {
				out[k] = s
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hwndValue 把 hwnd 参数（数字/数字字符串）转为 uint64；0 或非法返回 false。
func hwndValue(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return uint64(n), true
		}
	case int:
		if n > 0 {
			return uint64(n), true
		}
	case int64:
		if n > 0 {
			return uint64(n), true
		}
	case string:
		if parsed, err := strconv.ParseUint(strings.TrimSpace(n), 10, 64); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}

func (d *desktopPlugin) start(ctx context.Context, cx pluginsdk.CallContext, purpose string, meta map[string]any) (map[string]any, error) {
	if purpose == "" {
		purpose = "llm"
	}
	params := map[string]any{"purpose": purpose}
	if len(meta) > 0 {
		params["meta"] = meta
	}
	res, err := d.eng.Call(ctx, "start", params)
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
