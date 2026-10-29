// Package scriptrun 是 `task_run` 的多 runtime 脚本执行器（2026-10-28 P2-3：由主仓
// `mcp-tools/internal/scriptrun` 迁入 task 插件，对外工具名统一为 `task_run`）。
//
// runtime 白名单：shell / cmd / bash / python / js / powershell / vbs / java。
// 参数：command（脚本/命令内容）与 file（脚本文件绝对路径）**二选一**；runtime 缺省 shell；
// requires（pip/npm 自动安装）；interpreter（显式解释器，优先级最高）；cwd（运行目录，
// 内部即原 workdir）；env；args；超时由调用方设定（超时即杀整棵进程树）。
//
// 解释器解析顺序（2026-10-28 定稿）：
//  1. `interpreter` 参数（可执行文件路径，受 R-11 路径约束）
//  2. 插件配置（task 插件配置页的解释器路径；`shell` 另有 Shell 覆盖项）
//  3. `CHONKPILOT_INTERPRETERS` 环境变量（`{runtime: 路径}` JSON；迁移期兼容）
//  4. 系统默认（cmd/bash；Windows 的 shell = PowerShell，其它平台 = /bin/sh）
//
// Windows 的 `shell` 一律走 PowerShell（`-NoProfile -NonInteractive -Command`），与合并前
// `task_run` 行为一致（老调用里的 PowerShell 语法继续有效）；`cmd` / `bash` 为显式 runtime。
// 跑 exe 不需要额外 runtime：用 `shell`（Windows = PowerShell）或 `cmd`，`file` = exe 路径，
// 或 `command` = exe 路径 + 参数。
//
// 提权：**不做** elevated 参数、不走 UAC —— 以调用方进程权限运行；启动失败于
// ERROR_ELEVATION_REQUIRED(740) 或返回 ERROR_ACCESS_DENIED(5) 时 `ok:false` + 提示文案
// （附可复制的命令行，请用户以管理员身份打开 PowerShell / CMD 后手动粘贴执行）。
package scriptrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chonkpilot/chonkpilot-task-plugin/lib/pathutil"
	"github.com/lsong98sh/chonkpilot-plugins/shared/winproc"
)

// runtimes 支持的 runtime 白名单（与工具契约一致）。
var runtimes = map[string]struct{}{
	"shell": {}, "cmd": {}, "bash": {},
	"python": {}, "js": {}, "powershell": {}, "vbs": {}, "java": {},
}

// scriptExt 代码文件型 runtime 的临时脚本扩展名。
// java 必须为 .java：JDK 11+ 单文件源码模式（JEP 330）要求 `java <file>.java`，
// 其它扩展名会被当作类名 → ClassNotFoundException。
var scriptExt = map[string]string{
	"python": ".py", "js": ".js", "powershell": ".ps1", "vbs": ".vbs", "bash": ".sh", "java": ".java",
}

// defaultOutputLimit 是 stdout+stderr 累计读取上限（8MB）：超出即封顶丢弃尾部，
// 防脚本产出无界输出全量入内存；调用方的配置上限（MaxOutputKB）更小时以其为准。
const defaultOutputLimit = 8 << 20

// installTimeout 是依赖安装（pip/npm）的独立上限：安装可能因网络/交互而挂起。
const installTimeout = 5 * time.Minute

// Options 是一次脚本执行的入参（由工具 handler 组装，不含任何配置读取）。
type Options struct {
	Runtime      string            // runtime（空 = shell；已在 Run 内归一）
	Command      string            // 脚本/命令内容（与 File 二选一）
	File         string            // 脚本文件路径（与 Command 二选一）
	Cwd          string            // 运行目录（空 = 不设置）
	InstanceID   string            // 调用实例（解析 !/ 临时目录）
	Requires     []string          // 依赖（python → pip install；js → npm install）
	Interpreter  string            // 显式解释器（优先级最高）
	Env          map[string]string // 用户显式环境变量（同名覆盖）
	Args         []string          // 追加到命令行的参数
	Interpreters map[string]string // 插件配置：runtime → 解释器（含 shell 覆盖）
	Timeout      time.Duration     // 超时（<=0 → 120s；超时杀整棵进程树）
	MaxOutput    int               // 输出字节上限（<=0 → 8MB）
}

