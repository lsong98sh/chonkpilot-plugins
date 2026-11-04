// workdir.go —— 定时任务执行前的「项目已打开」保证（SANDBOX-WRITE-1 P3 / 方案 §5.5.2 ②）。
//
// 与 P1/P2 的沙箱链路互补：`sandbox.resolve` 只能给出**已登记实例**的策略（未命中 → 仅内置
// 默认项，G-96）。P3 起执行前先经宿主桥 `plugin/v1/workdir.ensure{work_dir, hidden:true}` 要求
// 宿主**隐藏打开**该项目（C13/C17：正常打开流程、窗口不显示）→ 之后 `sandbox.resolve` 必然命中
// 在册实例，用户配置的 prj `security-*` 随之生效。
//
// 失败语义（C13）：**本轮跳过 + 记错，不 disable**（与 P2 的 `work_dir_missing` 区分——目录存在
// 但打不开是可重试的瞬态问题，下一轮仍会尝试）。
package main

import (
	"context"
	"errors"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// 宿主桥方法（UDS 方法面；非 MQ 主题）。
const methodWorkdirEnsure = "plugin/v1/workdir.ensure"

// ensureWorkdir 请求宿主确保该 work_dir 的项目已打开（未打开 → 隐藏打开），返回主实例 id。
func ensureWorkdir(ctx context.Context, p *pluginsdk.Plugin, workDir string) (string, error) {
	if p == nil {
		return "", errors.New("插件未连接宿主桥")
	}
	var out struct {
		InstanceID string `json:"instance_id"`
		WorkDir    string `json:"work_dir"`
		Opened     bool   `json:"opened"`
	}
	if err := p.CallInto(ctx, methodWorkdirEnsure,
		map[string]any{"work_dir": workDir, "hidden": true}, &out); err != nil {
		return "", err
	}
	if out.InstanceID == "" {
		return "", errors.New("宿主未返回实例 id")
	}
	return out.InstanceID, nil
}
