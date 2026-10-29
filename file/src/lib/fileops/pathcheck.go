// pathcheck.go — 文件操作参数路径强校验（决策 R-11，薄封装）。
//
// 唯一实现已上移到 chonkpilot-lib/paths（paths.ResolvePath / paths.InvalidPathMessage）：
// 工具/DSL 的文件与目录参数必须为**绝对路径**、**~/ 开头的用户目录**或 **!/ 开头的临时目录**；
// 相对路径一律拒绝。规范见 docs/spec/10-architecture/16-路径解析规范.md。
//
// 本文件仅保留 mcp-tools 侧的调用别名，避免各 handler 大面积改名。
package fileops

import (
	"errors"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/paths"
)

// ValidateFilePath 强校验文件操作参数路径（R-11）：转发 paths.ResolvePath(raw, "")。
// 返回 (解析后的绝对/展开路径, 统一错误消息)；空值返回 ("", "")（必填校验优先）。
func ValidateFilePath(raw string) (string, string) {
	return paths.ResolvePath(raw, "")
}

// InvalidPathMessage 构造路径不合规的统一错误消息（转发 lib 单一实现）。
func InvalidPathMessage(raw string) string {
	return paths.InvalidPathMessage(raw)
}

// ValidateField 校验带位置的路径参数；不合规时返回「位置：消息」，合规返回空串。
// label 用于指明是哪一个参数（如 files[0].path、md5["x"]、DSL 的 RPL 等）。
func ValidateField(label, raw string) string {
	if _, msg := ValidateFilePath(raw); msg != "" {
		return label + "：" + msg
	}
	return ""
}

// ResolveLocalPath 解析运行时本地落盘/句柄路径（R-11，含 !/ 临时目录与 ~ 展开）；
// 相对路径返回统一错误。供 DSL 文件系统句柄与落盘动作复用。
func ResolveLocalPath(p string) (string, error) {
	abs, msg := ValidateFilePath(p)
	if msg != "" {
		return "", errors.New(msg)
	}
	if abs == "" {
		return "", errors.New(InvalidPathMessage(p))
	}
	return abs, nil
}
