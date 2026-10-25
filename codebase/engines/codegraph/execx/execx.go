// Package execx 是 codegraph 查询工具「一工具一 exe」的公共执行外壳。
//
// 契约（与其它 executor 一致）：
//
//	<exe> <tool> --input=<params.json>   # 执行一次查询
//	<exe> --help                         # 打印本 exe 承载的工具名
//
// 工具名硬编码在各自 main（一工具一 exe）；输入 JSON 中 workdir 由插件注入，缺省回落
// CHONKPILOT_WORKDIR（宿主 spawn executor 时注入的调用上下文）。
package execx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	cg "github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph/server"
)

// busyMessage 是索引库被其它进程占用、有界等待超时后的统一失败文本
// （2026-10-19 用户定稿：等 2-3 秒仍失败 → 工具失败「正在索引，不可用」）。
const busyMessage = "正在索引，不可用（索引库被另一进程占用，已等待约 3 秒，请稍后重试）"

// Run 执行指定查询工具，返回进程退出码（0 = 成功；1 = 工具失败；2 = 用法错误）。
func Run(tool string) int {
	return run(tool, os.Args[1:], os.Stdout, os.Stderr, 3*time.Second)
}

// run 是 Run 的可测核心（stdout/stderr 与等待窗口可注入；LOCK 故障注入测试用，见 lock_test.go）。
func run(tool string, args []string, stdout, stderr io.Writer, wait time.Duration) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprintln(stdout, tool)
		return 0
	}
	name := args[0]
	if name != tool {
		fmt.Fprintf(stderr, "unknown tool: %s (this executor only provides %s)\n", name, tool)
		return 2
	}
	inputPath := ""
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--input=") {
			inputPath = strings.TrimPrefix(a, "--input=")
		}
	}
	if inputPath == "" {
		fmt.Fprintln(stderr, "missing --input")
		return 2
	}
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprint(stdout, "read input: "+err.Error())
		return 1
	}
	params := map[string]any{}
	if len(raw) > 0 {
		// 容忍 BOM（与 mcp-tools/cli 同口径）。
		raw = []byte(strings.TrimPrefix(string(raw), "\uFEFF"))
		if err := json.Unmarshal(raw, &params); err != nil {
			fmt.Fprint(stdout, "parse input json: "+err.Error())
			return 1
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	// workdir：插件注入优先；缺省回落宿主注入的调用上下文。
	if _, ok := params["workdir"]; !ok {
		if wd := strings.TrimSpace(os.Getenv("CHONKPILOT_WORKDIR")); wd != "" {
			params["workdir"] = wd
		}
	}
	// bbolt 索引库为**排他打开**：与插件管理调用 / resident indexer 的短窗口可能重叠 →
	// 有界重试（约 3s）；仍失败按「正在索引，不可用」结构化文本返回（可重试）。
	deadline := time.Now().Add(wait)
	var out any
	busy := false
	for {
		out, err = cg.ExecuteQuery(context.Background(), tool, params)
		// 就绪门把「库被占用」折叠成结构化应答（status=error + lock/timeout 文本）→ 同样视为可重试。
		busy = err == nil && busyResp(out)
		if err != nil {
			if !isLockErr(err) || !time.Now().Before(deadline) {
				break
			}
		} else if !busy || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		if isLockErr(err) {
			fmt.Fprint(stdout, busyMessage)
			return 1
		}
		fmt.Fprint(stdout, err.Error())
		return 1
	}
	if busy {
		fmt.Fprint(stdout, busyMessage)
		return 1
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprint(stdout, "encode result: "+err.Error())
		return 1
	}
	fmt.Fprint(stdout, string(b))
	return 0
}

// isLockErr 判定是否为索引库被其它进程占用（bbolt 打开超时 / Windows 共享冲突）。
func isLockErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "lock") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "being used by another process") || strings.Contains(s, "另一个进程")
}

// busyResp 判定结构化应答是否为「索引库被占用」：readyGate 把 EnsureReady 的瞬时错误
// 折叠成 `{status:"error", message:"...timeout..."}`（不再作为 Go error 上抛）。
func busyResp(out any) bool {
	if out == nil {
		return false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return false
	}
	var gate struct {
		Status  string `json:"status"`
		State   string `json:"state"`
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &gate) != nil {
		return false
	}
	if gate.Status != "error" && gate.State != "error" {
		return false
	}
	return isLockErr(errors.New(gate.Message))
}
