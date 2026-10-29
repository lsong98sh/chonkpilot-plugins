// Package pathutil 是 task 插件自持的**最小**路径/环境工具（不跨插件依赖 file 插件的 lib）。
//
// 来源：原主仓 `mcp-tools/internal/fileops` 的 ValidateFilePath / HostEnv 语义（决策 R-11、
// R-11 二次升级），随 `task_run` 实现迁入本插件（2026-10-28 P2-3）。只保留 task_run 需要的部分：
//
//   - Resolve：工具参数路径强校验（绝对路径 / `~/` 用户目录 / `!/` 临时目录；相对路径拒绝）；
//   - HostEnv：读取宿主注入的 CHONKPILOT_*（供子进程环境透传）；
//   - EnvInterpreters：解释器映射环境变量名（迁移期兼容）。
package pathutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/lsong98sh/chonkpilot-util/pathx"
)

// 宿主注入 env 的变量名（对齐 docs/spec/10-architecture/16-路径解析规范 §8）。
const (
	EnvInstance     = "CHONKPILOT_INSTANCE"
	EnvWorkDir      = "CHONKPILOT_WORKDIR"
	EnvDataDir      = "CHONKPILOT_DATADIR"
	EnvPurpose      = "CHONKPILOT_PURPOSE"
	EnvTempDir      = "CHONKPILOT_TEMPDIR"
	EnvExeDir       = "CHONKPILOT_EXEDIR"
	EnvProject      = "CHONKPILOT_PROJECT"
	EnvInterpreters = "CHONKPILOT_INTERPRETERS" // runtime → 解释器绝对路径（JSON 对象；迁移期兼容）
)

// ErrNoInstance 表示缺少 instance：无法解析 `!/` 临时目录。
var ErrNoInstance = errors.New("缺少 instance（宿主未注入调用上下文），无法解析 !/ 临时目录")

// Resolve 解析工具参数路径（R-11 严格语义）：
//
//  1. raw == ""                  → ("", "")（空值交给调用方做必填校验）
//  2. `!/` 或 `!\` 前缀           → <temp>/chonkpilot/<sanitize(instance)>/...（`..` 越界拒绝）
//  3. `~` / `~/x` / `~\x`         → 展开用户 home（`~foo` 不展开，按字面处理）
//  4. filepath.IsAbs             → filepath.Clean 原样
//  5. 其余（相对路径）            → ("", 统一错误消息)
//
// 返回 (解析后的绝对/展开路径, 错误消息)；错误消息非空表示不合规。
func Resolve(raw, instanceID string) (string, string) {
	if raw == "" {
		return "", ""
	}
	if rest, ok := trimTempPrefix(raw); ok {
		root, err := TempRoot(instanceID)
		if err != nil {
			return "", err.Error()
		}
		p, ok := joinTempRoot(root, rest)
		if !ok {
			return "", InvalidMessage(raw)
		}
		return p, ""
	}
	s := expandHome(raw)
	if filepath.IsAbs(s) {
		return filepath.Clean(s), ""
	}
	return "", InvalidMessage(raw)
}

// InvalidMessage 构造路径不合规的统一错误消息（含原值与允许的三种写法）。
func InvalidMessage(raw string) string {
	return "路径必须是绝对路径、以 ~/ 开头的用户目录、或以 !/ 开头的临时目录：" + raw +
		`（例：C:\work\proj\src\main.py、~/data/x.txt、!/tmp.csv）`
}

// TempRoot 返回指定 instance 的临时目录根：<系统 temp>/chonkpilot/<sanitize(instance)>。
// 目录按需创建（创建失败不阻断，返回路径仍可用）；instance 为空/平凡 → ("", ErrNoInstance)。
func TempRoot(instanceID string) (string, error) {
	id := sanitizeInstance(instanceID)
	if id == "" {
		return "", ErrNoInstance
	}
	root := filepath.Join(os.TempDir(), "chonkpilot", id)
	_ = os.MkdirAll(root, 0o755)
	return root, nil
}

// HostEnv 返回宿主注入的 CHONKPILOT_* 五元组（缺失为空串）；TEMPDIR/EXEDIR 由本进程自算。
// 进程内无 instance → TEMPDIR 为空（不报错，子进程 env 只为透传）。
func HostEnv() map[string]string {
	workDir := strings.TrimSpace(os.Getenv(EnvWorkDir))
	dataDir := strings.TrimSpace(os.Getenv(EnvDataDir))
	tempDir := ""
	if inst := strings.TrimSpace(os.Getenv(EnvInstance)); inst != "" {
		if r, err := TempRoot(inst); err == nil {
			tempDir = r
		}
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	return map[string]string{
		EnvWorkDir: workDir,
		EnvDataDir: dataDir,
		EnvPurpose: strings.TrimSpace(os.Getenv(EnvPurpose)),
		EnvTempDir: tempDir,
		EnvExeDir:  exeDir,
		EnvProject: workDir,
	}
}

// expandHome 把 ~ / ~/x / ~\x 前缀展开为用户 home；~foo（非 ~/ 前缀）不展开。
func expandHome(p string) string {
	return pathx.ExpandHome(p)
}

// trimTempPrefix 识别 !/ 或 !\ 前缀（临时目录），返回其后的相对部分。
func trimTempPrefix(raw string) (string, bool) {
	return pathx.TrimTempPrefix(raw)
}

// joinTempRoot 把 !/ 之后的相对部分接到临时根下；`..` 越出根 → ok=false。
func joinTempRoot(root, rest string) (string, bool) {
	return pathx.JoinUnderRoot(root, rest)
}

// sanitizeInstance 清洗 instance id（仅保留字母/数字/-/_/.，其余替换为 _）；空/平凡值 → ""。
func sanitizeInstance(id string) string {
	return pathx.SanitizeInstance(id)
}
