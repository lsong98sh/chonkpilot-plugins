package server

import (
	"path/filepath"
	"strings"
)

// 索引内一律使用 '/' 分隔、相对 workdir 的路径（store 可移植）；对外返回时拼回绝对路径。

// relOf 把绝对路径转为相对 base 的 '/' 规范路径。
func relOf(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(r)
}

// cleanPath 规整 './'、'../' 与空段（基于字符串，不访问磁盘）。
func cleanPath(p string) string {
	segs := strings.Split(p, "/")
	var out []string
	for _, s := range segs {
		switch s {
		case "", ".":
			continue
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	return strings.Join(out, "/")
}

func joinPath(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "/" + b
}
