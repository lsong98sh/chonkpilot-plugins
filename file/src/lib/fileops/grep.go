package fileops

import (
	"github.com/lsong98sh/chonkpilot-util/glob"
)

// MatchGlob 是 globMatch 的导出包装（供 file_find 原子工具复用）。
func MatchGlob(name, pattern string) bool {
	return glob.MatchCSV(name, pattern)
}

// globMatch 判断 name 是否匹配一个或多个逗号/竖线分隔的 glob。
func globMatch(name, pattern string) bool {
	return glob.MatchCSV(name, pattern)
}
