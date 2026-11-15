package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// webuiFS 是插件自持的静态资源（PLUGIN-UI-1）：打进 exe，宿主不再直读磁盘。
//
//go:embed all:webui
var webuiFS embed.FS

const uiAssetRoot = "webui"

// uiAssetGet 实现 `plugin/v1/ui.asset.get`：入参 `{path}`，返回 `{data, size}`。
// 越界路径 → invalid_path；未命中 → not_found（宿主按 404 处理）。
func uiAssetGet(_ context.Context, payload map[string]any) (any, error) {
	raw, _ := payload["path"].(string)
	rel, ok := normalizeUIAssetPath(raw)
	if !ok {
		return nil, fmt.Errorf("invalid_path: %q", raw)
	}
	b, err := fs.ReadFile(webuiFS, uiAssetRoot+"/"+rel)
	if err != nil {
		return nil, fmt.Errorf("not_found: %s", rel)
	}
	return map[string]any{"data": b, "size": len(b)}, nil
}

func normalizeUIAssetPath(p string) (string, bool) {
	s := strings.TrimSpace(p)
	if s == "" || strings.ContainsAny(s, `\:`) || strings.HasPrefix(s, "/") {
		return "", false
	}
	s = strings.TrimPrefix(s, uiAssetRoot+"/")
	if s == "" || s == "." || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "", false
	}
	clean := path.Clean(s)
	if clean != s || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}
