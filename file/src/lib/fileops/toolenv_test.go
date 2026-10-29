package fileops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 注入内部 executor 的子进程上下文（CHONKPILOT_INSTANCE）——宿主 spawn executor 时
// 必然注入，测试据此模拟（缺 instance 属异常，见 TestBuildToolEnvNoInstance）。
func TestMain(m *testing.M) {
	os.Setenv(EnvInstance, "unittest-fileops")
	os.Exit(m.Run())
}

// TestBuildToolEnv 进程环境 CHONKPILOT_* → env 表（决策 R-11 二次升级）：
// TEMPDIR 按 instance 分目录；PROJECT 与 WORKDIR 同值；无裸名别名；缺 instance → 顶层错误。
func TestBuildToolEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvInstance, "ins-1")
	t.Setenv(EnvWorkDir, dir)
	t.Setenv(EnvDataDir, filepath.Join(dir, "d"))

	te, err := BuildToolEnv()
	if err != nil {
		t.Fatalf("BuildToolEnv: %v", err)
	}
	if te.Env[EnvWorkDir] != dir || te.Env[EnvProject] != dir {
		t.Fatalf("workdir/project 应同值，got %v", te.Env)
	}
	wantTemp := filepath.Join(os.TempDir(), "chonkpilot", "ins-1")
	if te.Env[EnvTempDir] != wantTemp {
		t.Fatalf("TEMPDIR = %q, want %q", te.Env[EnvTempDir], wantTemp)
	}
	env, ok := te.Vars["env"].(map[string]any)
	if !ok || env["CHONKPILOT_WORKDIR"] != dir || env["CHONKPILOT_TEMPDIR"] != wantTemp {
		t.Fatalf("Vars.env 不符: %#v", te.Vars)
	}
	// 根作用域只注入 env；无裸名别名（WORKDIR）
	if len(te.Vars) != 1 {
		t.Fatalf("根作用域应只注入 env，got %#v", te.Vars)
	}
	if _, alias := env["WORKDIR"]; alias {
		t.Fatalf("env 表不得含裸名别名 WORKDIR: %#v", env)
	}
}

// TestBuildToolEnvNoInstance 缺 CHONKPILOT_INSTANCE → 顶层错误（不回落后 default）。
func TestBuildToolEnvNoInstance(t *testing.T) {
	t.Setenv(EnvInstance, "")
	if _, err := BuildToolEnv(); err == nil || !strings.Contains(err.Error(), "缺少 instance") {
		t.Fatalf("缺 instance 应报错，got %v", err)
	}
}

// TestHostEnvNoInstance HostEnv 不要求 instance：缺失项为空、TEMPDIR 为空（不报错）。
func TestHostEnvNoInstance(t *testing.T) {
	t.Setenv(EnvInstance, "")
	t.Setenv(EnvWorkDir, "")
	env := HostEnv()
	if env[EnvTempDir] != "" || env[EnvWorkDir] != "" {
		t.Fatalf("缺 instance/workdir 时应为空，got %v", env)
	}
	if env[EnvProject] != "" {
		t.Fatalf("缺 workdir 时 PROJECT 应为空，got %v", env)
	}
}
