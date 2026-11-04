// sandbox.go —— 定时任务 `run` 的**进程级写隔离**（SANDBOX-WRITE-1 P2 / 方案 §5.5.2）。
//
// 与 P1 的 task 插件不同：crontab 执行时没有 `tools/call` 上下文，拿不到 gateway 下发的
// `_meta.chonkpilot.sandbox`，因此执行前经宿主桥 `plugin/v1/sandbox.resolve{work_dir}`
// 拉取可写白名单，再对白名单目录打 Low IL 标签（幂等、进程内缓存），最后交由
// `scriptrun.Options.Sandbox` 以 Low IL + Job 启动子进程。
//
// 失败语义（C5 / P2）：**拿不到策略 = fail-open**（不隔离执行），但必须留痕
// （`RunRecord.Detail` 写明原因）；打标失败的目录不进入 Writable（fail-closed）。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/sandbox"
	"github.com/lsong98sh/chonkpilot-util/scriptrun"
)

// 宿主桥方法（UDS 方法面；非 MQ 主题）。
const methodSandboxResolve = "plugin/v1/sandbox.resolve"

// projectDataDirName 是项目数据根目录名（与宿主 `lockfile.ProjectDataDirName` 同名）。
const projectDataDirName = ".chonkpilot"

// resolveSandbox 经宿主能力面取该 work_dir 的可写白名单。
func resolveSandbox(ctx context.Context, p *pluginsdk.Plugin, workDir string) ([]string, error) {
	if p == nil {
		return nil, errors.New("插件未连接宿主桥")
	}
	var out struct {
		WritableDirs []string `json:"writable_dirs"`
	}
	if err := p.CallInto(ctx, methodSandboxResolve, map[string]any{"work_dir": workDir}, &out); err != nil {
		return nil, err
	}
	return out.WritableDirs, nil
}

// effectiveSandbox 返回任务实际生效的沙箱开关（任务级 > 插件配置）。
func effectiveSandbox(t Task, cfg settings) bool {
	if t.Sandbox != nil {
		return *t.Sandbox
	}
	return cfg.Sandbox
}

// sandboxDecision 是一次执行的沙箱决定。
type sandboxDecision struct {
	Enabled  bool
	Writable []string
	// Reason 非空 = 本次**未隔离**或部分目录未能打标的原因（写入 RunRecord.Detail）。
	Reason string
}

// sandboxFor 组装本次执行的沙箱参数（Enabled=true 时才向宿主拉策略）。
func (r *runner) sandboxFor(ctx context.Context, t Task) sandboxDecision {
	if !effectiveSandbox(t, r.cfg.Settings()) {
		return sandboxDecision{}
	}
	if r.resolveSandbox == nil {
		return sandboxDecision{Reason: "sandbox_unavailable: 宿主未提供 " + methodSandboxResolve}
	}
	wd := strings.TrimSpace(t.CreatedBy.WorkDir)
	dirs, err := r.resolveSandbox(ctx, wd)
	if err != nil {
		return sandboxDecision{Reason: "sandbox_unavailable: " + err.Error()}
	}

	candidates := append([]string{}, dirs...)
	if wd != "" {
		// 与 gateway 同口径：内置默认可写项 `<work_dir>/.chonkpilot/tmp` 按需创建
		// （用户配置的白名单目录不代建，缺失时只记录打标失败）。
		_ = os.MkdirAll(filepath.Join(wd, projectDataDirName, "tmp"), 0o755)
		candidates = append(candidates, wd)
	}
	writable := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	var failed []string
	for _, d := range candidates {
		p := strings.TrimSpace(d)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			continue
		}
		seen[key] = true
		if e := sandbox.EnsureLowILCached(p); e != nil {
			if errors.Is(e, sandbox.ErrUnsupportedPlatform) {
				return sandboxDecision{Reason: "sandbox_unsupported_platform"}
			}
			failed = append(failed, p)
			continue
		}
		writable = append(writable, p)
	}
	d := sandboxDecision{Enabled: true, Writable: writable}
	if len(failed) > 0 {
		d.Reason = "sandbox_lowil_failed: 无法打标（该目录不可写）: " + strings.Join(failed, "; ")
	}
	return d
}

// scriptSandbox 转为 scriptrun 的沙箱参数。
func (d sandboxDecision) scriptSandbox() scriptrun.Sandbox {
	return scriptrun.Sandbox{Enabled: d.Enabled, Writable: d.Writable}
}

// appendDetail 把 extra 追加到已有 detail（分号分隔；避免互相覆盖）。
func appendDetail(existing, extra string) string {
	a := strings.TrimSpace(existing)
	b := strings.TrimSpace(extra)
	switch {
	case b == "":
		return a
	case a == "":
		return b
	default:
		return fmt.Sprintf("%s; %s", a, b)
	}
}
