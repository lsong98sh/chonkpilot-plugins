package main

import (
	"sort"
	"strings"
	"testing"
)

// TestContractsCoverAllTools 校验 embed 的工具契约：四个工具齐全、均标 hot、schema/description 可用。
// （2026-10-28 P2-3：契约改 `*.tool.md` + go:embed，hot 注册与合并前一致。）
func TestContractsCoverAllTools(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	got := make([]string, 0, len(tools))
	for _, tool := range tools {
		got = append(got, tool.Name)
		if !tool.Hot {
			t.Fatalf("%s 应为 hot（hot_tools 不变）", tool.Name)
		}
		if tool.Schema == nil || tool.Schema["properties"] == nil {
			t.Fatalf("%s 缺少参数 schema", tool.Name)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Fatalf("%s 缺少 description（# 摘要 首段）", tool.Name)
		}
	}
	sort.Strings(got)
	want := []string{"task_llm", "task_run", "task_status", "task_stop"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("契约工具集 = %v, want %v", got, want)
	}
}

// TestTaskRunContractShape task_run 契约必须同时暴露 command / file（二选一）与 runtime 等参数。
func TestTaskRunContractShape(t *testing.T) {
	tools, err := loadContracts()
	if err != nil {
		t.Fatalf("loadContracts: %v", err)
	}
	var props map[string]any
	for _, tool := range tools {
		if tool.Name == "task_run" {
			props, _ = tool.Schema["properties"].(map[string]any)
		}
	}
	if props == nil {
		t.Fatal("未找到 task_run 契约")
	}
	for _, k := range []string{"command", "file", "runtime", "requires", "interpreter", "cwd", "timeoutSec", "env", "args"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("task_run 契约缺少参数 %s", k)
		}
	}
	if _, ok := props["workdir"]; ok {
		t.Fatal("对外参数名应为 cwd（workdir 为内部名）")
	}
}

// TestSettingsInterpreterMap 解释器配置 → runtime 表（node 键映射到 js runtime）。
func TestSettingsInterpreterMap(t *testing.T) {
	s := settings{
		Shell:    `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		NodePath: `C:\node\node.exe`,
		JavaPath: `C:\jdk\bin\java.exe`,
	}
	m := s.interpreterMap()
	if m["shell"] == "" || m["js"] != `C:\node\node.exe` || m["java"] != `C:\jdk\bin\java.exe` {
		t.Fatalf("interpreterMap = %v", m)
	}
	if _, ok := m["python"]; ok {
		t.Fatal("未配置的解释器不应入表")
	}
}
