package scriptrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunFileRejectsRelative R-11：file 参数为相对路径 → 参数级错误。
func TestRunFileRejectsRelative(t *testing.T) {
	_, err := Run(context.Background(), Options{Runtime: "python", File: "script.py"})
	if err == nil {
		t.Fatal("相对 file 应失败")
	}
	if !strings.Contains(err.Error(), "file") || !strings.Contains(err.Error(), "script.py") {
		t.Fatalf("错误消息应指明 file 与原值，got %q", err.Error())
	}
}

// TestRunFileAcceptsAbsolute 绝对路径不再被路径校验拦截（后续因脚本不存在而失败）。
func TestRunFileAcceptsAbsolute(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "not-exist.py")
	t.Setenv("CHONKPILOT_INTERPRETERS", `{"python":"python"}`)
	_, err := Run(context.Background(), Options{Runtime: "python", File: abs})
	if err == nil {
		t.Fatal("不存在的绝对脚本应失败")
	}
	if strings.Contains(err.Error(), "路径必须是绝对路径") {
		t.Fatalf("绝对路径不应触发 R-11 拒绝，got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "脚本文件不存在") {
		t.Fatalf("应因脚本不存在失败，got %q", err.Error())
	}
}

// TestRunCwdRejectsRelative R-11：cwd（运行目录）为相对路径 → 参数级错误。
func TestRunCwdRejectsRelative(t *testing.T) {
	_, err := Run(context.Background(), Options{Runtime: "cmd", Command: "echo hi", Cwd: "sub/dir"})
	if err == nil {
		t.Fatal("相对 cwd 应失败")
	}
	if !strings.Contains(err.Error(), "cwd") || !strings.Contains(err.Error(), "sub/dir") {
		t.Fatalf("错误消息应指明 cwd 与原值，got %q", err.Error())
	}
}

// TestRunInterpreterRejectsRelative R-11：interpreter 为相对路径 → 参数级错误。
func TestRunInterpreterRejectsRelative(t *testing.T) {
	_, err := Run(context.Background(), Options{Runtime: "python", Command: "print(1)", Interpreter: "python"})
	if err == nil {
		t.Fatal("相对 interpreter 应失败")
	}
	if !strings.Contains(err.Error(), "interpreter") || !strings.Contains(err.Error(), "python") {
		t.Fatalf("错误消息应指明 interpreter 与原值，got %q", err.Error())
	}
}

// TestRunCommandFileEitherOr：command 与 file 必须且仅有一个有值。
func TestRunCommandFileEitherOr(t *testing.T) {
	if _, err := Run(context.Background(), Options{Runtime: "shell"}); err == nil ||
		!strings.Contains(err.Error(), "必须且仅有一个") {
		t.Fatalf("两者皆空应失败，got %v", err)
	}
	if _, err := Run(context.Background(), Options{Runtime: "shell", Command: "echo hi", File: `C:\x.ps1`}); err == nil ||
		!strings.Contains(err.Error(), "必须且仅有一个") {
		t.Fatalf("两者皆有应失败，got %v", err)
	}
}

// TestRunRuntimeWhitelist：未知 runtime 拒绝；空 runtime 归一为 shell。
func TestRunRuntimeWhitelist(t *testing.T) {
	if _, err := Run(context.Background(), Options{Runtime: "haskell", Command: "x"}); err == nil ||
		!strings.Contains(err.Error(), "不支持的 runtime") {
		t.Fatalf("未知 runtime 应失败，got %v", err)
	}
	rt, err := normalizeRuntime("")
	if err != nil || rt != "shell" {
		t.Fatalf("空 runtime 应归一为 shell，got %q err=%v", rt, err)
	}
	for _, name := range []string{"shell", "cmd", "bash", "python", "js", "powershell", "vbs", "java"} {
		if _, err := normalizeRuntime(name); err != nil {
			t.Fatalf("白名单 runtime %q 被拒绝: %v", name, err)
		}
	}
}

// TestJavaTempScriptExtIsJava 回归 I-80：java 临时脚本扩展名必须为 .java。
func TestJavaTempScriptExtIsJava(t *testing.T) {
	script := "public class Hello { public static void main(String[] a){ System.out.println(\"x\"); } }"
	argv, cleanup, err := buildCommand("java", "javafake", script, "")
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if len(argv) != 2 {
		t.Fatalf("argv = %v, want [interp, script]", argv)
	}
	if ext := filepath.Ext(argv[1]); ext != ".java" {
		t.Fatalf("java 临时脚本扩展名 = %q, want .java", ext)
	}
	if b, err := os.ReadFile(argv[1]); err != nil || string(b) != script {
		t.Fatalf("临时脚本内容不符：err=%v content=%q", err, string(b))
	}
}

