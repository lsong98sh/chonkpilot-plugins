package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// syntheticContracts 为全部 hot 工具生成最小契约（用于注册面测试）。
func syntheticContracts() []ContractTool {
	out := make([]ContractTool, 0, len(toolSpecs))
	for _, spec := range toolSpecs {
		if !spec.Hot {
			continue
		}
		out = append(out, ContractTool{
			Name:        spec.Name,
			Description: "test",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"workdir": map[string]any{"type": "string"},
					"query":   map[string]any{"type": "string"},
				},
			},
			Meta: map[string]any{
				"hot":        true,
				"source":     "md",
				"async":      "auto",
				"timeout":    30,
				"on_timeout": "background",
			},
		})
	}
	return out
}

func newTestClient(t *testing.T) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	if err := RegisterToolsWithContract(srv, syntheticContracts()); err != nil {
		t.Fatalf("RegisterToolsWithContract: %v", err)
	}
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// toInt 兼容 JSON 往返后的 float64 与直接构造的 int。
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return -1
}

func callText(t *testing.T, cs *mcp.ClientSession, params *mcp.CallToolParams) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", params.Name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallTool(%s): 空结果", params.Name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("CallTool(%s): 非文本结果 %T", params.Name, res.Content[0])
	}
	return tc.Text, res.IsError
}

// TestContractMetaListed：注册面必须带上 md 的 _meta（source=md / async / timeout / on_timeout）。
func TestContractMetaListed(t *testing.T) {
	cs := newTestClient(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var found *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "codegraph_symbol_search" {
			found = tool
			break
		}
	}
	if found == nil {
		t.Fatal("tools/list 缺少 codegraph_symbol_search")
	}
	if found.Meta["source"] != "md" {
		t.Errorf("_meta.source = %v，want md", found.Meta["source"])
	}
	if found.Meta["hot"] != true {
		t.Errorf("_meta.hot = %v，want true", found.Meta["hot"])
	}
	if found.Meta["async"] != "auto" || found.Meta["on_timeout"] != "background" {
		t.Errorf("_meta 策略不完整: %v", found.Meta)
	}
	if toInt(found.Meta["timeout"]) != 30 {
		t.Errorf("_meta.timeout = %v，want 30", found.Meta["timeout"])
	}
}

// TestWorkdirFromMeta：未传 workdir 时从 _meta.chonkpilot.work_dir 注入，且查询不写盘。
func TestWorkdirFromMeta(t *testing.T) {
	cs := newTestClient(t)
	dir := t.TempDir()

	text, isErr := callText(t, cs, &mcp.CallToolParams{
		Name:      "codegraph_symbol_search",
		Arguments: map[string]any{"query": "x"},
		Meta:      mcp.Meta{"chonkpilot": map[string]any{"work_dir": dir}},
	})
	if isErr {
		t.Fatalf("未初始化查询不应是协议错误: %s", text)
	}
	if strings.Contains(text, "缺少 workdir") {
		t.Fatalf("workdir 未从 _meta 注入: %s", text)
	}
	if !strings.Contains(text, "not_initialized") {
		t.Errorf("期望 not_initialized，实际: %s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, ".chonkpilot")); !os.IsNotExist(err) {
		t.Errorf("查询路径不应创建 .chonkpilot（stat err=%v）", err)
	}
}

// TestWorkdirValidation：显式 workdir 必须绝对、非 ~，且不得越出 _meta 当前项目。
func TestWorkdirValidation(t *testing.T) {
	cs := newTestClient(t)
	dir := t.TempDir()

	cases := []struct {
		name    string
		args    map[string]any
		meta    mcp.Meta
		wantSub string
	}{
		{
			name:    "relative",
			args:    map[string]any{"query": "x", "workdir": "sub"},
			meta:    mcp.Meta{"chonkpilot": map[string]any{"work_dir": dir}},
			wantSub: "workdir 必须是绝对路径",
		},
		{
			name:    "home",
			args:    map[string]any{"query": "x", "workdir": "~/other"},
			meta:    mcp.Meta{"chonkpilot": map[string]any{"work_dir": dir}},
			wantSub: "不得使用 ~",
		},
		{
			name:    "outside",
			args:    map[string]any{"query": "x", "workdir": t.TempDir()},
			meta:    mcp.Meta{"chonkpilot": map[string]any{"work_dir": dir}},
			wantSub: "越出当前项目",
		},
		{
			name:    "missing-both",
			args:    map[string]any{"query": "x"},
			meta:    nil,
			wantSub: "缺少 workdir",
		},
		{
			name:    "inside",
			args:    map[string]any{"query": "x", "workdir": filepath.Join(dir, "sub")},
			meta:    mcp.Meta{"chonkpilot": map[string]any{"work_dir": dir}},
			wantSub: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, _ := callText(t, cs, &mcp.CallToolParams{
				Name:      "codegraph_symbol_search",
				Arguments: tc.args,
				Meta:      tc.meta,
			})
			if tc.wantSub == "" {
				if strings.Contains(text, "workdir") && strings.Contains(text, "必须是") {
					t.Fatalf("合法 workdir 被拒绝: %s", text)
				}
				return
			}
			if !strings.Contains(text, tc.wantSub) {
				t.Fatalf("期望错误包含 %q，实际: %s", tc.wantSub, text)
			}
		})
	}
}
