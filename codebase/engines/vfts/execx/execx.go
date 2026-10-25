// Package execx 是 vfts_query「一工具一 exe」的执行外壳。
//
// 契约（与其它 executor 一致）：
//
//	<exe> vfts_query --input=<params.json>
//	<exe> --help
//
// 输入 JSON 中 workdir 由插件注入，缺省回落 CHONKPILOT_WORKDIR。
package execx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	vf "github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts/server"
)

const toolName = "vfts_query"

// busyMessage 是索引库被其它进程占用、有界等待超时后的统一失败文本
// （2026-10-19 用户定稿：等 2-3 秒仍失败 → 工具失败「正在索引，不可用」）。
const busyMessage = "正在索引，不可用（索引库被另一进程占用，已等待约 3 秒，请稍后重试）"

// Run 执行 vfts_query，返回进程退出码（0 = 成功；1 = 工具失败；2 = 用法错误）。
func Run() int {
	args := os.Args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(toolName)
		return 0
	}
	if args[0] != toolName {
		fmt.Fprintf(os.Stderr, "unknown tool: %s (this executor only provides %s)\n", args[0], toolName)
		return 2
	}
	inputPath := ""
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--input=") {
			inputPath = strings.TrimPrefix(a, "--input=")
		}
	}
	if inputPath == "" {
		fmt.Fprintln(os.Stderr, "missing --input")
		return 2
	}
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		fmt.Fprint(os.Stdout, "read input: "+err.Error())
		return 1
	}
	params := map[string]any{}
	if len(raw) > 0 {
		raw = []byte(strings.TrimPrefix(string(raw), "\uFEFF"))
		if err := json.Unmarshal(raw, &params); err != nil {
			fmt.Fprint(os.Stdout, "parse input json: "+err.Error())
			return 1
		}
	}
	if params == nil {
		params = map[string]any{}
	}
	if _, ok := params["workdir"]; !ok {
		if wd := strings.TrimSpace(os.Getenv("CHONKPILOT_WORKDIR")); wd != "" {
			params["workdir"] = wd
		}
	}
	// zvec 集合为**排他 LOCK**：与插件管理调用 / resident indexer 的短窗口可能重叠 →
	// 有界重试（200ms × 15 ≈ 3s）；仍失败按「正在索引，不可用」结构化文本返回（可重试）。
	var out any
	busy := false
	for attempt := 0; attempt < 15; attempt++ {
		out, err = vf.ExecuteQuery(context.Background(), params)
		// 就绪门把「集合被占用」折叠成结构化应答（status=error + lock/timeout 文本）→ 同样可重试。
		busy = err == nil && busyResp(out)
		if err == nil && !busy {
			break
		}
		if err != nil && !isLockErr(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		if isLockErr(err) {
			fmt.Fprint(os.Stdout, busyMessage)
			return 1
		}
		fmt.Fprint(os.Stdout, err.Error())
		return 1
	}
	if busy {
		fmt.Fprint(os.Stdout, busyMessage)
		return 1
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprint(os.Stdout, "encode result: "+err.Error())
		return 1
	}
	fmt.Fprint(os.Stdout, string(b))
	return 0
}

// isLockErr 判定是否为 zvec 集合被占用（跨进程排他 LOCK）。
func isLockErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "lock") || strings.Contains(s, "timeout") ||
		strings.Contains(s, "being used by another process") || strings.Contains(s, "另一个进程")
}

// busyResp 判定结构化应答是否为「集合被占用」：readyGate 把 EnsureReady 的瞬时错误
// 折叠成 `{status:"error", message:"...lock..."}`（不再作为 Go error 上抛）。
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
