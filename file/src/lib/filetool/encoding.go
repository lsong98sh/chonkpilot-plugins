// encoding.go — 文件域编码相关的**薄封装**（唯一实现 = chonkpilot-util/encodingx）。
//
// 保留原函数名，函数体统一委托 util，避免各仓各写一份「BOM 判定 / 候选打分 / GBK 解码」。
package filetool

import (
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

// detectEncoding 做四层判定中的前 3 层（BOM / 字符集分布 / 常用字覆盖）；
// 第 4 层（是否自然语言）留给调用方看 Sample 仲裁。
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
	cands := encodingx.Candidates(data)
	res.Candidates = cands
	if len(cands) > 0 {
		res.Encoding = cands[0].Encoding
		res.Confidence = encodingx.Confidence(cands)
		if s, err := encodingx.Decode(res.Encoding, data); err == nil {
			res.Sample = encodingx.Sample(s, 512)
		}
	}
	return res
}

// looksLikeUTF8 判断数据是否严格合法 UTF-8（供 write 的 auto 编码判断）。
func looksLikeUTF8(data []byte) bool {
	return encodingx.IsUTF8Text(data)
}

// encodeUTF16 按编码名（utf-16le / utf-16be）编码 UTF-16，并按需写入 BOM。
func encodeUTF16(s, name string, withBOM bool) ([]byte, error) {
	return encodingx.Encode(name, s, withBOM)
}
