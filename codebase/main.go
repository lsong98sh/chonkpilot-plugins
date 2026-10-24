// chonkpilot-codebase —— 代码知识库插件（codegraph + vfts 合二为一）。
//
// 形态：manifest 插件 + 进程内 Streamable HTTP MCP。
//   - codegraph：符号 / 调用关系 / 依赖图 / 复杂度（tree-sitter 索引）；
//   - vfts：全文检索（zvec 向量 + 中文分词）。
//
// 两者共用同一个 exe 与同一份配置页（webui/config.html），对外工具名保持不变
// （codegraph_* / vfts_*），由 gateway 以 namespace="-" 原样暴露给 LLM。
//
// 依赖：同目录需带 zvec_c_api.dll（vfts 引擎）。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	codegraph "github.com/chonkpilot/chonkpilot-codegraph-mcp-server/server"
	vfts "github.com/chonkpilot/chonkpilot-vfts-mcp-server/server"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const pluginVersion = "1.0.0"

// hotTools 是注入 LLM 的工具（与管理工具 codegraph_configure / codegraph_status 区分）。
var hotTools = []string{
	"codegraph_symbol_search",
	"codegraph_get_symbol_info",
	"codegraph_callers",
	"codegraph_callees",
	"codegraph_get_dependency_graph",
	"codegraph_find_circular_deps",
	"codegraph_analyze_complexity",
	"codegraph_get_module_summary",
	"vfts_query",
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "codebase",
		Name:    "代码知识库",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp":   "http",
			"ui":    true,
			"tools": hotTools,
		},
	})
	if err != nil {
		slog.Error("codebase: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	p.OnMCPServer(func(srv *mcp.Server) error {
		if err := codegraph.RegisterTools(srv); err != nil {
			return fmt.Errorf("codegraph 注册失败: %w", err)
		}
		if err := vfts.RegisterTools(srv); err != nil {
			return fmt.Errorf("vfts 注册失败: %w", err)
		}
		return nil
	})
	p.MCPHot(hotTools...)

	p.OnRequest("ping", func(_ context.Context, _ map[string]any) (any, error) {
		return map[string]any{"ok": true, "version": pluginVersion}, nil
	})
	p.OnRequest("status", func(_ context.Context, _ map[string]any) (any, error) {
		return map[string]any{
			"ok":      true,
			"version": pluginVersion,
			"engines": []string{"codegraph", "vfts"},
			"tools":   hotTools,
		}, nil
	})
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/codebase/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)
	// UI 动作面（PLUGIN-UI-1 第二段）：宿主 `POST /plugins/codebase/api/<action>`
	// 经通用 RPC 信封（`plugin.ui.rpc`，method = action）透传到本表。
	// 本段只提供最小探活动作（`ui.ping`）；具体业务动作在下一段实现，
	// 未注册的 action 由 SDK 回 `method_not_found`（宿主映射 501）。
	p.OnRequest("ui.ping", func(_ context.Context, _ map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	// 配置页直调引擎管理工具（codegraph_status / codegraph_initialize / vfts_status / vfts_index …）。
	// 注意：配置写入口**不在此**——配置统一走 `load` / `save` 动作（prj 键为唯一源，见 config.go）。
	p.OnRequest("tool.call", func(ctx context.Context, payload map[string]any) (any, error) {
		name, _ := payload["name"].(string)
		args, _ := payload["args"].(map[string]any)
		if name == "" {
			return nil, fmt.Errorf("tool.call: name 必填")
		}
		if args == nil {
			args = map[string]any{}
		}
		return callTool(ctx, name, args)
	})

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` 动作 + 一次性键迁移 + 配置变化下发引擎。
	cfg := newConfigManager(p)
	for name, fn := range cfg.Actions() {
		p.OnRequest(name, fn)
	}
	// 打开项目（instance-register）→ 迁移 + 按配置下发引擎；退出清理；prj 配置刷新 → 热生效。
	p.Subscribe("instance-register", func(_ context.Context, _ string, payload []byte) {
		cfg.onInstanceRegister(payload)
	})
	p.Subscribe("instance-exit", func(_ context.Context, _ string, payload []byte) {
		cfg.onInstanceExit(payload)
	})
	p.Subscribe("data-prj-config-refresh", func(_ context.Context, _ string, payload []byte) {
		cfg.onPrjConfigRefresh(payload)
	})
	// 宿主就绪（含晚连接补发）→ 对已登记实例补跑一次迁移（此前错过的实例不丢）。
	p.Subscribe("server-ready", func(_ context.Context, _ string, _ []byte) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cfg.migrateAll(ctx)
		}()
	})

	if err := p.Run(context.Background()); err != nil {
		slog.Error("codebase: 运行结束", "err", err)
	}
}

// callTool 按前缀把调用分派到 codegraph / vfts 引擎（进程内直调，不经过 MCP 传输）。
func callTool(ctx context.Context, name string, args map[string]any) (any, error) {
	switch {
	case strings.HasPrefix(name, "codegraph_"):
		return codegraph.Call(ctx, name, args)
	case strings.HasPrefix(name, "vfts_"):
		return vfts.Call(ctx, name, args)
	default:
		return nil, fmt.Errorf("tool.call: 未知工具 %s", name)
	}
}