// TestDefaultRuntimeShellUsesPowerShell 白盒：缺省 runtime=shell 在 Windows 走 PowerShell
// （`-NoProfile -NonInteractive -Command`），保证老 task_run 调用的 PowerShell 语法继续有效。
func TestDefaultRuntimeShellUsesPowerShell(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 断言 PowerShell argv")
	}
	rt, err := normalizeRuntime("")
	if err != nil {
		t.Fatalf("normalizeRuntime: %v", err)
	}
	argv, cleanup, err := buildCommand(rt, defaultPowerShell(), "echo hi", "")
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	want := []string{"powershell", "-NoProfile", "-NonInteractive", "-Command", "echo hi"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %v, want %v", argv, want)
	}

	// file 形态：同前缀 + -File
	argv, _, err = buildCommand(rt, defaultPowerShell(), "", `C:\tmp\a.ps1`)
	if err != nil {
		t.Fatalf("buildCommand(file): %v", err)
	}
	want = []string{"powershell", "-NoProfile", "-NonInteractive", "-File", `C:\tmp\a.ps1`}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("file argv = %v, want %v", argv, want)
	}

	// 配置成 cmd 时回落 cmd 语义（/c）
	argv, _, err = buildCommand(rt, "C:\\Windows\\System32\\cmd.exe", "echo hi", "")
	if err != nil {
		t.Fatalf("buildCommand(cmd interp): %v", err)
	}
	if len(argv) != 3 || argv[1] != "/c" {
		t.Fatalf("cmd 覆盖 argv = %v, want [cmd /c echo hi]", argv)
	}
}

// TestRunShellCommandExecutes 端到端：缺省 runtime=shell 跑一条命令并返回输出。
func TestRunShellCommandExecutes(t *testing.T) {
	res, err := Run(context.Background(), Options{Command: "echo ck-task-run-ok", Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK {
		t.Fatalf("应执行成功，got %+v", res)
	}
	if res.Runtime != "shell" || !strings.Contains(res.Output, "ck-task-run-ok") {
		t.Fatalf("runtime/输出不符: %+v", res)
	}
}

// TestRunTimeoutKillsTree：超时 → ok:false + 超时文案（且进程树被杀，Run 及时返回）。
func TestRunTimeoutKillsTree(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("超时用例使用 Windows 的 ping / cmd")
	}
	start := time.Now()
	res, err := Run(context.Background(), Options{
		Runtime: "cmd", Command: "ping -n 6 127.0.0.1", Timeout: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OK {
		t.Fatalf("超时应 ok:false，got %+v", res)
	}
	if !strings.Contains(res.Error, "超时") {
		t.Fatalf("错误文案应含「超时」，got %q", res.Error)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("超时未及时返回：%v", elapsed)
	}
}

// TestRequiresElevationClassification：740 / 5 → 需提权；普通错误 → 否。
func TestRequiresElevationClassification(t *testing.T) {
	if !requiresElevation(syscall.Errno(740)) {
		t.Fatal("ERROR_ELEVATION_REQUIRED(740) 应判定为需提权")
	}
	if !requiresElevation(syscall.Errno(5)) {
		t.Fatal("ERROR_ACCESS_DENIED(5) 应判定为需提权")
	}
	if requiresElevation(errors.New("boom")) {
		t.Fatal("普通错误不应判定为需提权")
	}
}

// TestElevationMessageContainsCommandLine：拒绝访问提示必须附**原命令行**（供用户复制）。
func TestElevationMessageContainsCommandLine(t *testing.T) {
	const cmd = `net session /delete`
	msg := ElevationMessage(cmd, "exit status 5")
	if !strings.Contains(msg, cmd) {
		t.Fatalf("提示文案应含原命令行 %q，got %q", cmd, msg)
	}
	if !strings.Contains(msg, "管理员") || !strings.Contains(msg, "PowerShell") {
		t.Fatalf("提示文案应说明需管理员身份并给出 PowerShell/CMD 指引，got %q", msg)
	}
}

// TestRunRequiresUnsupported：非 python/js 的 runtime 不支持 requires。
func TestRunRequiresUnsupported(t *testing.T) {
	_, err := Run(context.Background(), Options{
		Runtime: "powershell", Command: "echo hi", Requires: []string{"x"},
	})
	if err == nil || !strings.Contains(err.Error(), "暂不支持 requires") {
		t.Fatalf("应报 requires 不支持，got %v", err)
	}
}

// TestRunEnvOverride：用户 env 覆盖继承环境并传给子进程。
func TestRunEnvOverride(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("断言使用 PowerShell 语法")
	}
	res, err := Run(context.Background(), Options{
		Command: "echo $env:CK_TASK_PROBE",
		Env:     map[string]string{"CK_TASK_PROBE": "probe-ok"},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.Output, "probe-ok") {
		t.Fatalf("用户 env 未生效: %+v", res)
	}
}
