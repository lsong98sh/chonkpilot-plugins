// sandbox.go —— web_download 的写隔离校验（C6，2026-11-03）。
//
// 通道：gateway 每次 tools/call 在 `_meta.chonkpilot.sandbox` 下发可写白名单
// （C3）；未启用（无该字段 / enabled=false）→ 不校验（保持旧行为）。
package main

import (
	"fmt"
	"strings"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
)

// checkSandboxWritable 校验落盘路径是否落在可写白名单内（递归；读取不受限）。
func checkSandboxWritable(cx pluginsdk.CallContext, path string) error {
	if cx.Sandbox == nil || !cx.Sandbox.Enabled {
		return nil
	}
	policy := sandbox.Policy{Enabled: true, Writable: trimDirs(cx.Sandbox.WritableDirs)}
	if policy.AllowsPath(path) {
		return nil
	}
	return fmt.Errorf("sandbox: 落盘路径不在可写白名单内：%s（如需写入请在项目「安全/可写目录」中放行，或改用白名单内目录）", path)
}

// trimDirs 去掉空白项（保持顺序）。
func trimDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if s := strings.TrimSpace(d); s != "" {
			out = append(out, s)
		}
	}
	return out
}