// Result 是一次执行的结果（形状沿用 task_run）。
type Result struct {
	Runtime   string `json:"runtime"`
	Command   string `json:"command"`
	Cwd       string `json:"cwd"`
	ExitCode  int    `json:"exit_code"`
	MS        int64  `json:"ms"`
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
}

// Run 执行一次脚本/命令。返回的 error 表示**参数级/前置条件**失败（runtime 非法、
// command 与 file 二选一违规、路径不合规、解释器未配置、依赖安装失败等）；
// 运行期失败（退出码非 0、超时、拒绝访问）以 Result{OK:false, Error} 返回。
func Run(ctx context.Context, opt Options) (Result, error) {
	rt, err := normalizeRuntime(opt.Runtime)
	if err != nil {
		return Result{}, err
	}
	command := strings.TrimSpace(opt.Command)
	filePath := strings.TrimSpace(opt.File)
	if (command == "") == (filePath == "") {
		return Result{}, errors.New("command 与 file 必须且仅有一个有值")
	}

	cwd, msg := pathutil.Resolve(strings.TrimSpace(opt.Cwd), opt.InstanceID)
	if msg != "" {
		return Result{}, errors.New("cwd：" + msg)
	}
	if filePath != "" {
		resolved, msg := pathutil.Resolve(filePath, opt.InstanceID)
		if msg != "" {
			return Result{}, errors.New("file：" + msg)
		}
		filePath = resolved
	}

	interp, err := resolveInterpreter(rt, opt.Interpreter, opt.Interpreters, opt.InstanceID)
	if err != nil {
		return Result{}, err
	}

	// 宿主注入环境（R-11 二次升级）：本进程 CHONKPILOT_* → 子进程环境；用户显式 env 优先。
	childEnv := mergeChildEnv(os.Environ(), pathutil.HostEnv(), opt.Env)

	if len(opt.Requires) > 0 {
		if err := installRequires(rt, interp, cwd, opt.Requires, childEnv); err != nil {
			return Result{}, err
		}
	}

	argv, cleanup, err := buildCommand(rt, interp, command, filePath)
	if err != nil {
		return Result{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	argv = append(argv, opt.Args...)

	limit := opt.MaxOutput
	if limit <= 0 || limit > defaultOutputLimit {
		limit = defaultOutputLimit
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	display := displayCommand(command, argv)

	out := &cappedBuffer{limit: limit}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	runErr := execute(cctx, argv, cwd, childEnv, out)
	res := Result{
		Runtime:   rt,
		Command:   display,
		Cwd:       cwd,
		ExitCode:  0,
		MS:        time.Since(start).Milliseconds(),
		Output:    strings.TrimSpace(out.String()),
		Truncated: out.Dropped(),
		OK:        true,
	}
	if errors.Is(cctx.Err(), context.DeadlineExceeded) {
		res.OK = false
		res.ExitCode = exitCodeOf(runErr)
		res.Error = fmt.Sprintf("超时（%s）已终止（已杀进程树）", formatTimeout(timeout))
		return res, nil
	}
	if runErr == nil {
		return res, nil
	}
	res.ExitCode = exitCodeOf(runErr)
	res.OK = false
	if requiresElevation(runErr) {
		res.Error = ElevationMessage(display, runErr.Error())
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.Error = fmt.Sprintf("脚本执行失败（exit %d）：%s", res.ExitCode, runErr)
		return res, nil
	}
	res.ExitCode = -1
	res.Error = runErr.Error()
	return res, nil
}

// execute 构造并运行命令；ctx 取消/超时 → 杀**整棵进程树**（Windows: taskkill /T /F）。
func execute(ctx context.Context, argv []string, dir string, env []string, w *cappedBuffer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.SysProcAttr = winproc.SysProcAttr()
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	cmd.Stdout = w
	cmd.Stderr = w
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 3 * time.Second
	return cmd.Run()
}

// killTree 杀掉 cmd 的整棵进程树（Windows: taskkill /T /F；其它平台回落 Process.Kill）。
func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		// 先按树杀（shell 会再派生真实命令，只杀 shell 会留下孤儿）；失败再由 Process.Kill 兜底。
		_ = winproc.KillTree(cmd.Process.Pid)
	}
	return cmd.Process.Kill()
}

// normalizeRuntime 归一并校验 runtime（空 = shell）。
func normalizeRuntime(raw string) (string, error) {
	rt := strings.ToLower(strings.TrimSpace(raw))
	if rt == "" {
		return "shell", nil
	}
	if _, ok := runtimes[rt]; !ok {
		return "", fmt.Errorf("不支持的 runtime: %s（可选 shell/cmd/bash/python/js/powershell/vbs/java）", rt)
	}
	return rt, nil
}

// resolveInterpreter 解析解释器（顺序见包注释）。显式 interpreter 受 R-11 约束；
// 插件配置 / 环境变量为内部注入，不做配置侧绝对路径校验（与迁移前口径一致）。
func resolveInterpreter(rt, explicit string, cfg map[string]string, instanceID string) (string, error) {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		resolved, msg := pathutil.Resolve(explicit, instanceID)
		if msg != "" {
			return "", errors.New("interpreter：" + msg)
		}
		return resolved, nil
	}
	if v := strings.TrimSpace(cfg[rt]); v != "" {
		return v, nil
	}
	if m := interpretersFromEnv(); m != nil {
		if v := strings.TrimSpace(m[rt]); v != "" {
			return v, nil
		}
	}
	switch rt {
	case "cmd":
		return "cmd", nil
	case "bash":
		return "bash", nil
	case "powershell":
		return defaultPowerShell(), nil
	case "shell":
		if runtime.GOOS == "windows" {
			return defaultPowerShell(), nil
		}
		return "sh", nil
	}
	return "", fmt.Errorf("runtime %q 未配置解释器路径：请在 task 插件配置页填写「%s 解释器」，或显式传 interpreter", rt, rt)
}

