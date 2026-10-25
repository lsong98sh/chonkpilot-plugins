package server

import (
	"strings"
)

// defaultExts 默认参与索引的扩展名（含点、小写）：源码 + 纯文本（.txt / .md）。
// 可用 vfts_configure / vfts_index 的 exts 参数整体替换。
//
// 注意：**文档类扩展名（docExts）不在本集合内** —— 它们由独立的 `docs` 开关驱动，
// 走「文档转换服务」抽取文本通道（见 docs.go）；若并入本集合，用户自定义 vfts.exts
// 就会丢掉文档支持。故两者严格分组。
func defaultExts() []string {
	return []string{
		// Go
		".go",
		// JS / TS / Vue
		".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".vue",
		// Python
		".py", ".pyw",
		// Rust / Java / Kotlin / C 系 / C# / PHP / Ruby
		".rs", ".java", ".kt", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".php", ".rb",
		// 脚本 / 查询 / 标记
		".sh", ".ps1", ".bat", ".sql", ".html", ".css", ".scss",
		".json", ".yaml", ".yml", ".toml", ".xml",
		// 纯文本
		".txt", ".md",
	}
}

// docExts 文档类扩展名（Office / PDF）——**独立一组**，仅在配置开启 `docs` 时参与收集，
// 且必须先经「文档转换服务」（mcps/markitdown）抽取为文本（不读原文当文本）。
func docExts() []string {
	return []string{".docx", ".xlsx", ".pptx", ".pdf"}
}

// isDocExt 是否文档类扩展名（大小写不敏感）。
func isDocExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".docx", ".xlsx", ".pptx", ".pdf":
		return true
	}
	return false
}

// newExtSet 扩展名集合（小写归一）。
func newExtSet(exts []string) map[string]bool {
	m := make(map[string]bool, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		m[e] = true
	}
	return m
}

// isBinary 粗判二进制：前 8KB 出现 NUL 字节即视为二进制（跳过不索引）。
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8192 {
		n = 8192
	}
	for i := 0; i < n; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

// normalizeText 统一换行为 '\n'（避免 CRLF 影响行号与片段）。
func normalizeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}
