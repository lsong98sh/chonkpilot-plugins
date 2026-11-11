// encoding.go — 文件域编码相关的**薄封装**。
//
// 识别：BOM / 二进制（NUL 启发式）/ joshtechnologygroup/chardet（统计检测）。
// 解码/编码：唯一实现 = chonkpilot-util/encodingx（GBK/GB18030/Shift-JIS/Big5/EUC-KR/UTF-16）。
package filetool

import (
	"strings"

	"github.com/joshtechnologygroup/chardet"
	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"github.com/lsong98sh/chonkpilot-util/textx"
)

// detectResult 是一次编码检测的结果（file_detect 的对外结构，形状保持不变）。
type detectResult struct {
	Binary     bool
	BOM        string
	Encoding   string
	Confidence string
	Candidates []encodingx.Candidate
	Sample     string
}

// normalizeEncoding 归一化编码名（大小写/连字符/下划线/别名）。
func normalizeEncoding(name string) string {
	return encodingx.Normalize(name)
}

// decodeByName 按显式编码名解码。unknown 编码 → error。
func decodeByName(data []byte, name string) (string, error) {
	return encodingx.Decode(name, data)
}

// encodeByName 按显式编码名编码；utf-8 = 原样。
func encodeByName(s, name string) ([]byte, error) {
	return encodingx.Encode(name, s, false)
}

// chardetName 把 chardet 的编码名映射到本仓规范名；不支持的名字返回 ""。
func chardetName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ascii", "utf-8", "utf8", "utf-8-sig":
		return encodingx.UTF8
	case "utf-16le":
		return encodingx.UTF16LE
	case "utf-16be":
		return encodingx.UTF16BE
	case "gb2312", "gbk", "cp936":
		return encodingx.GBK
	case "gb18030":
		return encodingx.GB18030
	case "shift_jis", "shift-jis", "sjis", "cp932", "euc-jp":
		return encodingx.ShiftJIS
	case "big5", "cp950":
		return encodingx.Big5
	case "euc-kr", "cp949":
		return encodingx.EUCKR
	}
	return ""
}

// detectEncoding 做编码检测：BOM → 二进制 → chardet（多候选 + 置信度）。
// chardet 给出不支持/低置信结果时，回落到 encodingx 的候选打分。
func detectEncoding(data []byte) detectResult {
	res := detectResult{BOM: "none"}
	if name := encodingx.BOMName(data); name != "" {
		res.BOM = name
		res.Encoding = name
		res.Confidence = "high"
		switch name {
		case encodingx.UTF8:
			res.Sample = encodingx.Sample(string(data[3:]), 512)
		default:
			if s, err := encodingx.Decode(name, data); err == nil {
				res.Sample = encodingx.Sample(s, 512)
			}
		}
		return res
	}
	if textx.IsBinary(data) {
		res.Binary = true
		res.Encoding = "binary"
		res.Confidence = "high"
		return res
	}

	// 严格合法 UTF-8 直接高置信返回（chardet 在纯 ASCII/短文本上可能给 windows-1252）。
	if encodingx.IsUTF8Text(data) {
		res.Encoding = encodingx.UTF8
		res.Confidence = "high"
		res.Candidates = []encodingx.Candidate{{Encoding: encodingx.UTF8, Score: 1}}
		res.Sample = encodingx.Sample(string(data), 512)
		return res
	}

	// chardet：主结果 + 全部候选（过滤到本仓支持的解码器）。
	// chardet 对极短输入会 panic（实测 1 字节）；过短样本直接走旧候选打分。
	var top chardet.Result
	var all []chardet.Result
	if len(data) >= 8 {
		top, all = chardetSafe(data)
	}
	cands := make([]encodingx.Candidate, 0, 4)
	seen := map[string]bool{}
	for _, r := range all {
		name := chardetName(r.Encoding)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		cands = append(cands, encodingx.Candidate{Encoding: name, Score: r.Confidence})
	}
	if len(cands) > 0 {
		res.Candidates = cands
		res.Encoding = cands[0].Encoding
		res.Confidence = chardetConfidence(cands[0].Score)
		if s, err := encodingx.Decode(res.Encoding, data); err == nil {
			res.Sample = encodingx.Sample(s, 512)
		}
		return res
	}

	// chardet 未给出可用编码（如 windows-1252/ISO-8859）：回落旧候选打分。
	if name := chardetName(top.Encoding); name != "" {
		res.Encoding = name
		res.Confidence = chardetConfidence(top.Confidence)
		if s, err := encodingx.Decode(name, data); err == nil {
			res.Sample = encodingx.Sample(s, 512)
		}
		return res
	}
	cands = encodingx.Candidates(data)
	res.Candidates = cands
	if len(cands) > 0 {
		res.Encoding = cands[0].Encoding
		res.Confidence = encodingx.Confidence(cands)
		if s, err := encodingx.Decode(res.Encoding, data); err == nil {
			res.Sample = encodingx.Sample(s, 512)
		}
	} else {
		res.Confidence = "low"
	}
	return res
}

