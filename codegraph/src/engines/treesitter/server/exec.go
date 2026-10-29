// 查询工具的一工具一 exe 执行面（2026-10-19，G-68 第二步）。
//
// 插件（plugin-codegraph）仍按启用状态把 8 个查询工具注册给 gateway；执行不再由插件常驻
// 引擎承接，而是委托 core-mcp-server：每次调用 spawn 一个**单工具 executor exe**，
// 该 exe 直接以只读方式打开落盘 bbolt 索引执行查询（与引擎查询同一实现）。
// 好处：取消只杀本次 executor，不影响其它并发调用；也不依赖插件进程内的引擎会话。
package server

import (
	"context"
	"fmt"
)

// QueryToolNames 返回对外查询工具名（一工具一 exe 的源清单；顺序稳定）。
func QueryToolNames() []string {
	out := make([]string, 0, 8)
	for _, s := range toolSpecs {
		if s.Hot {
			out = append(out, s.Name)
		}
	}
	return out
}

// ExecuteQuery 按工具名执行一次查询（args 必须含 workdir；索引就绪门/结构化 pending 与
// 引擎的 MCP 工具实现完全一致）。未知工具 → error。
func ExecuteQuery(ctx context.Context, name string, args map[string]any) (any, error) {
	switch name {
	case "codegraph_symbol_search":
		return toolSymbolSearch(ctx, args)
	case "codegraph_get_symbol_info":
		return toolGetSymbolInfo(ctx, args)
	case "codegraph_callers":
		return toolCallers(ctx, args)
	case "codegraph_callees":
		return toolCallees(ctx, args)
	case "codegraph_get_dependency_graph":
		return toolDependencyGraph(ctx, args)
	case "codegraph_find_circular_deps":
		return toolCircularDeps(ctx, args)
	case "codegraph_analyze_complexity":
		return toolComplexity(ctx, args)
	case "codegraph_get_module_summary":
		return toolModuleSummary(ctx, args)
	}
	return nil, fmt.Errorf("unknown codegraph query tool: %s", name)
}
