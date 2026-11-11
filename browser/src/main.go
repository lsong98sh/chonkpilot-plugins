// chonkpilot-browser-run —— 浏览器插件（browser_run 工具面）。
//
// 形态：manifest 插件（桥接层）+ 既有常驻引擎（chonkpilot-browser-plugin.exe，WebView2）。
// 复用层：pluginsdk（工具/配置/状态/RPC）+ pluginsdk/engineclient（引擎 UDS 桥）。
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk/engineclient"
	"github.com/lsong98sh/chonkpilot-util/pathx"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
)

const pluginVersion = "1.0.0"

// toolContractsFS 内嵌 browser_run 的 `.tool.md`（契约是 schema / 策略的唯一源）。
//
//go:embed tools/browser/*.tool.md
var toolContractsFS embed.FS

// maxWaitTimeoutMs 是 wait 轮询上限：超过它会让该 context 长时间持锁，后续调用排队超时。
const maxWaitTimeoutMs = 600000

type browserPlugin struct {
	p     *pluginsdk.Plugin
	cfg   *configManager
	eng   *engineclient.Client
	mu    sync.Mutex
	ctxID map[string]string // instance_id → context_id
}

func main() {
	tool, err := loadBrowserContract()
	if err != nil {
		slog.Error("browser: 解析内嵌工具契约失败", "err", err)
		os.Exit(1)
	}
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
	// 契约（schema / 描述 / 策略）全部来自内嵌 md；`_meta.source=md` 由 contract.MetaFor 写入。
	p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), b.run)
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

// loadBrowserContract 解析内嵌 `.tool.md`（唯一契约源）。
func loadBrowserContract() (contract.Tool, error) {
	paths, err := fs.Glob(toolContractsFS, "tools/browser/*.tool.md")
	if err != nil {
		return contract.Tool{}, err
	}
	if len(paths) != 1 {
		return contract.Tool{}, fmt.Errorf("内嵌契约数量 = %d，期望 1", len(paths))
	}
	raw, err := toolContractsFS.ReadFile(paths[0])
	if err != nil {
		return contract.Tool{}, fmt.Errorf("%s: %w", paths[0], err)
	}
	t, err := contract.Parse(string(raw))
	if err != nil {
		return contract.Tool{}, fmt.Errorf("%s: %w", paths[0], err)
	}
	if t.Name == "" {
		return contract.Tool{}, fmt.Errorf("%s: 缺少 name", paths[0])
	}
	if want := strings.TrimSuffix(path.Base(paths[0]), ".tool.md"); t.Name != want {
		return contract.Tool{}, fmt.Errorf("%s: name=%q 与文件名不一致", paths[0], t.Name)
	}
	return t, nil
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
		res, err := b.eng.Call(ctx, "stop", withOwner(map[string]any{"context_id": id}, cx))
		if err != nil {
			return nil, err
		}
		res["context_id"] = id
		return res, nil
	}

	opArgs, _ := args["args"].(map[string]any)
	if opArgs == nil {
		opArgs = map[string]any{}
	}
	// 先做参数前置校验（eval 别名 / wait 上限 / screenshot 默认目录与沙箱）：
	// 非法参数不应先自动起一个 context。
	action, opArgs, err := prepareOperation(action, opArgs, cx, b.cfg.Settings())
	if err != nil {
		return nil, err
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
	params := withOwner(map[string]any{"context_id": id, "action": action, "args": opArgs}, cx)
	res, err := b.eng.Call(ctx, "operation", params)
	if err != nil {
		return nil, err
	}
	res["context_id"] = id
	return res, nil
}

// withOwner 把调用归属（实例 / 会话）随请求下发，供引擎做 context 越权校验。
func withOwner(params map[string]any, cx pluginsdk.CallContext) map[string]any {
	if cx.InstanceID != "" {
		params["instance_id"] = cx.InstanceID
	}
	if cx.Session != "" {
		params["session"] = cx.Session
	}
	return params
}

