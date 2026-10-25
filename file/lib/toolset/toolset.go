// Package toolset 是文件域 11 个原子工具的进程内入口（GATEWAY-2 / L3）。
//
// 供内置 mcp-server 在契约 runtime == "builtin" 时直接调用，取代
// spawn chonkpilot-tools-executor.exe；命令行形态（cmd/tools-exec）复用同一实现，
// 二者输出口径一致（Text = executor stdout 原文，IsError = 退出码非 0）。
package toolset

import (
	"context"
	"fmt"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/filetool"
)

// Result 是一次工具调用的结果。
//   - Text：executor stdout 原文（JSON 或纯文本；由上层按同一封装口径判定）；
//   - IsError：是否失败（对齐 executor 非 0 退出码）。
type Result struct {
	Text    string
	IsError bool
}

// known 是文件域工具名集合（filetool 登记表快照，避免每次调用重复构建）。
var known = func() map[string]bool {
	m := make(map[string]bool)
	for _, n := range filetool.Names() {
		m[n] = true
	}
	return m
}()

// Dispatch 进程内执行一个文件域工具；未登记工具返回 (nil, false)。
//
// ctx 取消/超时 → 立即返回失败结果（底层文件操作为同步实现、无法中断，其结果随后丢弃）；
// panic 在此收口转为失败结果，不冒泡到 server 主循环。
func Dispatch(ctx context.Context, tool string, args map[string]any) (*Result, bool) {
	if !known[tool] {
		return nil, false
	}
	ch := make(chan *Result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- &Result{Text: fmt.Sprintf("tool %s panic: %v", tool, r), IsError: true}
			}
		}()
		res := filetool.Dispatch(tool, args)
		if res == nil {
			ch <- &Result{Text: "unknown tool: " + tool, IsError: true}
			return
		}
		text := res.Output
		if text == "" && res.Error != "" {
			text = res.Error
		}
		ch <- &Result{Text: text, IsError: !res.Success}
	}()
	select {
	case r := <-ch:
		return r, true
	case <-ctx.Done():
		return &Result{Text: ctx.Err().Error(), IsError: true}, true
	}
}
