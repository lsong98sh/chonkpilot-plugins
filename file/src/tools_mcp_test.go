package main

// tools_mcp_test.go —— file 域 11 工具的 **MCP 面**测试（进程内，无配置依赖）。
//
// 口径（PLUGIN-SPLIT-1 收尾）：契约源 = `tools/file/*.tool.md`；测试与生产同一条分派链
// （`contract.Register` → `dispatch` → `toolset`），客户端走 go-sdk 内存传输，
// 断言契约数量/名称/schema、典型读写链路，以及 `_meta.chonkpilot.instance_id`
// 对 `!/` 落地根（<temp>/chonkpilot/<instance>/）的作用。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wantTools 是与 `file/tools/file/*.tool.md` 一一对应的工具名（11 个）。
var wantTools = []string{
	"file_copy", "file_delete", "file_detect", "file_diff", "file_find", "file_iconv",
	"file_move", "file_patch", "file_read", "file_replace", "file_write",
}

// loadTools 读取本仓契约目录。
func loadTools(t *testing.T) []contract.Tool {
	t.Helper()
	tools, err := contract.LoadDir(filepath.Join("tools", "file"))
	if err != nil {
		t.Fatalf("加载契约: %v", err)
	}
	return tools
}

// newMCPFace 起一个只含 file 工具的进程内 MCP server，返回已握手的客户端会话。
func newMCPFace(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "file-test", Version: "0.0.0"}, nil)
	if err := contract.Register(srv, loadTools(t), func(ctx context.Context, name string, args map[string]any) (string, bool, error) {
		text, isErr := dispatch(ctx, name, args)
		return text, isErr, nil
	}); err != nil {
		t.Fatalf("注册契约: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("挂载 server 传输: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cli := mcp.NewClient(&mcp.Implementation{Name: "file-test-client", Version: "0.0.0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("连接 MCP server: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callTool 调一次工具，返回拼接后的文本与是否业务错误。
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, meta map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
		Meta:      mcp.Meta(meta),
	})
	if err != nil {
		t.Fatalf("%s 调用失败: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// TestFileToolsContractFace：11 个契约的名称 / schema / 描述齐备，且 MCP 工具面一致。
func TestFileToolsContractFace(t *testing.T) {
	tools := loadTools(t)
	if len(tools) != len(wantTools) {
		t.Fatalf("契约数=%d，期望 %d", len(tools), len(wantTools))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		seen[tool.Name] = true
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("%s: description 为空", tool.Name)
		}
		if got, _ := tool.Schema["type"].(string); got != "object" {
			t.Errorf("%s: schema.type=%v，期望 object", tool.Name, tool.Schema["type"])
		}
		if props, _ := tool.Schema["properties"].(map[string]any); props == nil {
			t.Errorf("%s: schema 缺 properties", tool.Name)
		}
	}
	for _, name := range wantTools {
		if !seen[name] {
			t.Errorf("缺少契约: %s", name)
		}
	}

	// MCP 工具面（宿主实际看到的名字集合）。
	cs := newMCPFace(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != len(wantTools) {
		t.Fatalf("MCP 工具数=%d，期望 %d", len(res.Tools), len(wantTools))
	}
	for _, t2 := range res.Tools {
		if !seen[t2.Name] {
			t.Errorf("MCP 暴露了未声明工具: %s", t2.Name)
		}
	}
}

// TestFileToolsMCPRoundTrip：写入 → 读取 → 查找 → 替换 → 复制 → 移动 → 差异 → 编码探测 → 删除。
func TestFileToolsMCPRoundTrip(t *testing.T) {
	cs := newMCPFace(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	text, isErr := callTool(t, cs, "file_write", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{
			map[string]any{"path": a, "content": "alpha\nneedle\n中文内容\n"},
			map[string]any{"path": b, "content": "beta\n"},
		},
	}, nil)
	if isErr {
		t.Fatalf("file_write: %s", text)
	}
	var wr struct {
		Summary struct {
			OK int `json:"ok"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(text), &wr); err != nil {
		t.Fatalf("file_write 结果非 JSON: %v (%s)", err, text)
	}
	if wr.Summary.OK != 2 {
		t.Fatalf("file_write ok=%d，期望 2（%s）", wr.Summary.OK, text)
	}

	text, isErr = callTool(t, cs, "file_read", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": a}},
	}, nil)
	if isErr {
		t.Fatalf("file_read: %s", text)
	}
	if !strings.Contains(text, "needle") || !strings.Contains(text, "中文内容") {
		t.Fatalf("file_read 内容不符: %s", text)
	}

	text, isErr = callTool(t, cs, "file_find", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"root": dir, "grep": "needle"}},
	}, nil)
	if isErr {
		t.Fatalf("file_find: %s", text)
	}
	if !strings.Contains(text, "a.txt") {
		t.Fatalf("file_find 未命中 a.txt: %s", text)
	}

	text, isErr = callTool(t, cs, "file_replace", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": a, "find": "needle", "replace": "replaced"}},
	}, nil)
	if isErr {
		t.Fatalf("file_replace: %s", text)
	}
	if raw, err := os.ReadFile(a); err != nil || !strings.Contains(string(raw), "replaced") {
		t.Fatalf("file_replace 未生效: %v (%s)", err, text)
	}

	c := filepath.Join(dir, "c.txt")
	d := filepath.Join(dir, "d.txt")
	if text, isErr = callTool(t, cs, "file_copy", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"from": a, "to": c}},
	}, nil); isErr {
		t.Fatalf("file_copy: %s", text)
	}
	if text, isErr = callTool(t, cs, "file_move", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"from": c, "to": d}},
	}, nil); isErr {
		t.Fatalf("file_move: %s", text)
	}
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		t.Fatalf("file_move 后源文件仍存在: %v", err)
	}
	if _, err := os.Stat(d); err != nil {
		t.Fatalf("file_move 后目标文件不存在: %v", err)
	}

	if text, isErr = callTool(t, cs, "file_diff", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"left": a, "right": d}},
	}, nil); isErr {
		t.Fatalf("file_diff: %s", text)
	}
	if text, isErr = callTool(t, cs, "file_detect", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": a}},
	}, nil); isErr {
		t.Fatalf("file_detect: %s", text)
	}

	if text, isErr = callTool(t, cs, "file_delete", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": b}, map[string]any{"path": d}},
	}, nil); isErr {
		t.Fatalf("file_delete: %s", text)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("file_delete 后文件仍存在: %v", err)
	}
}

// TestFileToolsInstanceRoot：`_meta.chonkpilot.instance_id` 决定 `!/` 的落地根。
func TestFileToolsInstanceRoot(t *testing.T) {
	cs := newMCPFace(t)
	const inst = "mcpface-test"
	meta := map[string]any{"chonkpilot": map[string]any{"instance_id": inst}}

	text, isErr := callTool(t, cs, "file_write", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": "!/mcp/meta.txt", "content": "instance-root\n"}},
	}, meta)
	if isErr {
		t.Fatalf("file_write(!/): %s", text)
	}
	root, err := paths.TempRootFor(inst)
	if err != nil {
		t.Fatalf("TempRootFor(%s): %v", inst, err)
	}
	got := filepath.Join(root, "mcp", "meta.txt")
	raw, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("读回 %s: %v（%s）", got, err, text)
	}
	if !strings.Contains(string(raw), "instance-root") {
		t.Fatalf("落盘内容不符: %q", string(raw))
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// 多实例：另一个 instance 的 `!/` 落到自己的根（互不覆盖）。
	const inst2 = "mcpface-test-2"
	meta2 := map[string]any{"chonkpilot": map[string]any{"instance_id": inst2}}
	if text, isErr = callTool(t, cs, "file_write", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": "!/mcp/meta2.txt", "content": "instance-2\n"}},
	}, meta2); isErr {
		t.Fatalf("file_write(!/, inst2): %s", text)
	}
	root2, err := paths.TempRootFor(inst2)
	if err != nil {
		t.Fatalf("TempRootFor(%s): %v", inst2, err)
	}
	if _, err := os.Stat(filepath.Join(root2, "mcp", "meta2.txt")); err != nil {
		t.Fatalf("inst2 落盘缺失: %v", err)
	}
	if root2 == root {
		t.Fatalf("两个 instance 的根相同: %s", root)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root2) })

	// 不带 _meta 的调用沿用「当前根」（最近一次 SetTempRoot = inst2，兼容旧单实例语义）。
	text, isErr = callTool(t, cs, "file_read", map[string]any{
		"purpose": "mcp 面测试",
		"targets": []any{map[string]any{"path": "!/mcp/meta2.txt"}},
	}, nil)
	if isErr {
		t.Fatalf("无 _meta 的 !/ 读取（应沿用当前根）: %s", text)
	}
	if !strings.Contains(text, "instance-2") {
		t.Fatalf("无 _meta 读取内容不符: %s", text)
	}
}