// prepareOperation 归一动作/参数别名并做前置校验（返回新 args，不修改调用方 map）。
//
//   - `script` 动作 = `eval`；eval 的参数键接受 script/eval/expr/js/code；
//   - wait.timeoutMs 限定 1..maxWaitTimeoutMs；
//   - screenshot 缺省路径取自配置，缺失则提前报错；落盘路径过沙箱白名单。
func prepareOperation(action string, args map[string]any, cx pluginsdk.CallContext, cfg settings) (string, map[string]any, error) {
	// 动作别名归一：别名也必须走同一套校验（尤其 shot→screenshot 的沙箱判定）。
	switch action {
	case "script", "js":
		action = "eval"
	case "goto", "open":
		action = "navigate"
	case "type":
		action = "fill"
	case "shot":
		action = "screenshot"
	}
	op := make(map[string]any, len(args))
	for k, v := range args {
		op[k] = v
	}
	switch action {
	case "eval":
		script := strings.TrimSpace(pluginsdk.Str(op["script"]))
		if script == "" {
			for _, key := range []string{"eval", "expr", "js", "code"} {
				if v := strings.TrimSpace(pluginsdk.Str(op[key])); v != "" {
					script = v
					break
				}
			}
		}
		if script != "" {
			op["script"] = script
		}
	case "wait":
		if raw, ok := op["timeoutMs"]; ok && raw != nil {
			n := pluginsdk.Int(raw)
			if n <= 0 || n > maxWaitTimeoutMs {
				return "", nil, fmt.Errorf("wait.timeoutMs 必须在 1..%d 之间（收到 %v）", maxWaitTimeoutMs, raw)
			}
		}
	case "screenshot":
		path := strings.TrimSpace(pluginsdk.Str(op["path"]))
		if path == "" {
			dir := pathx.ExpandHome(cfg.ScreenshotDir)
			if dir == "" {
				return "", nil, errors.New("screenshot 需要 path 参数，或先在插件配置中设置「截图默认目录」")
			}
			path = filepath.Join(dir, fmt.Sprintf("browser-%d.png", time.Now().UnixMilli()))
		}
		resolved, err := resolveScreenshotPath(path, cx)
		if err != nil {
			return "", nil, err
		}
		if err := checkScreenshotSandbox(cx, resolved); err != nil {
			return "", nil, err
		}
		op["path"] = resolved
	}
	return action, op, nil
}

// resolveScreenshotPath 展开 ~/；沙箱开启时把相对路径解析到调用工作目录，避免引擎 cwd 差异。
func resolveScreenshotPath(p string, cx pluginsdk.CallContext) (string, error) {
	p = pathx.ExpandHome(strings.TrimSpace(p))
	if p == "" {
		return "", errors.New("screenshot.path 不能为空")
	}
	if filepath.IsAbs(p) || cx.Sandbox == nil || !cx.Sandbox.Enabled {
		return p, nil
	}
	if wd := strings.TrimSpace(cx.WorkDir); wd != "" && filepath.IsAbs(wd) {
		return filepath.Join(wd, p), nil
	}
	return "", fmt.Errorf("sandbox: 截图路径必须是绝对路径或相对于工作目录（收到 %q）", p)
}

// checkScreenshotSandbox 校验截图落盘路径是否落在可写白名单内（递归）。
// 未启用沙箱（nil / enabled=false）→ 不限制；enabled=true 但没有可写目录 → 拒绝（fail-closed）；
// 越界 = 整次调用拒绝、不落盘。
func checkScreenshotSandbox(cx pluginsdk.CallContext, path string) error {
	if cx.Sandbox == nil || !cx.Sandbox.Enabled {
		return nil
	}
	dirs := make([]string, 0, len(cx.Sandbox.WritableDirs))
	for _, d := range cx.Sandbox.WritableDirs {
		if s := strings.TrimSpace(d); s != "" {
			dirs = append(dirs, s)
		}
	}
	if len(dirs) == 0 {
		return fmt.Errorf("sandbox: 沙箱已开启但未下发可写目录，拒绝落盘：%s", path)
	}
	policy := sandbox.Policy{Enabled: true, Writable: dirs}
	if policy.AllowsPath(path) {
		return nil
	}
	return fmt.Errorf("sandbox: 截图路径不在可写白名单内：%s（本次调用已拒绝，未落盘）", path)
}

func (b *browserPlugin) start(ctx context.Context, cx pluginsdk.CallContext, purpose string) (map[string]any, error) {
	cfg := b.cfg.Settings()
	if purpose == "" {
		purpose = "llm"
	}
	res, err := b.eng.Call(ctx, "start", withOwner(map[string]any{
		"purpose":  purpose,
		"headless": cfg.Headless,
		"debug":    cfg.Debug,
		"width":    cfg.Width,
		"height":   cfg.Height,
	}, cx))
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
