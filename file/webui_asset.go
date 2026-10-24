package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// webuiFS 是插件自持的静态资源（PLUGIN-UI-1）：插件把自己的 webui 打进 exe，
// 宿主不再直读磁盘 `capability/plugins/<id>/webui`（见 docs/spec 28-plugins §3A）。
//
// 源目录 = `src/plugins/file/webui`（**唯一源**；出厂 `src/initdata/capability/plugins/file/`
// 只保留 plugin.json，不再携带 webui 副本）。
//
//go:embed all:webui
var webuiFS embed.FS

// uiAssetRoot 是 embed 内的资源根目录名。
const uiAssetRoot = "webui"

// uiAssetGet 实现宿主资源能力 `plugin/v1/ui.asset.get`：入参 `{path}`，返回 `{data, size}`。
//
// MIME 不由插件决定（宿主按扩展名推断）。越界路径 → invalid_path；未命中 → not_found
// （宿主对本段两类错误一视同仁按 404 处理）。
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

// normalizeUIAssetPath 把宿主传入的路径归一为 webui 根内的相对路径：
//   - 允许 manifest 的 `ui.views[].entry` 形态（带 `webui/` 前缀）与裸相对路径；
//   - 拒绝绝对路径 / 盘符 / 反斜杠 / `..` / 空路径（越界一律拒绝，不做 Clean 兜底）。
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
