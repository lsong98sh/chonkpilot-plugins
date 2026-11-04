// sandbox.go —— task_run 的进程级写隔离（C5，2026-11-03）。
//
// 与 file/web 的「路径预检」不同：脚本可以在运行时自行计算路径，预检拦不住 →
// 走**真隔离**（Windows Low IL + Job Object，由 chonkpilot-util/sandbox 提供）。
//
// 白名单来源 = gateway 每次 tools/call 下发的 `_meta.chonkpilot.sandbox`
// （C3；未启用 / 无该字段 → 不隔离，保持旧行为）。执行前对白名单目录与 cwd 打 Low IL
// 标签（幂等、进程内缓存），否则 Low IL 子进程对它们也只能读不能写（fail-closed）。
package main

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
	"github.com/lsong98sh/chonkpilot-util/scriptrun"
)

// sandboxOptions 组装 scriptrun 的写隔离参数（Enabled 时 Writable = 已打标的目录）。
func sandboxOptions(cx pluginsdk.CallContext, cwd string) scriptrun.Sandbox {
	if cx.Sandbox == nil || !cx.Sandbox.Enabled {
		return scriptrun.Sandbox{}
	}
	dirs := append([]string{}, cx.Sandbox.WritableDirs...)
	if strings.TrimSpace(cwd) != "" {
		dirs = append(dirs, cwd)
	}
	writable := make([]string, 0, len(dirs))
	seen := map[string]bool{}
	for _, d := range dirs {
		p := strings.TrimSpace(d)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			continue
		}
		seen[key] = true
		if tagLowIL(p) {
			writable = append(writable, p)
		}
	}
	return scriptrun.Sandbox{Enabled: true, Writable: writable}
}

// tagLowIL 给目录打 Low IL 标签（幂等；进程内缓存成功结果）。
// 非 Windows（ErrUnsupportedPlatform）→ false 且不告警；无权限 → warn + false
// （该目录对沙箱子进程不可写 = fail-closed，脚本写它会被 OS 拒绝）。
//
// 缓存实现已上收 `chonkpilot-util/sandbox`（crontab 插件共用），本函数仅保留原名字做薄封装。
func tagLowIL(dir string) bool {
	err := sandbox.EnsureLowILCached(dir)
	if err == nil {
		return true
	}
	if !errors.Is(err, sandbox.ErrUnsupportedPlatform) {
		slog.Warn("task: 给目录打 Low IL 标签失败（该目录对沙箱脚本不可写）", "dir", dir, "err", err)
	}
	return false
}
