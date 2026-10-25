// encoding.go — 文件域显式编解码与编码候选打分。
//
// 解码优先复用 fileops.DecodeText 的识别链；本文件补齐：
//   - 按显式编码名解码 / 编码（file_iconv / file_write 用）
//   - 候选编码打分（file_detect 用；最终仲裁交给调用方 LLM，工具不擅自决定）
package filetool

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
)

// encEntry 是一个受支持的编码。
type encEntry struct {
	name string
	enc  encoding.Encoding
}

// supportedEncodings 是本工具支持的编码表（顺序 = 检测优先级）。
var supportedEncodings = []encEntry{
	{"utf-8", nil}, // nil = 原样 UTF-8
	{"gbk", simplifiedchinese.GBK},
	{"gb18030", simplifiedchinese.GB18030},
	{"shift_jis", japanese.ShiftJIS},
	{"big5", traditionalchinese.Big5},
	{"euc-kr", korean.EUCKR},
}

// unicodeLE / unicodeBE 是 UTF-16 字节序别名。
var (
	unicodeLE = xunicode.LittleEndian
	unicodeBE = xunicode.BigEndian
)

// normalizeEncoding 归一化编码名（大小写/连字符/下划线/别名）。
func normalizeEncoding(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "_", "-")
	switch s {
	case "", "auto":
		return "auto"
	case "utf8", "utf-8":
		return "utf-8"
	case "utf16", "utf16le", "utf-16le", "utf-16-le":
		return "utf-16le"
	case "utf16be", "utf-16be", "utf-16-be":
		return "utf-16be"
	case "gbk", "cp936":
		return "gbk"
	case "gb18030":
		return "gb18030"
	case "shift-jis", "shiftjis", "sjis", "cp932":
		return "shift_jis"
	case "big5", "cp950":
		return "big5"
	case "euckr", "euc-kr":
		return "euc-kr"
	}
	return s
}

// utf16OrderOf 返回数据 BOM 的字节序（非 UTF-16 BOM → 返回 false）。
func utf16OrderOf(data []byte) (xunicode.Endianness, bool) {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		return xunicode.LittleEndian, true
	}
	if len(data) >= 2 && data[0] == 0xFE && data[1] == 0xFF {
		return xunicode.BigEndian, true
	}
	return xunicode.LittleEndian, false
}

// decodeByName 按显式编码名解码。unknown 编码 → error。
func decodeByName(data []byte, name string) (string, error) {
	switch normalizeEncoding(name) {
	case "utf-8":
		// 容忍非法字节：按 UTF-8 原样返回（替换符由调用方观察）
		return string(data), nil
	case "utf-16le", "utf-16be":
		order := unicodeLE
		if normalizeEncoding(name) == "utf-16be" {
			order = unicodeBE
		}
		dec := xunicode.UTF16(order, xunicode.IgnoreBOM).NewDecoder()
		s, _, err := transform.String(dec, string(stripUTF16BOM(data)))
		return s, err
	}
	for _, e := range supportedEncodings {
		if e.name != normalizeEncoding(name) || e.enc == nil {
			continue
		}
		s, _, err := transform.String(e.enc.NewDecoder(), string(data))
		return s, err
	}
	return "", &unknownEncodingError{name: name}
}

// unknownEncodingError 表示不支持的编码名。
type unknownEncodingError struct{ name string }

func (e *unknownEncodingError) Error() string { return "unsupported encoding: " + e.name }

// encodeByName 按显式编码名编码；utf-8 = 原样。
func encodeByName(s, name string) ([]byte, error) {
	switch normalizeEncoding(name) {
	case "utf-8":
		return []byte(s), nil
	case "utf-16le", "utf-16be":
		order := unicodeLE
		if normalizeEncoding(name) == "utf-16be" {
			order = unicodeBE
		}
		enc := xunicode.UTF16(order, xunicode.UseBOM).NewEncoder()
		b, _, err := transform.Bytes(enc, []byte(s))
		return b, err
	}
	for _, e := range supportedEncodings {
		if e.name != normalizeEncoding(name) || e.enc == nil {
			continue
		}
		b, _, err := transform.Bytes(e.enc.NewEncoder(), []byte(s))
		return b, err
	}
	return nil, &unknownEncodingError{name: name}
}

// stripUTF16BOM 去掉 UTF-16 BOM（解码时 IgnoreBOM 已处理，这里防双 BOM）。
func stripUTF16BOM(data []byte) []byte {
	if len(data) >= 2 && ((data[0] == 0xFF && data[1] == 0xFE) || (data[0] == 0xFE && data[1] == 0xFF)) {
		return data[2:]
	}
	return data
}

// candidate 是一个编码候选及其得分。
type candidate struct {
	Encoding string  `json:"encoding"`
	Score    float64 `json:"score"`
}

