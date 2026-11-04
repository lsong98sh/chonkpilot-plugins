// Package fileops 是 chonkpilot-mcp-tools 自持的 file 分类工具实现。
// 特性（与主仓库 executor 的差异）：
//   - 路径越界的强制拦截由**进程级沙箱**（gateway spawn 时施加）承担，本包不做应用层路径白名单；
//     文件操作参数须为绝对路径或以 ~/ 开头的用户目录路径（R-11，见 pathcheck.go），相对路径报错
//   - 支持 UTF-16（BOM 识别为文本并解码）
//   - file_find 递归遍历跳过 SkipDirs（含 .svn）
//   - 零持久状态（无 DB / 无版本快照）
package fileops

import (
	"encoding/base64"
	"strings"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"github.com/lsong98sh/chonkpilot-util/textx"
)

// ResolvePath 解析并强校验文件操作参数路径（R-11，file_read/file_find/file_diff/filesys_run
// 的路径收敛点）：必须为**绝对路径或以 ~/ 开头的用户目录路径**；相对路径一律拒绝
// （不再按 workDir 兜底）。空值返回 "path is required"（必填校验优先）。
//
// workDir 参数保留仅为兼容既有调用签名（当前恒为空），不再参与解析。
func ResolvePath(userPath, workDir string) (string, string) {
	if userPath == "" {
		return "", "path is required"
	}
	return ValidateFilePath(userPath)
}

// SkipDirs 是 FIND 递归遍历默认跳过的目录（newWalkFilter 使用）。
var SkipDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	"node_modules": true,
	".trae":        true,
	".chonkpilot":  true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	"build":        true,
	"dist":         true,
	".next":        true,
	".nuxt":        true,
}

// WalkFilter 是递归遍历的过滤配置（FIND 的 skip_dirs + ignore_files 与内置默认合并）。
type WalkFilter struct {
	SkipDirs    map[string]bool
	IgnoreFiles []string // glob 模式（支持逗号/竖线分隔），匹配文件名则跳过
}

// newWalkFilter 构造遍历过滤配置：
//   - skipDirs: 目录名，追加到内置默认集（.git/.svn/node_modules/...），不替换
//   - ignoreFiles: 文件 glob 模式（如 "*.log"），递归遍历时跳过
func newWalkFilter(skipDirs, ignoreFiles []string) *WalkFilter {
	f := &WalkFilter{SkipDirs: map[string]bool{}}
	for k := range SkipDirs {
		f.SkipDirs[k] = true
	}
	for _, s := range skipDirs {
		if s != "" {
			f.SkipDirs[s] = true
		}
	}
	for _, s := range ignoreFiles {
		if s != "" {
			f.IgnoreFiles = append(f.IgnoreFiles, s)
		}
	}
	return f
}

// SkipDir 判断目录名是否在跳过集。
func (f *WalkFilter) SkipDir(name string) bool { return f.SkipDirs[name] }

// IgnoreFile 判断文件名是否被忽略（glob 匹配）。
func (f *WalkFilter) IgnoreFile(name string) bool {
	if len(f.IgnoreFiles) == 0 {
		return false
	}
	return globMatch(name, strings.Join(f.IgnoreFiles, ","))
}

// HasUTF16BOM reports whether data starts with a UTF-16 LE/BE BOM.
func HasUTF16BOM(data []byte) bool {
	return textx.HasUTF16BOM(data)
}

// IsBinaryBytes 判定二进制：含 0x00 即二进制，但 UTF-16（BOM）是文本。
func IsBinaryBytes(data []byte) bool {
	return textx.IsBinary(data)
}

// DecodeText 按 BOM/UTF-8 自动解码文本；UTF-16 BOM 优先，其次 UTF-8 校验，
// 再依次尝试 GBK / Shift-JIS / Big5 / EUC-KR（薄封装 → chonkpilot-util/encodingx）。
func DecodeText(data []byte) string {
	s, _ := encodingx.DecodeAuto(data)
	return s
}

// encodeFor 返回把文本按 data 的原始编码回写的编码函数（nil = 原样 UTF-8 写回）。
// 识别顺序与 DecodeText 一致，保证「解码→修改→编码」往返不改变文件编码（R7-C-12）。
func encodeFor(data []byte) func(string) []byte {
	name := encodingx.AutoName(data)
	if name == "" {
		return nil
	}
	return func(s string) []byte {
		b, err := encodingx.Encode(name, s, true)
		if err != nil {
			return []byte(s)
		}
		return b
	}
}

// EncodingName 返回显示用编码名（UTF-16LE/BE / UTF-8 / binary）。
func EncodingName(data []byte) string {
	return encodingx.DisplayName(data)
}

// DataURI 将二进制编码为 data URI（base64）。
func DataURI(data []byte) string {
	return "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(data)
}
