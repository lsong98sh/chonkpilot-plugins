// sandbox.go —— 写隔离校验（C4，2026-11-03）。
//
// 通道：gateway 每次 tools/call 在 `_meta.chonkpilot.sandbox` 下发可写白名单
// （C3；插件 MCP 是共享 http 长连接，header 装不下按项目变化的白名单）。
// 判定：写工具的每个落盘路径都必须落在白名单内（递归；读取不受限）；
// 未启用（无该字段 / enabled=false）→ 不校验（保持旧行为）。
//
// 越界 = **整次调用拒绝**（调用前预检，未做任何写入）：路径校验在分派前完成，
// 命中即返回明确原因（含越界路径），避免「部分写入后才发现越界」。
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
)

// writeTools 是**需要写白名单校验**的 7 个写工具（C4）；只读工具（read/find/diff/detect）不校验。
var writeTools = map[string]bool{
	"file_write":   true,
	"file_patch":   true,
	"file_replace": true,
	"file_copy":    true,
	"file_move":    true,
	"file_delete":  true,
	"file_iconv":   true,
}

// fromToTools 的 `from` 是删除（源）语义，需与 `to` 一并判定；其余写工具只判 `path`。
var fromToTools = map[string]bool{"file_copy": true, "file_move": true}

// checkSandboxWritable 返回首个越界项的失败消息（"" = 通过 / 未启用沙箱）。
func checkSandboxWritable(ctx context.Context, tool string, args map[string]any) string {
	if !writeTools[tool] {
		return ""
	}
	policy := sandboxPolicyFromMeta(ctx)
	if !policy.Enabled {
		return ""
	}
	for _, raw := range writableTargets(tool, args) {
		if raw == "" {
			continue
		}
		resolved, msg := paths.ResolvePath(raw, "")
		if msg != "" || resolved == "" {
			continue // 交由 R-11 路径校验统一报错（不重复判定）
		}
		if !policy.AllowsPath(resolved) {
			return fmt.Sprintf("sandbox: 目标路径不在可写白名单内：%s（本次调用已拒绝、未做任何写入；如需写入请在项目「安全/可写目录」中放行，读取不受限）", raw)
		}
	}
	return ""
}

// writableTargets 提取该工具本次调用涉及的全部落盘路径（原样，未解析）。
func writableTargets(tool string, args map[string]any) []string {
	targets, _ := args["targets"].([]any)
	out := make([]string, 0, len(targets)*2)
	for _, it := range targets {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if p, _ := m["path"].(string); p != "" {
			out = append(out, p)
		}
		if fromToTools[tool] {
			if p, _ := m["from"].(string); p != "" {
				out = append(out, p)
			}
			if p, _ := m["to"].(string); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// sandboxPolicyFromMeta 从 `_meta.chonkpilot.sandbox` 解析可写白名单（缺失 = 未启用）。
func sandboxPolicyFromMeta(ctx context.Context) sandbox.Policy {
	meta := contract.Meta(ctx)
	if meta == nil {
		return sandbox.Policy{}
	}
	ns, _ := meta["chonkpilot"].(map[string]any)
	raw, _ := ns["sandbox"].(map[string]any)
	if raw == nil {
		return sandbox.Policy{}
	}
	enabled, _ := raw["enabled"].(bool)
	if !enabled {
		return sandbox.Policy{}
	}
	var dirs []string
	switch v := raw["writable_dirs"].(type) {
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
				dirs = append(dirs, s)
			}
		}
	case []string:
		dirs = append(dirs, v...)
	}
	return sandbox.Policy{Enabled: true, Writable: dirs}
}