// detectResult 是一次编码检测的结果。
type detectResult struct {
	Binary     bool
	BOM        string
	Encoding   string
	Confidence string
	Candidates []candidate
	Sample     string
}

// detectEncoding 做四层判定中的前 3 层（BOM / 字符集分布 / 常用字覆盖）；
// 第 4 层（是否自然语言）留给调用方看 Sample 仲裁。
func detectEncoding(data []byte) detectResult {
	res := detectResult{BOM: "none"}

	// 第 1 层：硬门槛（BOM / 二进制）
	if order, ok := utf16OrderOf(data); ok {
		name := "utf-16le"
		if order == unicodeBE {
			name = "utf-16be"
		}
		res.BOM = name
		res.Encoding = name
		res.Confidence = "high"
		s, _ := decodeByName(data, name)
		res.Sample = sampleOf(s, 512)
		return res
	}
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		res.BOM = "utf-8"
		res.Encoding = "utf-8"
		res.Confidence = "high"
		res.Sample = sampleOf(string(data[3:]), 512)
		return res
	}
	if fileops.IsBinaryBytes(data) {
		res.Binary = true
		res.Encoding = "binary"
		res.Confidence = "high"
		return res
	}

	// 第 2/3 层：候选打分
	cands := []candidate{}
	for _, e := range supportedEncodings {
		var s string
		var ok bool
		if e.enc == nil {
			ok = utf8.Valid(data)
			s = string(data)
		} else {
			decoded, _, err := transform.String(e.enc.NewDecoder(), string(data))
			if err != nil {
				continue
			}
			s, ok = decoded, true
		}
		if !ok {
			continue
		}
		cands = append(cands, candidate{Encoding: e.name, Score: scoreDecoded(s)})
	}
	sortCandidates(cands)
	res.Candidates = cands
	if len(cands) > 0 {
		res.Encoding = cands[0].Encoding
		res.Confidence = confidenceOf(cands)
	}
	if res.Encoding != "" {
		if s, err := decodeByName(data, res.Encoding); err == nil {
			res.Sample = sampleOf(s, 512)
		}
	}
	return res
}

// scoreDecoded 给解码结果打分：坏字符越少、常用字占比越高，分越高。
func scoreDecoded(s string) float64 {
	if s == "" {
		return 0
	}
	var total, bad, cjk, halfKana int
	for _, r := range s {
		total++
		switch {
		case r == unicode.ReplacementChar:
			bad++
		case unicode.Is(unicode.Han, r), unicode.Is(unicode.Hiragana, r),
			unicode.Is(unicode.Katakana, r), unicode.Is(unicode.Hangul, r):
			cjk++
		case r >= 0xFF61 && r <= 0xFF9F: // 半角片假名（Shift-JIS 的强特征）
			halfKana++
		}
	}
	if total == 0 {
		return 0
	}
	good := float64(total-bad) / float64(total)
	coverage := float64(cjk) / float64(total)
	score := good*0.5 + coverage*0.5
	// 半角片假名是 Shift-JIS 的强线索：小幅加权，但不绝对（避免误判）
	if halfKana > 0 {
		score += 0.05 * float64(halfKana) / float64(total)
	}
	if score > 1 {
		score = 1
	}
	return score
}

// confidenceOf 依据前两名分差给出置信度。
func confidenceOf(cands []candidate) string {
	if len(cands) == 0 {
		return "low"
	}
	if len(cands) == 1 {
		return "high"
	}
	delta := cands[0].Score - cands[1].Score
	switch {
	case delta >= 0.25:
		return "high"
	case delta >= 0.10:
		return "medium"
	default:
		return "low"
	}
}

// sortCandidates 按得分降序（同分按名字稳定排序）。
func sortCandidates(c []candidate) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0; j-- {
			if c[j-1].Score > c[j].Score || (c[j-1].Score == c[j].Score && c[j-1].Encoding <= c[j].Encoding) {
				break
			}
			c[j-1], c[j] = c[j], c[j-1]
		}
	}
}

// sampleOf 截取用于人工/LLM 判断的样本（按 rune 截断，避免切碎多字节字符）。
func sampleOf(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	rs := []rune(s)
	if len(rs) > maxRunes {
		rs = rs[:maxRunes]
	}
	return string(rs)
}

// looksLikeUTF8 判断数据是否严格合法 UTF-8（供 write 的 auto 编码判断）。
func looksLikeUTF8(data []byte) bool {
	return utf8.Valid(data) && !bytes.Contains(data, []byte{0})
}

// encodeUTF16 按字节序编码 UTF-16，并按需写入 BOM。
func encodeUTF16(s string, order xunicode.Endianness, withBOM bool) ([]byte, error) {
	policy := xunicode.IgnoreBOM
	if withBOM {
		policy = xunicode.UseBOM
	}
	enc := xunicode.UTF16(order, policy).NewEncoder()
	b, _, err := transform.Bytes(enc, []byte(s))
	return b, err
}
