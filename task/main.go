// chonkpilot-task —— 任务系列工具插件（执行 / LLM / 状态 / 终止）。
//
// 四个工具：
//   - task_run    执行本机命令（前台、带超时；cwd 缺省取调用上下文的 work_dir）
//   - task_llm    单次 LLM 调用（宿主 plugin/v1/llm.step，插件拿不到 apiKey）
//   - task_status 读取任务树 / 任务列表（数据面 task.tree / task.tasks）
//   - task_stop   终止任务（数据面 task.stop，payload = {task_id,cascade?}）
//
// 配置（插件自身，不分四级）：默认超时 / 默认工作目录 / shell / 输出上限。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

// 复用层别名：参数取值 / 信封解包统一由 pluginsdk 提供。
var (
	asString = pluginsdk.Str
	toInt    = pluginsdk.Int
	unwrap   = pluginsdk.Unwrap
)

type taskPlugin struct {
	p   *pluginsdk.Plugin
	cfg *configManager
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "task",
		Name:    "任务工具",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("task: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	t := &taskPlugin{p: p, cfg: newConfigManager(p)}
	p.Tool("task_run", "执行一条本机命令（前台执行，带超时，返回退出码与输出）。cwd 缺省 = 当前会话工作目录。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "要执行的命令（Windows 用 PowerShell，其它平台用 /bin/sh -c）"},
				"cwd":        map[string]any{"type": "string", "description": "工作目录（绝对路径；缺省 = 会话 work_dir / 插件默认目录）"},
				"timeoutSec": map[string]any{"type": "integer", "description": "超时秒数（缺省用配置；上限 600）"},
			},
			"required": []string{"command"},
		}, t.run)

	p.Tool("task_llm", "调用一次 LLM（无会话、无工具）并返回文本。适合摘要 / 改写 / 判定。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt":     map[string]any{"type": "string", "description": "用户提示词"},
				"system":     map[string]any{"type": "string", "description": "系统提示词（可选）"},
				"timeoutSec": map[string]any{"type": "integer", "description": "超时秒数（缺省 120）"},
			},
			"required": []string{"prompt"},
		}, t.llm)

	p.Tool("task_status", "查询当前会话的任务树 / 任务列表（只读）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "最多返回的节点/任务条数（缺省用配置）"},
			},
		}, t.status)

	p.Tool("task_stop", "终止一个任务（按 task_id）；cascade=true 连带子任务。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "要终止的任务 id"},
				"cascade": map[string]any{"type": "boolean", "description": "是否连带子任务"},
			},
			"required": []string{"task_id"},
		}, t.stop)

	for _, n := range []string{"task_run", "task_llm", "task_status", "task_stop"} {
		p.HotTool(n)
	}
	p.OnRequest("status", t.info)
	p.OnRequest("ping", pluginsdk.PingHandler(pluginVersion))
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/task/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := t.cfg.Init(context.Background()); err != nil {
		slog.Warn("task: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range t.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("task: 运行结束", "err", err)
	}
}

// ── task_run ─────────────────────────

func (t *taskPlugin) run(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	cfg := t.cfg.Settings()
	command := strings.TrimSpace(asString(args["command"]))
	if command == "" {
		return nil, errors.New("command 不能为空")
	}
	cwd := strings.TrimSpace(asString(args["cwd"]))
	if cwd == "" {
		cwd = cfg.DefaultCwd
	}
	if cwd == "" && cx.WorkDir != "" {
		cwd = cx.WorkDir
	}
	timeout := cfg.DefaultTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 600 {
		timeout = 600
	}
	maxKB := cfg.MaxOutputKB
	if maxKB <= 0 {
		maxKB = 256
	}

	shell := cfg.Shell
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		if shell == "" {
			shell = "powershell"
		}
		if strings.EqualFold(shell, "cmd") {
			cmd = exec.CommandContext(ctx, "cmd", "/C", command)
		} else {
			cmd = exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", command)
		}
	} else {
		if shell == "" {
			shell = "/bin/sh"
		}
		cmd = exec.CommandContext(ctx, shell, "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	var buf bytes.Buffer
	limit := maxKB << 10
	cmd.Stdout = &limitedWriter{buf: &buf, max: limit}
	cmd.Stderr = &limitedWriter{buf: &buf, max: limit}

	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd = rebind(cmd, cctx, killTree)

	start := time.Now()
	err := cmd.Run()
	ms := time.Since(start).Milliseconds()
	out := buf.String()
	if len(out) > limit {
		out = out[:limit]
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	res := map[string]any{
		"command":   command,
		"cwd":       cwd,
		"exit_code": code,
		"ms":        ms,
		"output":    out,
		"truncated": buf.Len() >= limit,
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res["ok"] = false
			return res, nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			res["ok"] = false
			res["error"] = fmt.Sprintf("超时（%ds）已终止", timeout)
			return res, nil
		}
		return nil, err
	}
	res["ok"] = true
	return res, nil
}

// rebind 用带超时的 ctx 重建命令（exec.Cmd 的 ctx 不可替换，故重新构造）。
// cancel（可选）用于覆盖默认的「仅杀直接子进程」：Windows 下 shell 会再派生真实命令
// （powershell → ping 等），只杀 shell 会留下孤儿 → 由 killTree 杀整棵进程树。
func rebind(cmd *exec.Cmd, ctx context.Context, cancel func(*exec.Cmd) error) *exec.Cmd {
	out := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	out.Dir = cmd.Dir
	out.Env = cmd.Env
	out.Stdout = cmd.Stdout
	out.Stderr = cmd.Stderr
	if cancel != nil {
		out.Cancel = func() error { return cancel(out) }
	}
	return out
}

// killTree 杀掉 cmd 的整棵进程树（Windows: taskkill /T /F；其它平台回落 Process.Kill）。
func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
		_ = k.Run()
	}
	return cmd.Process.Kill()
}

type limitedWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	remain := w.max - w.buf.Len()
	if remain <= 0 {
		return len(p), nil // 丢弃超出部分（上层用 truncated 标识）
	}
	if len(p) > remain {
		w.buf.Write(p[:remain])
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// ── task_llm ─────────────────────────

func (t *taskPlugin) llm(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	prompt := strings.TrimSpace(asString(args["prompt"]))
	if prompt == "" {
		return nil, errors.New("prompt 不能为空")
	}
	timeout := 120
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeout = n
	}
	text, err := t.p.LLM(ctx, prompt, strings.TrimSpace(asString(args["system"])), time.Duration(timeout)*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]any{"text": text}, nil
}

// ── task_status ─────────────────────────

func (t *taskPlugin) status(ctx context.Context, _ map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.InstanceID == "" {
		return nil, errors.New("缺少会话实例（instance_id）")
	}
	cfg := t.cfg.Settings()
	limit := cfg.StatusLimit
	if limit <= 0 {
		limit = 50
	}
	treeRaw, err := t.p.DataRead(ctx, "task", "tree", cx.InstanceID, map[string]any{"session_id": cx.Session})
	if err != nil {
		return nil, fmt.Errorf("读取任务树失败: %w", err)
	}
	tasksRaw, err := t.p.DataRead(ctx, "task", "tasks", cx.InstanceID, map[string]any{"session_id": cx.Session})
	if err != nil {
		return nil, fmt.Errorf("读取任务列表失败: %w", err)
	}
	return map[string]any{
		"ok":    true,
		"limit": limit,
		"tree":  unwrap(treeRaw),
		"tasks": unwrap(tasksRaw),
	}, nil
}

// ── task_stop ─────────────────────────

func (t *taskPlugin) stop(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
	if cx.InstanceID == "" {
		return nil, errors.New("缺少会话实例（instance_id）")
	}
	taskID := strings.TrimSpace(asString(args["task_id"]))
	if taskID == "" {
		return nil, errors.New("task_id 不能为空")
	}
	params := map[string]any{"task_id": taskID}
	if v, ok := args["cascade"].(bool); ok {
		params["cascade"] = v
	}
	raw, err := t.p.DataWrite(ctx, "task", "stop", cx.InstanceID, params)
	if err != nil {
		return nil, fmt.Errorf("终止任务失败: %w", err)
	}
	return map[string]any{"ok": true, "result": unwrap(raw)}, nil
}

func (t *taskPlugin) info(_ context.Context, _ map[string]any) (any, error) {
	cfg := t.cfg.Settings()
	return map[string]any{
		"ok":                true,
		"version":           pluginVersion,
		"defaultTimeoutSec": cfg.DefaultTimeoutSec,
		"defaultCwd":        cfg.DefaultCwd,
		"shell":             cfg.Shell,
		"maxOutputKB":       cfg.MaxOutputKB,
		"statusLimit":       cfg.StatusLimit,
		"goos":              runtime.GOOS,
	}, nil
}

// 参数取值 / 信封解包见文件头的复用层别名（pluginsdk.Str/Int/Unwrap）。
