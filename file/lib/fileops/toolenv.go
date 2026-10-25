// toolenv.go — executor 的运行时上下文（决策 R-11 二次升级）。
//
// 上下文不再经 tool arguments 传递：宿主（gateway → mcp-server）经调用上下文（协议 _meta）
// 取 instance/workdir/datadir，spawn executor 时**只注入子进程环境变量** CHONKPILOT_*。
// executor 据此：
//  1. paths.SetTempRoot(CHONKPILOT_INSTANCE) 初始化 !/ 前缀的落地根（<temp>/chonkpilot/<instance>/）；
//  2. 构造 DSL 只读变量表 env（Options.Vars），供脚本以 {{env.CHONKPILOT_WORKDIR}} 等显式拼绝对路径；
//  3. 同名变量另注入 script_run 子进程环境（用户显式 env 优先）。
//
// CHONKPILOT_* 仅在本仓 spawn 的内部 executor 进程内可见（第三方 MCP server 不经我们 spawn，
// 拿不到这些变量）。
package fileops

import (
	"os"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/exedir"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
)

// 宿主注入 env 的变量名（对齐 16-路径解析规范 §8；仅内部 executor 进程可见）。
const (
	EnvInstance     = "CHONKPILOT_INSTANCE"     // 运行实例 id
	EnvWorkDir      = "CHONKPILOT_WORKDIR"      // 项目工作目录（宿主 _meta.work_dir）
	EnvDataDir      = "CHONKPILOT_DATADIR"      // 数据目录（宿主 _meta.data_dir）
	EnvPurpose      = "CHONKPILOT_PURPOSE"      // 本次调用的运行目的（宿主 _meta.purpose；C1 2026-10-15）
	EnvTempDir      = "CHONKPILOT_TEMPDIR"      // 临时目录根（= <temp>/chonkpilot/<instance>/）
	EnvExeDir       = "CHONKPILOT_EXEDIR"       // 当前可执行文件所在目录
	EnvProject      = "CHONKPILOT_PROJECT"      // 项目目录（与 CHONKPILOT_WORKDIR 同值）
	EnvInterpreters = "CHONKPILOT_INTERPRETERS" // runtime → 解释器绝对路径（JSON 对象；script_run 读）
)

// ToolEnv 是宿主注入环境的两视图：Vars 供 DSL 插值，Env 供子进程环境注入。
type ToolEnv struct {
	Vars map[string]any    // dsl.Options.Vars（{"env": map[string]any{...}}，根作用域只注入 env）
	Env  map[string]string // 子进程环境变量（CHONKPILOT_*）
}

// HostEnv 从本进程环境读宿主注入的 CHONKPILOT_*（不要求 instance）：
// CHONKPILOT_WORKDIR/DATADIR/TEMPDIR/EXEDIR/PROJECT 五元组（缺失为空）。
// instance 非空时同时刷新 paths 临时根并给出 TEMPDIR；EXEDIR 由本进程自算。
func HostEnv() map[string]string {
	workDir := strings.TrimSpace(os.Getenv(EnvWorkDir))
	dataDir := strings.TrimSpace(os.Getenv(EnvDataDir))
	tempDir := ""
	if inst := strings.TrimSpace(os.Getenv(EnvInstance)); inst != "" {
		if r, err := paths.SetTempRoot(inst); err == nil {
			tempDir = r
		}
	}
	exeDir := ""
	if d, err := exedir.Dir(); err == nil {
		exeDir = d
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

// BuildToolEnv 构造 DSL 只读变量表（Options.Vars）与子进程 env；缺 CHONKPILOT_INSTANCE →
// 顶层错误（paths.ErrNoInstance）——instance 为空即异常（宿主未注入调用上下文），无法解析
// !/ 或注入 env。纯绝对路径的非 DSL 工具不调用本函数，不受影响。
func BuildToolEnv() (ToolEnv, error) {
	if strings.TrimSpace(os.Getenv(EnvInstance)) == "" {
		return ToolEnv{}, paths.ErrNoInstance
	}
	env := HostEnv()
	envAny := make(map[string]any, len(env))
	for k, v := range env {
		envAny[k] = v
	}
	// 根作用域只注入 env 一个对象；{{env.<NAME>}} 是唯一引用形式（无裸名变量）。
	return ToolEnv{Vars: map[string]any{"env": envAny}, Env: env}, nil
}
