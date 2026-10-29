// Package exedir 提供获取当前执行可执行文件所在目录的能力。
// 用途：chonkpilot-mcp-server 的 --root 缺省值（契约扫描根 = exe 所在目录）等。
package exedir

import (
	"os"
	"path/filepath"
)

// Dir 返回当前执行的可执行文件所在目录（绝对路径，Clean 后）。
// 失败（如 os.Executable 不可用）时返回 error。
func Dir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(filepath.Dir(exe))
	if err != nil {
		return "", err
	}
	return abs, nil
}
