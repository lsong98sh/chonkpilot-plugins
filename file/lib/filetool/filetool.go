// Package filetool 是文件域 11 个原子工具的实现（GATEWAY-2 / L2）。
//
// 参数模型统一为 {purpose, targets[]}：单文件也是长度 1 的数组。
// 与旧 filesys_run（DSL 动作流）的区别：
//   - 无流控、无句柄、无脚本解析，一次调用只做一类动作；
//   - 参数级违规 = 整体失败；运行期错误 = 逐项 failed；
//   - 结果恒为结构化 JSON（purpose / results[] / summary），由 mcp-server 透传。
//
// 路径规则复用 fileops.ValidateFilePath（绝对路径 / ~/ / !/），相对路径一律拒绝。
package filetool

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
)

// maxTargets 是 targets 数组的硬上限（与 file_find.limit 同口径）。
const maxTargets = 2000

const (
	statusOK      = "ok"
	statusFailed  = "failed"
	statusSkipped = "skipped"
)

// tools 是工具名 → 处理函数。
var tools = map[string]func(tool string, args map[string]any) *cli.Result{
	"file_read":    runRead,
	"file_write":   runWrite,
	"file_replace": runReplace,
	"file_patch":   runPatch,
	"file_diff":    runDiff,
	"file_delete":  runDelete,
	"file_move":    runMove,
	"file_copy":    runCopy,
	"file_find":    runFind,
	"file_detect":  runDetect,
	"file_iconv":   runIconv,
}

// Names 返回本包承载的全部工具名（构建/自检用）。
func Names() []string {
	out := make([]string, 0, len(tools))
	for k := range tools {
		out = append(out, k)
	}
	return out
}

// Dispatch 分发一次工具调用；未知工具返回 nil（由 cli 报 unknown tool）。
func Dispatch(tool string, args map[string]any) *cli.Result {
	fn, ok := tools[tool]
	if !ok {
		return nil
	}
	return fn(tool, args)
}

// request 是解析后的统一入参。
type request struct {
	purpose string
	targets []map[string]any
}

// parseRequest 校验 purpose / targets 的形状（参数级违规 → 整体失败）。
func parseRequest(args map[string]any) (*request, string) {
	purpose, _ := args["purpose"].(string)
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return nil, "purpose is required"
	}
	raw, ok := args["targets"]
	if !ok {
		return nil, "targets is required"
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, "targets must be an array"
	}
	if len(arr) == 0 || len(arr) > maxTargets {
		return nil, fmt.Sprintf("targets length must be 1..%d", maxTargets)
	}
	out := make([]map[string]any, 0, len(arr))
	for i, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("targets[%d] must be an object", i)
		}
		out = append(out, m)
	}
	return &request{purpose: purpose, targets: out}, ""
}

// finish 汇总逐项结果并输出结构化 JSON。部分失败仍返回完整结果（不吞掉成功项）。
func finish(tool, purpose string, results []map[string]any) *cli.Result {
	okN, failN, skipN := 0, 0, 0
	for _, r := range results {
		switch r["status"] {
		case statusOK:
			okN++
		case statusFailed:
			failN++
		case statusSkipped:
			skipN++
		}
	}
	env := map[string]any{
		"purpose": purpose,
		"results": results,
		"summary": map[string]any{
			"total":   len(results),
			"ok":      okN,
			"failed":  failN,
			"skipped": skipN,
		},
	}
	b, err := json.Marshal(env)
	if err != nil {
		return cli.Err(tool, "marshal result: "+err.Error())
	}
	// 部分失败：退出码置 1（让上层知道有问题），但 stdout 仍是完整 JSON。
	if failN > 0 {
		return &cli.Result{Tool: tool, Success: false, Output: string(b), Error: fmt.Sprintf("%d target(s) failed", failN)}
	}
	return cli.Ok(tool, string(b), nil)
}

// paramErr 构造参数级整体失败。
func paramErr(tool, msg string) *cli.Result { return cli.Err(tool, msg) }

// resolvePath 解析并强校验路径（R-11）。
func resolvePath(raw, label string) (string, string) {
	if strings.TrimSpace(raw) == "" {
		return "", label + ": path is required"
	}
	abs, msg := fileops.ValidateFilePath(raw)
	if msg != "" {
		return "", label + ": " + msg
	}
	if abs == "" {
		return "", label + ": invalid path"
	}
	return abs, ""
}

// md5File 流式计算文件 md5（不存在/不可读 → error）。
func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAtomic 原子写：同目录临时文件 + rename（失败不留半截文件）。
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ck-tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// itemErr 构造单项失败结果。
func itemErr(fields map[string]any, err string) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["status"] = statusFailed
	fields["error"] = err
	return fields
}

// itemSkip 构造单项跳过结果。
func itemSkip(fields map[string]any, reason string) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["status"] = statusSkipped
	fields["reason"] = reason
	return fields
}

// itemOK 构造单项成功结果。
func itemOK(fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["status"] = statusOK
	return fields
}

// asString 取字符串参数（缺省 ""）。
func asString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// asInt 取整数参数（缺省 def）。
func asInt(m map[string]any, key string, def int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

// asBool 取布尔参数（缺省 def）。
func asBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

// asStringSlice 取字符串数组参数。
func asStringSlice(m map[string]any, key string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		if s, ok := it.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
