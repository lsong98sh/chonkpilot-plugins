package fileops

import (
	"path/filepath"
	"strings"
)

// MatchGlob 是 globMatch 的导出包装（供 file_find 原子工具复用）。
func MatchGlob(name, pattern string) bool {
	return globMatch(name, pattern)
}

// globMatch 判断 name 是否匹配一个或多个逗号/竖线分隔的 glob。
func globMatch(name, pattern string) bool {
	if pattern == "" {
		return true
	}
	for _, p := range strings.FieldsFunc(pattern, func(r rune) bool { return r == ',' || r == '|' }) {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if matchGlob(p, name) {
			return true
		}
	}
	return false
}

// matchGlob 单模式匹配：不含 `**` → 直接 filepath.Match（保留其字符类 `[...]` 等完整语义）；
// 含 `**` → 走本实现（`**` 跨路径分隔符，`*`/`?` 不跨段；该分支不支持字符类）。
func matchGlob(pattern, name string) bool {
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	p := []rune(pattern)
	n := []rune(name)
	var match func(pi, ni int) bool
	match = func(pi, ni int) bool {
		for pi < len(p) {
			switch p[pi] {
			case '*':
				if pi+1 < len(p) && p[pi+1] == '*' {
					// `**`：匹配任意字符（含 '/'、'\\'），回溯尝试剩余位置
					pi += 2
					for k := ni; k <= len(n); k++ {
						if match(pi, k) {
							return true
						}
					}
					return false
				}
				// 单个 `*`：不跨路径分隔符
				pi++
				for {
					if match(pi, ni) {
						return true
					}
					if ni >= len(n) || isPathSep(n[ni]) {
						return false
					}
					ni++
				}
			case '?':
				if ni >= len(n) || isPathSep(n[ni]) {
					return false
				}
				pi++
				ni++
			default:
				if ni >= len(n) || n[ni] != p[pi] {
					return false
				}
				pi++
				ni++
			}
		}
		return ni == len(n)
	}
	return match(0, 0)
}

// isPathSep 报告 rune 是否为路径分隔符（'/' 或 '\\'）。
func isPathSep(r rune) bool { return r == '/' || r == '\\' }
