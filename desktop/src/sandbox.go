// sandbox.go —— screenshot 落盘路径的沙箱校验。
//
// 语义（与 gateway/宿主一致）：
//   - `cx.Sandbox == nil` 或 `enabled=false` → 不做限制；
//   - `enabled=true` → 路径必须落在 `writable_dirs` 内（递归包含），否则明确报错且不落盘。
//
// 相对路径在插件侧先按当前调用上下文的 work_dir / data_dir 解析为绝对路径，
// 再交给引擎落盘，保证「校验的位置 = 实际写入的位置」。
package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/pathx"
)

// resolveSandboxPath 规整 screenshot 落盘路径并按沙箱白名单校验。
// 返回空串表示未给路径（由引擎报 path is required）。
func resolveSandboxPath(cx pluginsdk.CallContext, raw string) (string, error) {
	p := pathx.ExpandHome(strings.TrimSpace(raw))
	if p == "" {
		return "", nil
	}
	base := cx.WorkDir
	if base == "" {
		base = cx.DataDir
	}
	if !filepath.IsAbs(p) && base != "" {
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)

	sb := cx.Sandbox
	if sb == nil || !sb.Enabled {
		return p, nil
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("sandbox_denied: screenshot path must be absolute or resolvable within work_dir: %q", raw)
	}
	for _, dir := range sb.WritableDirs {
		d := pathx.ExpandHome(strings.TrimSpace(dir))
		if d == "" {
			continue
		}
		if !filepath.IsAbs(d) {
			if base == "" {
				continue
			}
			d = filepath.Join(base, d)
		}
		if pathWithin(p, filepath.Clean(d)) {
			return p, nil
		}
	}
	return "", fmt.Errorf("sandbox_denied: screenshot path outside writable_dirs: %s", p)
}

// pathWithin 判断 path 是否等于 dir 或位于 dir 之下（词法判断，不跟随符号链接）。
func pathWithin(path, dir string) bool {
	if equalPath(path, dir) {
		return true
	}
	prefix := strings.TrimRight(dir, `\/`) + string(filepath.Separator)
	if len(path) <= len(prefix) {
		return false
	}
	return equalPath(path[:len(prefix)], prefix)
}

// equalPath 在 Windows 上大小写不敏感，其余平台按字面比较。
func equalPath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
