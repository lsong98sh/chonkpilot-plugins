// expression.go —— mcp_match / mcp_find 本地回退的检索表达式引擎（纯函数，可单测）。
//
// 表达式语法（搜索引擎式）：
//
//	张三 李四 -"王五"        → 三个词级条件：张三 OR 李四，且命中「王五」的条目被剔除
//	"代码审查" -read        → 短语「代码审查」精确匹配；命中 read 的条目被剔除
//
// 规则：
//   - 多个正向词默认 **OR**（任一命中即保留）；全部正向条件都不命中 → 剔除；
//   - `-"短语"` / `-词` 排除：命中任一排除项 → 剔除；
//   - 引号短语 = 精确匹配（原文一段，含空格），权重高于散词；
//   - 大小写不敏感；匹配面 = name + description；
//   - 打分：命中名称 2 分 / 命中描述 1 分（按词累加），短语 ×2；同分保持宿主顺序。
package main

import (
	"sort"
	"strings"
)

// exprTerm 是表达式里的一个条件词。
type exprTerm struct {
	text    string // 小写原文（短语含空格）
	negated bool   // - 前缀
	phrase  bool   // 引号包裹
}

// parseExpression 解析检索表达式。空串 → nil（= 不过滤）。
func parseExpression(raw string) []exprTerm {
	var terms []exprTerm
	i := 0
	for i < len(raw) {
		c := raw[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			// 短语：到下一个引号（无闭合则到串尾）。
			j := i + 1
			for j < len(raw) && raw[j] != '"' {
				j++
			}
			if t := strings.ToLower(strings.TrimSpace(raw[i+1 : j])); t != "" {
				terms = append(terms, exprTerm{text: t, phrase: true})
			}
			i = j + 1
		case c == '-':
			// 排除前缀：紧随的词/短语为排除项（`-` 后无内容则忽略）。
			j := i + 1
			if j < len(raw) && raw[j] == '"' {
				k := j + 1
				for k < len(raw) && raw[k] != '"' {
					k++
				}
				if t := strings.ToLower(strings.TrimSpace(raw[j+1 : k])); t != "" {
					terms = append(terms, exprTerm{text: t, negated: true, phrase: true})
				}
				i = k + 1
				continue
			}
			k := j
			for k < len(raw) && raw[k] != ' ' && raw[k] != '\t' && raw[k] != '\n' && raw[k] != '\r' {
				k++
			}
			if t := strings.ToLower(raw[j:k]); t != "" {
				terms = append(terms, exprTerm{text: t, negated: true})
			}
			i = k
		default:
			j := i
			for j < len(raw) && raw[j] != ' ' && raw[j] != '\t' && raw[j] != '\n' && raw[j] != '\r' {
				j++
			}
			if t := strings.ToLower(raw[i:j]); t != "" {
				terms = append(terms, exprTerm{text: t})
			}
			i = j
		}
	}
	return terms
}

// exprScore 计算条目与表达式的匹配分；负数 = 被排除项命中（调用方剔除）。
// 无正向条件（只有排除）→ 剩余条目只要不被排除即保留（0 分）。
func exprScore(e entry, terms []exprTerm) int {
	name := strings.ToLower(e.Name)
	desc := strings.ToLower(e.Description)
	score := 0
	hasPositive := false
	for _, t := range terms {
		inName := strings.Contains(name, t.text)
		inDesc := strings.Contains(desc, t.text)
		hit := inName || inDesc
		if t.negated {
			if hit {
				return -1 // 命中排除项 → 剔除
			}
			continue
		}
		hasPositive = true
		if !hit {
			continue // OR：未命中这个词不计分
		}
		weight := 1
		if inName {
			weight = 2
		}
		if t.phrase {
			weight *= 2
		}
		score += weight
	}
	if hasPositive && score == 0 {
		return -2 // 有正向条件但一个都没命中 → 剔除
	}
	return score
}

// filterByExpression 按表达式过滤并按匹配分稳定降序重排（同分保持宿主顺序）。
// terms 为 nil / 空 = 不过滤不重排。
func filterByExpression(list []entry, terms []exprTerm) []entry {
	if len(terms) == 0 {
		return list
	}
	out := make([]entry, 0, len(list))
	for _, e := range list {
		if s := exprScore(e, terms); s >= 0 {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return exprScore(out[i], terms) > exprScore(out[j], terms)
	})
	return out
}
