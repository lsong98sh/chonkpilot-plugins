// 查询工具的一工具一 exe 执行面（2026-10-19，G-68 第二步）。
//
// 插件（plugin-vfts）仍按启用状态把 vfts_query 注册给 gateway；执行委托 core-mcp-server：
// 每次调用 spawn 一个单工具 executor exe，exe 以只读方式打开落盘 zvec 集合执行检索
// （与引擎的 vfts_query 同一实现）。取消只杀本次 executor。
package server

import "context"

// ExecuteQuery 执行一次 vfts_query（args 必须含 workdir；索引就绪门/结构化 pending 与
// 引擎的 MCP 工具实现一致）。
func ExecuteQuery(ctx context.Context, args map[string]any) (any, error) {
	return toolQuery(ctx, args)
}
