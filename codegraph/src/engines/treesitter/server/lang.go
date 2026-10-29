package server

import (
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
	tg "github.com/tree-sitter/tree-sitter-go/bindings/go"
	tj "github.com/tree-sitter/tree-sitter-java/bindings/go"
	tjs "github.com/tree-sitter/tree-sitter-javascript/bindings/go"
	tpy "github.com/tree-sitter/tree-sitter-python/bindings/go"
	trs "github.com/tree-sitter/tree-sitter-rust/bindings/go"
	tts "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// langSpec 一种受支持语言。
type langSpec struct {
	name string // 逻辑名（索引 lang 字段）
	exts []string
	make func() *ts.Language
}

var langs = []langSpec{
	{"go", []string{".go"}, func() *ts.Language { return ts.NewLanguage(tg.Language()) }},
	{"javascript", []string{".js", ".jsx", ".mjs", ".cjs"}, func() *ts.Language { return ts.NewLanguage(tjs.Language()) }},
	{"typescript", []string{".ts"}, func() *ts.Language { return ts.NewLanguage(tts.LanguageTypescript()) }},
	{"tsx", []string{".tsx"}, func() *ts.Language { return ts.NewLanguage(tts.LanguageTSX()) }},
	{"python", []string{".py", ".pyw"}, func() *ts.Language { return ts.NewLanguage(tpy.Language()) }},
	{"rust", []string{".rs"}, func() *ts.Language { return ts.NewLanguage(trs.Language()) }},
	{"java", []string{".java"}, func() *ts.Language { return ts.NewLanguage(tj.Language()) }},
}

var extLang = map[string]langSpec{}

func init() {
	for _, l := range langs {
		for _, e := range l.exts {
			extLang[e] = l
		}
	}
}

// LangForExt 返回扩展名（含点，小写）对应的语言；未知返回 ok=false。
func LangForExt(ext string) (string, bool) {
	l, ok := extLang[strings.ToLower(ext)]
	if !ok {
		return "", false
	}
	return l.name, true
}

// SupportedLangs 支持的扩展名集合（用于工具描述与探测）。
func SupportedLangs() []string {
	out := []string{}
	seen := map[string]bool{}
	for e, l := range extLang {
		if !seen[l.name] {
			seen[l.name] = true
			out = append(out, l.name+" ("+e+")")
		}
	}
	return out
}
