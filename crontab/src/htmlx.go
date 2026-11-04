package main

import (
	"html"
	"strings"
)

// HTMLToText 把一段 HTML 降级为系统通知可渲染的纯文本：
// 块级标签换行、<br> 换行、其余标签剥离、实体解码、空白折叠。
//
// 系统 Toast 不支持富文本，故 notify 的 param（HTML）在弹之前统一走本函数。
func HTMLToText(src string) string {
	s := strings.TrimSpace(src)
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '<' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 { // 未闭合 '<' → 按字面保留
			b.WriteString(s[i:])
			break
		}
		tag := strings.ToLower(strings.TrimSpace(s[i+1 : i+j]))
		name := tag
		if k := strings.IndexAny(name, " \t\r\n/"); k >= 0 {
			name = name[:k]
		}
		switch name {
		case "br", "p", "div", "li", "tr", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre":
			b.WriteByte('\n')
		}
		i += j + 1
	}
	text := html.UnescapeString(b.String())
	text = strings.ReplaceAll(text, "\u00a0", " ")
	text = strings.ReplaceAll(text, "\r\n", "\n")

	// 折叠空白：行内连续空格压成一个，连续空行压成一个。
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln == "" {
			if blank || len(out) == 0 {
				continue
			}
			blank = true
			out = append(out, "")
			continue
		}
		blank = false
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
