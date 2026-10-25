// Package fileops 是 chonkpilot-mcp-tools 自持的 file 分类工具实现。
// 特性（与主仓库 executor 的差异）：
//   - 路径越界的强制拦截由**进程级沙箱**（gateway spawn 时施加）承担，本包不做应用层路径白名单；
//     文件操作参数须为绝对路径或以 ~/ 开头的用户目录路径（R-11，见 pathcheck.go），相对路径报错
//   - 支持 UTF-16（BOM 识别为文本并解码）
//   - file_find 递归遍历跳过 SkipDirs（含 .svn）
//   - 零持久状态（无 DB / 无版本快照）
package fileops

import (
	"bytes"
	"encoding/base64"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
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
	return len(data) >= 2 && ((data[0] == 0xFF && data[1] == 0xFE) || (data[0] == 0xFE && data[1] == 0xFF))
}

// IsBinaryBytes 判定二进制：含 0x00 即二进制，但 UTF-16（BOM）是文本。
func IsBinaryBytes(data []byte) bool {
	if HasUTF16BOM(data) {
		return false
	}
	checkLen := len(data)
	if checkLen > 8192 {
		checkLen = 8192
	}
	for _, b := range data[:checkLen] {
		if b == 0 {
			return true
		}
	}
	return false
}

// DecodeText 按 BOM/UTF-8 自动解码文本；UTF-16 BOM 优先，其次 UTF-8 校验，
// 再依次尝试 GBK / Shift-JIS / Big5 / EUC-KR。
func DecodeText(data []byte) string {
	if HasUTF16BOM(data) {
		dec := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder()
		if s, _, err := transform.String(dec, string(data)); err == nil {
			return s
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	for _, dec := range []transform.Transformer{
		simplifiedchinese.GBK.NewDecoder(),
		japanese.ShiftJIS.NewDecoder(),
		traditionalchinese.Big5.NewDecoder(),
		korean.EUCKR.NewDecoder(),
	} {
		s, _, err := transform.String(dec, string(data))
		if err == nil && hasVisible(s) {
			return s
		}
	}
	return string(data)
}

func hasVisible(s string) bool {
	return strings.Count(s, "\uFFFD") < len(s)/2
}

// encodeFor 返回把文本按 data 的原始编码回写的编码函数（nil = 原样 UTF-8 写回）。
// 与 DecodeText 的识别顺序一致，保证「解码→修改→编码」往返不改变文件编码，
// 避免 GBK/Shift-JIS/Big5/EUC-KR/UTF-16 文件被静默写成 UTF-8（R7-C-12）。
func encodeFor(data []byte) func(string) []byte {
	if HasUTF16BOM(data) {
		order := unicode.LittleEndian
		if data[0] == 0xFE && data[1] == 0xFF {
			order = unicode.BigEndian
		}
		enc := unicode.UTF16(order, unicode.UseBOM).NewEncoder()
		return func(s string) []byte { b, _, _ := transform.Bytes(enc, []byte(s)); return b }
	}
	if utf8.Valid(data) {
		return nil // 已是 UTF-8（或纯 ASCII）→ 原样写回
	}
	for _, c := range []struct {
		dec transform.Transformer
		enc transform.Transformer
	}{
		{simplifiedchinese.GBK.NewDecoder(), simplifiedchinese.GBK.NewEncoder()},
		{japanese.ShiftJIS.NewDecoder(), japanese.ShiftJIS.NewEncoder()},
		{traditionalchinese.Big5.NewDecoder(), traditionalchinese.Big5.NewEncoder()},
		{korean.EUCKR.NewDecoder(), korean.EUCKR.NewEncoder()},
	} {
		if s, _, err := transform.String(c.dec, string(data)); err == nil && hasVisible(s) {
			enc := c.enc
			return func(str string) []byte { b, _, _ := transform.Bytes(enc, []byte(str)); return b }
		}
	}
	return nil // 无法识别编码 → 保持既有 UTF-8 行为
}

// EncodingName 返回显示用编码名（UTF-16LE/BE / UTF-8 / GBK …）。
func EncodingName(data []byte) string {
	if HasUTF16BOM(data) {
		if data[0] == 0xFF && data[1] == 0xFE {
			return "UTF-16LE"
		}
		return "UTF-16BE"
	}
	if !bytes.Contains(data[:min(len(data), 1024)], []byte{0}) {
		return "UTF-8"
	}
	return "binary"
}

// DataURI 将二进制编码为 data URI（base64）。
func DataURI(data []byte) string {
	return "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(data)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
