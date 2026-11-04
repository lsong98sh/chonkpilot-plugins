// charenc.go — 索引前的编码识别策略（解码实现 = chonkpilot-util/encodingx）。
//
// 背景（2026-10-31）：此前的索引链把原始字节直接当 UTF-8 交给 jieba/zvec，
// 导致 GBK/GB18030 中文文本切出乱码，UTF-16（BOM）文件被判二进制跳过。
// 现改为：读字节 → 二进制粗判（textx.IsBinary）→ 识别编码并统一转 UTF-8 → splitChunks → 分词。
//
// 本文件只保留**索引侧策略**（通用识别 / 解码 / 编码 / 常用字判据全部在 util `encodingx`）：
//  1. UTF-16 LE/BE BOM → 解码（BOM 不入文本）；
//  2. UTF-8 BOM → 剥离；
//  3. 严格合法 UTF-8 → 原样；
//  4. GB18030（覆盖 GBK / GB2312）→ 仅当解码结果「像中文」时采纳；
//  5. 其余遗留编码（Shift-JIS / Big5 / EUC-KR）→ 走 `encodingx.BestAuto` **高置信度**判定，
//     只有唯一高分（假名 / 谚文 / 繁体专用字信号）且与次名分差足够才采纳；
//  6. 以上都不成立 → 原样返回（保持历史行为：不因无法识别丢内容）。
//
// **为何默认不猜日 / 韩 / 繁**：这些编码与 GBK 字节区间高度重叠，低置信度猜测会造出
// 不存在的中文词、污染检索。`BestAuto` 的分差门槛即「宁可不识别」；需要确定性时由
// 项目级配置 `plugins.codegraph.vfts.encoding` 显式指定（见 2B §4.1）。
package server

import (
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
)

// decodeIndexText 识别 data 的编码并转成 UTF-8 文本；第二个返回值 = 识别到的编码名
// （"utf-8" 表示无需转码）。forced 非空 = 项目级配置显式指定的编码（优先于自动识别）。
func decodeIndexText(data []byte, forced string) (string, string) {
	if len(data) == 0 {
		return "", encodingx.UTF8
	}
	// 1/2) BOM：UTF-16 解码（内部剥 BOM）；UTF-8 剥掉 BOM 再进分词。
	if name := encodingx.BOMName(data); name != "" {
		if name == encodingx.UTF8 {
			return string(data[3:]), encodingx.UTF8
		}
		if s, err := encodingx.Decode(name, data); err == nil {
			return s, name
		}
		return string(data), encodingx.UTF8
	}
	// 3) 严格合法 UTF-8（绝大多数源码/文本落这里）。
	if utf8.Valid(data) {
		return string(data), encodingx.UTF8
	}
	// 4) 项目级显式配置（确定性优先；解码失败则回落自动识别）。
	if forced != "" && forced != encodingx.UTF8 {
		if s, err := encodingx.Decode(forced, data); err == nil {
			return s, forced
		}
	}
	// 5) 多语言遗留编码：仅高置信度采纳（D-73）。
	if c, ok := encodingx.BestAuto(data); ok && c.Encoding != encodingx.UTF8 {
		if s, err := encodingx.Decode(c.Encoding, data); err == nil {
			return s, c.Encoding
		}
	}
	// 6) GB18030（含 GBK/GB2312）：解码成功且文本像中文才采纳（中文判据在 util）。
	if s, err := encodingx.Decode(encodingx.GB18030, data); err == nil && encodingx.LooksLikeChinese(s) {
		return s, encodingx.GB18030
	}
	// 7) 无法识别：保持原样。
	return string(data), encodingx.UTF8
}

// normalizeEncodingName 归一项目级编码配置：只接受 encodingx 支持的编码名；
// 空 / auto / 未知名 → ""（= 自动识别，宁可不猜）。
func normalizeEncodingName(name string) string {
	switch n := encodingx.Normalize(name); n {
	case encodingx.UTF8, encodingx.UTF16LE, encodingx.UTF16BE, encodingx.GBK,
		encodingx.GB18030, encodingx.ShiftJIS, encodingx.Big5, encodingx.EUCKR:
		return n
	}
	return ""
}
