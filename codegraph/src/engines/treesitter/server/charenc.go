// charenc.go — 源码文件交给 tree-sitter 前的编码归一（统一转 UTF-8）。
//
// 背景（2026-10-31）：此前把文件原始字节直接交给 tree-sitter：GBK/GB18030 中文注释会被当成
// 非法 UTF-8（产生错误节点、影响符号提取），UTF-16（BOM）文件更是完全解析不出来。
// 现改为在 `parseEntry` 读字节后先归一为 UTF-8，再交给 `parseFile`。
//
// 解码/识别实现 = **chonkpilot-util/encodingx（单一实现，与 file 插件、vfts 引擎共用）**。
//
// 策略说明：这里用 `DecodeAuto`（UTF-16 BOM → 严格 UTF-8 → GBK / Shift-JIS / Big5 / EUC-KR
// 首个可读候选），比 vfts 的「只认中文 GB18030 + 常用字判据」更积极——因为源码的 ASCII 结构
// （关键字 / 括号 / 缩进）在各类兼容编码下字节一致，解码只影响注释、字符串与非 ASCII 标识符；
// 而把非法 UTF-8 交给 tree-sitter 会直接丢符号，代价更大。
package server

import "github.com/lsong98sh/chonkpilot-util/encodingx"

// indexSource 把源码原始字节归一为 UTF-8：UTF-8 BOM 剥离；UTF-16 BOM / 遗留编码按
// encodingx 识别解码；无法识别时原样返回（保持既有行为，不丢内容）。
func indexSource(raw []byte) []byte {
	if encodingx.BOMName(raw) == encodingx.UTF8 {
		return raw[3:]
	}
	text, _ := encodingx.DecodeAuto(raw)
	return []byte(text)
}
