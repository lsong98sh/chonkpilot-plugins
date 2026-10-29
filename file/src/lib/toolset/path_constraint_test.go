// path_constraint_test.go — 文件域路径强约束（决策 R-11）的进程内回归。
//
// 2026-10-25（PLUGIN-SPLIT-1）：file 域 11 工具随插件迁出主仓，`chonkpilot-tools-executor.exe`
// 不再是执行体 → 原进程级黑盒（`src/test/chonkpilot-mcp-tools/unittest/path_constraint_test.go`）
// 的 file 段改为**进程内**调用本包 `Dispatch`，断言口径不变（相对路径失败并带原值 /
// 绝对路径成功 / `!/` 前缀按 CHONKPILOT_INSTANCE 落地 / md5 冲突 skipped）。
package toolset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
)

// runFile 进程内执行一个 file 域工具，返回 (isError, text)。
func runFile(t *testing.T, tool string, args map[string]any) (bool, string) {
	t.Helper()
	res, ok := Dispatch(context.Background(), tool, args)
	if !ok {
		t.Fatalf("未登记的工具：%s", tool)
	}
	return res.IsError, res.Text
}

type execResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

type execSummary struct {
	Total   int `json:"total"`
	OK      int `json:"ok"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

type execEnvelope struct {
	Results []execResult `json:"results"`
	Summary execSummary  `json:"summary"`
}

func parseEnvelope(t *testing.T, out string) execEnvelope {
	t.Helper()
	var env execEnvelope
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("输出不是结果 JSON：%v；out=%q", err, out)
	}
	return env
}

// fileArgs 构造 file 域工具入参 {purpose, targets:[target]}。
func fileArgs(target map[string]any) map[string]any {
	return map[string]any{"purpose": "路径约束测试", "targets": []any{target}}
}

func writeTmp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// TestRejectRelativePaths 各工具的相对路径入参 → 失败 + 消息含原值 + 路径约束说明。
func TestRejectRelativePaths(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"file_read.path相对", "file_read",
			fileArgs(map[string]any{"path": "a.txt"}), "a.txt"},
		{"file_find.root相对", "file_find",
			fileArgs(map[string]any{"root": "src"}), "src"},
		{"file_diff.left相对", "file_diff",
			fileArgs(map[string]any{"left": "a.txt", "right": "b.txt"}), "a.txt"},
		{"file_replace.path相对", "file_replace",
			fileArgs(map[string]any{"path": "src/main.py", "find": "a", "replace": "b"}), "src/main.py"},
		{"file_move.to相对", "file_move",
			fileArgs(map[string]any{"from": writeTmp(t, "x.txt", "x"), "to": "relative/path.txt"}), "relative/path.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isErr, out := runFile(t, tc.tool, tc.args)
			if !isErr {
				t.Fatalf("相对路径应失败，got out=%q", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("错误消息应含原值 %q，got out=%q", tc.want, out)
			}
			if !strings.Contains(out, "绝对路径") {
				t.Fatalf("错误消息应含路径约束说明，got out=%q", out)
			}
		})
	}
}

// TestAcceptAbsolutePaths 绝对路径入参 → 正常。
func TestAcceptAbsolutePaths(t *testing.T) {
	f := writeTmp(t, "ok.txt", "hello\n")
	dir := filepath.Dir(f)

	t.Run("file_read", func(t *testing.T) {
		isErr, out := runFile(t, "file_read", fileArgs(map[string]any{"path": f}))
		if isErr {
			t.Fatalf("绝对路径应成功，out=%q", out)
		}
		if !strings.Contains(out, "hello") {
			t.Fatalf("应读到文件内容，out=%q", out)
		}
	})
	t.Run("file_find", func(t *testing.T) {
		isErr, out := runFile(t, "file_find", fileArgs(map[string]any{"root": dir}))
		if isErr {
			t.Fatalf("绝对路径应成功，out=%q", out)
		}
	})
	t.Run("file_diff", func(t *testing.T) {
		f2 := writeTmp(t, "ok2.txt", "world\n")
		isErr, out := runFile(t, "file_diff", fileArgs(map[string]any{"left": f, "right": f2}))
		if isErr {
			t.Fatalf("绝对路径应成功，out=%q", out)
		}
	})
}

// TestTempPrefix `!/` 前缀映射到 <系统 temp>/chonkpilot/<instance>/（R-11 二次升级）。
//
// 插件形态下 instance 经协议 _meta（`chonkpilot.instance_id`）注入 → 插件在调用前
// `paths.SetTempRoot(inst)`（见 `file/main.go applyCallMeta`），本用例直接设置该根。
func TestTempPrefix(t *testing.T) {
	root, err := paths.SetTempRoot("unittest-l2")
	if err != nil {
		t.Fatalf("SetTempRoot: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "l2.txt"), []byte("temp-prefix-ok\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// 写：!/ 前缀落盘到 <temp>/chonkpilot/<instance>/
	isErr, out := runFile(t, "file_write", fileArgs(map[string]any{
		"path": "!/l2-out.txt", "content": "temp-prefix-ok\n",
	}))
	if isErr {
		t.Fatalf("!/ 前缀写应成功，out=%q", out)
	}
	data, rerr := os.ReadFile(filepath.Join(root, "l2-out.txt"))
	if rerr != nil {
		t.Fatalf("读取落盘结果失败：%v（out=%q）", rerr, out)
	}
	if !strings.Contains(string(data), "temp-prefix-ok") {
		t.Fatalf("临时目录文件内容不符，got %q", string(data))
	}

	// 读：!/ 前缀解析到同一 instance 目录
	isErr, out = runFile(t, "file_read", fileArgs(map[string]any{"path": "!/l2.txt"}))
	if isErr {
		t.Fatalf("!/ 前缀读应成功，out=%q", out)
	}
	if !strings.Contains(out, "temp-prefix-ok") {
		t.Fatalf("应读到临时目录文件内容，out=%q", out)
	}
}

// TestTempPrefixRequiresInstance 缺 instance 时 !/ 解析 → 失败（缺 instance）。
// 直接断言 paths 的无状态入口（ResolvePathFor("")），不受包内全局根影响。
func TestTempPrefixRequiresInstance(t *testing.T) {
	if _, msg := paths.ResolvePathFor("", "!/l2.txt", ""); !strings.Contains(msg, "缺少 instance") {
		t.Fatalf("缺 instance 时 !/ 应报「缺少 instance」，got=%q", msg)
	}
}

// TestMD5MismatchSkips 运行时冲突（expectMd5 不一致）→ 整体成功且该项 skipped、不写盘。
func TestMD5MismatchSkips(t *testing.T) {
	f := writeTmp(t, "m.txt", "original")
	isErr, out := runFile(t, "file_replace", fileArgs(map[string]any{
		"path": f, "find": "original", "replace": "changed", "expectMd5": "deadbeef",
	}))
	if isErr {
		t.Fatalf("md5 不一致应整体成功，out=%q", out)
	}
	env := parseEnvelope(t, out)
	if len(env.Results) != 1 || env.Results[0].Status != "skipped" {
		t.Fatalf("md5 不一致应记 skipped，got=%+v out=%q", env.Results, out)
	}
	if !strings.Contains(env.Results[0].Reason, "md5") {
		t.Fatalf("skip 原因应含 md5，got=%q", env.Results[0].Reason)
	}
	if env.Summary.Failed != 0 || env.Summary.Skipped != 1 {
		t.Fatalf("summary 不符：%+v", env.Summary)
	}
	if b, _ := os.ReadFile(f); string(b) != "original" {
		t.Fatalf("md5 不一致不应写盘，got %q", string(b))
	}
}
