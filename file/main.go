// chonkpilot-file —— 文件工具插件（file 系列 11 个工具）。
//
// 契约源 = 本仓 `file/tools/file/*.tool.md`（构建期拷到 `<pluginRoot>/tools/file/`）。
// 本插件写法：
//   - 契约解析/注册/参数封装 → framework `contract`（复用层）
//   - 默认参数（purpose / encoding / targets 上限）→ 本文件
//   - 执行 → 同仓 `file/lib/toolset`（工具实现随插件，2026-10-25 PLUGIN-SPLIT-1）
//
// 主仓不再持有 file 工具实现：`file_*` 由 gateway 调用本插件的 MCP server 执行。
package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/toolset"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const pluginVersion = "1.0.0"

type filePlugin struct {
	p     *pluginsdk.Plugin
	cfg   *configManager
	root  string
	tools []contract.Tool
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID: "file", Name: "文件工具", Version: pluginVersion,
		Features: map[string]any{"mcp": "http", "ui": true},
	})
	if err != nil {
		slog.Error("file: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	f := &filePlugin{p: p, cfg: newConfigManager(p), root: os.Getenv(pluginsdk.EnvRoot)}
	tools, err := contract.LoadDir(filepath.Join(f.root, "tools", "file"))
	if err != nil {
		slog.Error("file: 加载契约失败", "err", err)
		os.Exit(1)
	}
	if len(tools) == 0 {
		slog.Error("file: 未找到任何契约（<root>/tools/file/*.tool.md）", "root", f.root)
		os.Exit(1)
	}
	f.tools = tools

	p.OnMCPServer(func(srv *mcp.Server) error {
		return contract.Register(srv, tools, f.call)
	})
	for _, name := range contract.HotNames(tools) {
		p.MCPHot(name)
	}

	p.OnRequest("status", pluginsdk.StatusHandler(pluginVersion, f.status))
	p.OnRequest("tools", f.toolNames)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/file/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := f.cfg.Init(context.Background()); err != nil {
		slog.Warn("file: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range f.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("file: 运行结束", "err", err)
	}
}

// call 是业务分派：补默认参数 → toolset 进程内执行。
func (f *filePlugin) call(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	f.applyDefaults(args)
	text, isErr := dispatch(ctx, name, args)
	return text, isErr, nil
}

// dispatch 执行一次工具调用（默认参数由调用方补齐）：_meta 落地根 + toolset 分派。
//
// 抽成包级函数是为了让进程内 MCP 面测试（tools_mcp_test.go）复用同一条分派链，不重复实现。
func dispatch(ctx context.Context, name string, args map[string]any) (string, bool) {
	applyCallMeta(ctx)
	res, ok := toolset.Dispatch(ctx, name, args)
	if !ok {
		return "未知工具: " + name, true
	}
	return res.Text, res.IsError
}

// applyCallMeta 用协议 _meta（`chonkpilot.instance_id`）初始化调用上下文：
// `!/` 前缀的落地根 = <temp>/chonkpilot/<instance>/（与主仓 builtin 调用口径一致）。
func applyCallMeta(ctx context.Context) {
	meta := contract.Meta(ctx)
	if meta == nil {
		return
	}
	ns, _ := meta["chonkpilot"].(map[string]any)
	inst, _ := ns["instance_id"].(string)
	if inst = strings.TrimSpace(inst); inst == "" {
		return
	}
	_, _ = paths.SetTempRoot(inst)
}

// applyDefaults 用插件配置补默认参数（显式入参优先；LLM 可逐次覆盖）。
func (f *filePlugin) applyDefaults(args map[string]any) {
	cfg := f.cfg.Settings()
	pluginsdk.MergeMissing(args, map[string]any{
		"purpose": cfg.DefaultPurpose,
	})
	if cfg.DefaultEncoding != "" {
		pluginsdk.MergeMissing(args, map[string]any{"encoding": cfg.DefaultEncoding})
	}
	if maxTargets := cfg.MaxTargets; maxTargets > 0 {
		if targets, ok := args["targets"].([]any); ok && len(targets) > maxTargets {
			args["targets"] = targets[:maxTargets]
		}
	}
}

func (f *filePlugin) status(_ context.Context) map[string]any {
	cfg := f.cfg.Settings()
	return map[string]any{
		"tools":           len(f.tools),
		"root":            filepath.Join(f.root, "tools", "file"),
		"defaultPurpose":  cfg.DefaultPurpose,
		"defaultEncoding": cfg.DefaultEncoding,
		"maxTargets":      cfg.MaxTargets,
	}
}

func (f *filePlugin) toolNames(_ context.Context, _ map[string]any) (any, error) {
	names := make([]string, 0, len(f.tools))
	for _, t := range f.tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return map[string]any{"tools": names, "root": filepath.Join(f.root, "tools", "file")}, nil
}