// defaultPowerShell 返回 Windows PowerShell 的可执行名（走 PATH）。
func defaultPowerShell() string { return "powershell" }

// interpretersFromEnv 解析 CHONKPILOT_INTERPRETERS（`{runtime: 解释器}` JSON 对象）；
// 缺失/非法 → nil。迁移期兼容（配置已归 task 插件，宿主不再注入）。
func interpretersFromEnv() map[string]string {
	raw := strings.TrimSpace(os.Getenv(pathutil.EnvInterpreters))
	if raw == "" {
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	return m
}

// buildCommand 构造执行命令。命令串型（shell/cmd/bash）：解释器 + 串或脚本文件；
// 代码文件型：临时脚本文件（或用户 file） + 解释器。返回 argv 与清理函数。
func buildCommand(rt, interp, script, filePath string) ([]string, func(), error) {
	switch rt {
	case "cmd":
		if filePath != "" {
			return []string{interp, "/c", filePath}, nil, nil
		}
		return []string{interp, "/c", script}, nil, nil
	case "bash":
		if filePath != "" {
			return []string{interp, filePath}, nil, nil
		}
		return []string{interp, "-c", script}, nil, nil
	case "shell":
		if runtime.GOOS == "windows" {
			// Windows 的 shell = PowerShell（老 task_run 行为）；配置成 cmd 时回落 cmd 语义。
			if isCmdInterp(interp) {
				if filePath != "" {
					return []string{interp, "/c", filePath}, nil, nil
				}
				return []string{interp, "/c", script}, nil, nil
			}
			if filePath != "" {
				return []string{interp, "-NoProfile", "-NonInteractive", "-File", filePath}, nil, nil
			}
			return []string{interp, "-NoProfile", "-NonInteractive", "-Command", script}, nil, nil
		}
		if filePath != "" {
			return []string{interp, filePath}, nil, nil
		}
		return []string{interp, "-c", script}, nil, nil
	}

	// 代码文件型
	if filePath != "" {
		if _, err := os.Stat(filePath); err != nil {
			return nil, nil, fmt.Errorf("脚本文件不存在: %s", filePath)
		}
		return []string{interp, filePath}, nil, nil
	}
	ext := scriptExt[rt]
	if ext == "" {
		ext = ".txt"
	}
	f, err := os.CreateTemp("", "ck_script_*"+ext)
	if err != nil {
		return nil, nil, fmt.Errorf("创建临时脚本失败: %s", err)
	}
	path := f.Name()
	if _, err := f.WriteString(script); err != nil {
		f.Close()
		os.Remove(path)
		return nil, nil, fmt.Errorf("写入临时脚本失败: %s", err)
	}
	f.Close()
	// powershell 需绕过执行策略
	if rt == "powershell" {
		return []string{interp, "-ExecutionPolicy", "Bypass", "-File", path}, func() { os.Remove(path) }, nil
	}
	return []string{interp, path}, func() { os.Remove(path) }, nil
}

// isCmdInterp 判定解释器是否为 cmd（`cmd` / `C:\Windows\System32\cmd.exe` 等）。
func isCmdInterp(interp string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(interp)))
	return base == "cmd" || base == "cmd.exe"
}

