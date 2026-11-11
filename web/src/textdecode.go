// textdecode.go —— web_download 内容模式的文本判定 / charset 解码（P1，2026-11-08）。
//
// 规则：
//   - 有 BOM（UTF-8 / UTF-16LE / UTF-16BE）→ 按 BOM 解码并去掉 BOM；
//   - Content-Type 有 charset → 按 charset 解码（GBK/GB2312/GB18030、UTF-16、Big5、
//     Shift_JIS、EUC-JP、EUC-KR、ISO-8859-1/Windows-1252 等）；
//   - charset 缺省或 UTF-8/US-ASCII → 合法 UTF-8 即文本；
//   - 其余（未知 charset、解码失败、真正二进制）→ 返回 ok=false，调用方退回 base64。
package main

import (
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
)

// decodeContent 尝试把响应字节解码为 UTF-8 文本。
// ok=false 表示应按二进制处理（base64）。
func decodeContent(data []byte, contentType string) (text, enc string, ok bool) {
	if len(data) == 0 {
		return "", "utf-8", true
	}

	charset := ""
	hasType := false
	textual := true
	if mt, params, err := mime.ParseMediaType(contentType); err == nil {
		hasType = true
		mt = strings.ToLower(strings.TrimSpace(mt))
		charset = strings.ToLower(strings.TrimSpace(params["charset"]))
		textual = isTextualMediaType(mt)
	}
	// 明确的二进制 Content-Type 且未声明 charset → 直接 base64（避免 BOM/启发式误判）。
	if hasType && charset == "" && !textual {
		return "", "", false
	}

	// 1) BOM 优先（比 Content-Type 更可靠）。
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return string(data[3:]), "utf-8", true
	}
	if len(data) >= 2 {
		if data[0] == 0xFF && data[1] == 0xFE {
			dec := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
			out, err := dec.Bytes(data[2:])
			if err == nil {
				return string(out), "utf-16le", true
			}
			return "", "", false
		}
		if data[0] == 0xFE && data[1] == 0xFF {
			dec := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM).NewDecoder()
			out, err := dec.Bytes(data[2:])
			if err == nil {
				return string(out), "utf-16be", true
			}
			return "", "", false
		}
	}

	encObj, canonical := encodingFor(charset)
	if encObj == nil {
		// 无 charset / UTF-8：合法 UTF-8 即文本，否则按二进制。
		if utf8.Valid(data) {
			return string(data), "utf-8", true
		}
		return "", "", false
	}
	out, err := encObj.NewDecoder().Bytes(data)
	if err != nil {
		return "", "", false
	}
	return string(out), canonical, true
}

// isTextualMediaType 判断 Content-Type 是否为可解码文本（无 charset 时的兜底判定）。
func isTextualMediaType(mt string) bool {
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	switch mt {
	case "application/json", "application/ld+json", "application/xml", "application/xhtml+xml",
		"application/javascript", "application/x-javascript", "application/ecmascript",
		"application/x-www-form-urlencoded", "application/sql", "application/yaml",
		"application/x-yaml", "application/rtf", "application/csv":
		return true
	}
	return strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml")
}

// encodingFor 返回 charset 对应的解码器与规范名；空 / utf-8 返回 nil（走 UTF-8 直通）。
func encodingFor(charset string) (encoding.Encoding, string) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return nil, ""
	case "utf-16", "utf16":
		return unicode.UTF16(unicode.LittleEndian, unicode.ExpectBOM), "utf-16"
	case "utf-16le", "utf16le":
		return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), "utf-16le"
	case "utf-16be", "utf16be":
		return unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), "utf-16be"
	case "gbk", "x-gbk", "gb2312", "cp936", "windows-936":
		return simplifiedchinese.GBK, "gbk"
	case "gb18030":
		return simplifiedchinese.GB18030, "gb18030"
	case "big5", "big-5", "cp950":
		return traditionalchinese.Big5, "big5"
	case "shift_jis", "shift-jis", "sjis", "windows-31j", "cp932":
		return japanese.ShiftJIS, "shift_jis"
	case "euc-jp":
		return japanese.EUCJP, "euc-jp"
	case "euc-kr", "cp949":
		return korean.EUCKR, "euc-kr"
	case "iso-8859-1", "latin1", "latin-1":
		return charmap.ISO8859_1, "iso-8859-1"
	case "windows-1252", "cp1252":
		return charmap.Windows1252, "windows-1252"
	}
	return nil, ""
}
