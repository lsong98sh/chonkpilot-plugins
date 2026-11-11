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
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/pathutil"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
	"github.com/lsong98sh/chonkpilot-util/scriptrun"
)

// sandboxOptions 组装 scriptrun 的写隔离参数（Enabled 时 Writable = 已打标的目录）。
//
// 2026-11-08（P0）：`cwd` **不再**追加进可写白名单 —— 它只是执行目录，写权限仅来自
// gateway 下发的 `_meta.chonkpilot.sandbox.writable_dirs`。否则 LLM 传任意绝对目录即可
// 让沙箱脚本在其内写文件（写隔离绕过）。
func sandboxOptions(cx pluginsdk.CallContext) scriptrun.Sandbox {
	if cx.Sandbox == nil || !cx.Sandbox.Enabled {
		return scriptrun.Sandbox{}
	}
	dirs := append([]string{}, cx.Sandbox.WritableDirs...)
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

// resolveCwd 归一 cwd 并限制在会话 work_dir 子树内（越界拒绝）。
// 返回空串 = 不设置子进程 Dir（沿用插件进程 cwd）。
//
// 2026-11-08（P0）：任意绝对 cwd 不再被接受 —— cwd 只作为执行目录，且必须落在
// `_meta.chonkpilot.work_dir`（或项目工作目录）内；缺少 work_dir 时 fail-closed。
func resolveCwd(cwd, workDir, instanceID string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", nil
	}
	resolved, msg := pathutil.Resolve(cwd, instanceID)
	if msg != "" {
		return "", errors.New("cwd：" + msg)
	}
	wd := strings.TrimSpace(workDir)
	if wd == "" {
		return "", errors.New("cwd 需要会话工作目录（work_dir）作为边界；缺少 work_dir，拒绝在任意目录执行")
	}
	wdResolved, msg := pathutil.Resolve(wd, instanceID)
	if msg != "" {
		return "", errors.New("work_dir：" + msg)
	}
	if !withinDir(wdResolved, resolved) {
		return "", fmt.Errorf("cwd 越界：%s 不在会话工作目录 %s 内（只允许 work_dir 子树）", resolved, wdResolved)
	}
	return resolved, nil
}

// withinDir 判定 target 是否在 base 目录内（含相等；Windows 大小写不敏感）。
func withinDir(base, target string) bool {
	b := filepath.Clean(base)
	t := filepath.Clean(target)
	if runtime.GOOS == "windows" {
		b, t = strings.ToLower(b), strings.ToLower(t)
	}
	if b == t {
		return true
	}
	rel, err := filepath.Rel(b, t)
	if err != nil || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