// displayCommand 返回可复制的命令行：命令串形态 = 原命令文本；文件形态 = 完整 argv。
func displayCommand(command string, argv []string) string {
	if strings.TrimSpace(command) != "" {
		return command
	}
	return strings.Join(argv, " ")
}

// exitCodeOf 取退出码（非 ExitError → -1）。
func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// formatTimeout 把超时折成便于阅读的秒串（整秒不带小数）。
func formatTimeout(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// requiresElevation 判定错误是否为「需要管理员权限」：
// 启动失败于 ERROR_ELEVATION_REQUIRED(740) / ERROR_ACCESS_DENIED(5)，或运行返回退出码 5。
func requiresElevation(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if errors.As(err, &errno) && (errno == 740 || errno == 5) {
		return true
	}
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 5
}

// ElevationMessage 组装「需要管理员权限」的提示文案（附可复制命令行）。
// 抽成导出函数便于白盒断言（文案是契约的一部分）。
func ElevationMessage(command, detail string) string {
	return "需要管理员权限（Windows 返回 ERROR_ELEVATION_REQUIRED(740) 或 ERROR_ACCESS_DENIED(5)），" +
		"本工具不提权、不弹 UAC。请以管理员身份打开 PowerShell / CMD 后手动粘贴执行：\n" +
		command + "\n（原始错误：" + detail + "）"
}

// mergeChildEnv 合并子进程环境：base（os.Environ）为底，groups 依序覆盖（后组同名优先），
// 按变量名去重（保留最后一次取值）。
func mergeChildEnv(base []string, groups ...map[string]string) []string {
	idx := map[string]int{}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if _, dup := idx[k]; dup {
			continue
		}
		idx[k] = len(out)
		out = append(out, kv)
	}
	for _, g := range groups {
		for k, v := range g {
			kv := k + "=" + v
			if i, ok := idx[k]; ok {
				out[i] = kv
				continue
			}
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// installRequires 安装依赖（静默执行，不支持交互式安装）。
// python → pip install；js → npm install --no-save（无 package.json 先 init -y）；其余不支持。
func installRequires(rt, interp, workDir string, requires []string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), installTimeout)
	defer cancel()
	switch rt {
	case "python":
		pkgs := append([]string{"-m", "pip", "install", "--disable-pip-version-check", "-q"}, requires...)
		cmd := exec.CommandContext(ctx, interp, pkgs...)
		cmd.SysProcAttr = winproc.SysProcAttr()
		cmd.Dir = workDir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("pip install 失败：%s\n%s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case "js":
		if _, err := os.Stat(filepath.Join(workDir, "package.json")); err != nil {
			initCmd := exec.CommandContext(ctx, "npm", "init", "-y")
			initCmd.SysProcAttr = winproc.SysProcAttr()
			initCmd.Dir = workDir
			initCmd.Env = env
			if out, err := initCmd.CombinedOutput(); err != nil {
				return fmt.Errorf("npm init 失败：%s\n%s", err, strings.TrimSpace(string(out)))
			}
		}
		pkgs := append([]string{"install", "--no-save"}, requires...)
		cmd := exec.CommandContext(ctx, "npm", pkgs...)
		cmd.SysProcAttr = winproc.SysProcAttr()
		cmd.Dir = workDir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("npm install 失败：%s\n%s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		return fmt.Errorf("runtime %s 暂不支持 requires 依赖安装", rt)
	}
}

// cappedBuffer 是带累计上限的字节缓冲（并发安全：stdout/stderr 可能共写）：
// 写满上限后丢弃其余内容并标记截断。
type cappedBuffer struct {
	mu      sync.Mutex
	buf     []byte
	limit   int
	dropped bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if remain := w.limit - len(w.buf); remain > 0 {
		if len(p) <= remain {
			w.buf = append(w.buf, p...)
		} else {
			w.buf = append(w.buf, p[:remain]...)
			w.dropped = true
		}
	} else {
		w.dropped = true
	}
	return len(p), nil
}

// String 返回累计输出（截断时不追加提示，由 Result.Truncated 标识）。
func (w *cappedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// Dropped 返回是否发生截断。
func (w *cappedBuffer) Dropped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}
