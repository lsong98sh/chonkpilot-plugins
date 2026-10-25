// Package cli 是文件域工具 handler 的最小结果契约（插件自持实现）。
//
// 原主仓 `mcp-tools/internal/cli` 同时承载 executor 的 CLI 运行框架（--help / 退出码 /
// stdout 封装）；插件形态不需要这些，故此处只保留 handler 产出与封装所需的三件：
// Result / Ok / Err。
package cli

// Result 是工具执行的中间产物（handler 产出，由 toolset 封装为最终文本）。
type Result struct {
	Success   bool
	Output    string
	Error     string
	Tool      string
	RawResult interface{}
}

// Ok 构造成功结果。
func Ok(tool, output string, raw interface{}) *Result {
	return &Result{Success: true, Output: output, Tool: tool, RawResult: raw}
}

// Err 构造失败结果。
func Err(tool, msg string) *Result {
	return &Result{Success: false, Error: msg, Tool: tool, Output: msg}
}
