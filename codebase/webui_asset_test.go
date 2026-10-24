package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sourceWebUIDir 是 webui 唯一源目录（PLUGIN-UI-1 第二段：`src/initdata/.../codebase/webui`
// 已删除，`src/plugins/codebase/webui` 为单源；embed 副本必须与之逐字节一致）。
var sourceWebUIDir = "webui"

// TestUIAssetGetMatchesSource：config.html / status.html 的 embed 字节与源文件逐字节一致。
func TestUIAssetGetMatchesSource(t *testing.T) {
	for _, name := range []string{"config.html", "status.html"} {
		want, err := os.ReadFile(filepath.Join(sourceWebUIDir, name))
		if err != nil {
			t.Fatalf("读取源文件 %s: %v", name, err)
		}
		got, err := uiAssetGet(context.Background(), map[string]any{"path": name})
		if err != nil {
			t.Fatalf("uiAssetGet(%s): %v", name, err)
		}
		m, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("uiAssetGet(%s) 返回类型 %T", name, got)
		}
		data, _ := m["data"].([]byte)
		if !bytes.Equal(data, want) {
			t.Fatalf("%s 字节与源文件不一致：got %d bytes, want %d bytes", name, len(data), len(want))
		}
		if size, _ := m["size"].(int); size != len(want) {
			t.Fatalf("%s size=%d want %d", name, size, len(want))
		}
	}
}

// TestUIAssetGetWebuiPrefix：manifest 的 ui.views[].entry 带 `webui/` 前缀，也可直接请求。
func TestUIAssetGetWebuiPrefix(t *testing.T) {
	got, err := uiAssetGet(context.Background(), map[string]any{"path": "webui/config.html"})
	if err != nil {
		t.Fatalf("uiAssetGet(webui/config.html): %v", err)
	}
	data, _ := got.(map[string]any)["data"].([]byte)
	if len(data) == 0 {
		t.Fatal("webui/config.html 返回空")
	}
}

// TestUIAssetGetRejectsTraversal：绝对路径 / 反斜杠 / 盘符 / `..` / 空路径 → invalid_path。
func TestUIAssetGetRejectsTraversal(t *testing.T) {
	for _, p := range []string{
		"",
		"   ",
		"../plugin.json",
		"webui/../plugin.json",
		`webui\config.html`,
		"/etc/passwd",
		"C:/windows/win.ini",
		"webui//config.html",
		"./config.html",
	} {
		_, err := uiAssetGet(context.Background(), map[string]any{"path": p})
		if err == nil || !strings.Contains(err.Error(), "invalid_path") {
			t.Fatalf("path=%q 应返回 invalid_path，实际 err=%v", p, err)
		}
	}
}

// TestUIAssetGetNotFound：webui 根内不存在 → not_found。
func TestUIAssetGetNotFound(t *testing.T) {
	_, err := uiAssetGet(context.Background(), map[string]any{"path": "nope.html"})
	if err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("未命中资源应返回 not_found，实际 err=%v", err)
	}
}