// chardetSafe 调用 chardet 并吞掉其 panic（极短/异常字节序列时库可能越界）。
func chardetSafe(data []byte) (top chardet.Result, all []chardet.Result) {
	defer func() {
		if recover() != nil {
			top, all = chardet.Result{}, nil
		}
	}()
	return chardet.Detect(data), chardet.DetectAll(data)
}

// chardetConfidence 把 chardet 的 0..1 置信度映射为 high/medium/low。
func chardetConfidence(v float64) string {
	switch {
	case v >= 0.8:
		return "high"
	case v >= 0.5:
		return "medium"
	default:
		return "low"
	}
}

// NormalizeEncoding 是插件配置面复用的编码名归一（导出给 file 插件配置校验）。
func NormalizeEncoding(name string) string { return normalizeEncoding(name) }

// SupportedEncoding 判断规范编码名是否可解码/编码（导出给配置校验）。
func SupportedEncoding(name string) bool { return supportedEncoding(name) }

// supportedEncoding 判断规范编码名是否可解码/编码。
func supportedEncoding(name string) bool {
	switch normalizeEncoding(name) {
	case encodingx.UTF8, encodingx.UTF16LE, encodingx.UTF16BE,
		encodingx.GBK, encodingx.GB18030, encodingx.ShiftJIS, encodingx.Big5, encodingx.EUCKR:
		return true
	}
	return false
}

// isANSIEncoding 判断是否为 Windows 中文 ANSI（GBK/GB18030）——这两类旧文件在配置了
// 默认字符集时可转换到默认字符集；Shift-JIS/Big5/EUC-KR 不在此列（保持原编码）。
func isANSIEncoding(name string) bool {
	switch normalizeEncoding(name) {
	case encodingx.GBK, encodingx.GB18030:
		return true
	}
	return false
}

// looksLikeANSI 判断非 UTF-8 数据是否像是 Windows 中文 ANSI（GBK/GB18030）文本。
// 用于「旧文件检测置信度低、但配置了默认字符集」时的安全兜底：只有按 GBK 解码可读且像中文才采纳。
func looksLikeANSI(data []byte) bool {
	if encodingx.BOMName(data) != "" || encodingx.IsUTF8Text(data) {
		return false
	}
	s, err := encodingx.Decode(encodingx.GBK, data)
	if err != nil || !encodingx.Visible(s) {
		return false
	}
	return encodingx.LooksLikeChinese(s)
}

// looksLikeUTF8 判断数据是否严格合法 UTF-8（供 write 的 auto 编码判断）。
func looksLikeUTF8(data []byte) bool {
	return encodingx.IsUTF8Text(data)
}

// encodeUTF16 按编码名（utf-16le / utf-16be）编码 UTF-16，并按需写入 BOM。
func encodeUTF16(s, name string, withBOM bool) ([]byte, error) {
	return encodingx.Encode(name, s, withBOM)
}
