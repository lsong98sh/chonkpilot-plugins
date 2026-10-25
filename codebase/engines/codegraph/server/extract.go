package server

import (
	"fmt"
	"sort"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// defKinds: 每种语言「节点类型 → 符号类别」
// nameKinds: 可充当"名字"的叶子节点类型（无 name 字段时回退查找）
// decisionKinds: 计入圈复杂度的分支/循环节点类型
var defKinds = map[string]map[string]string{
	"go":         {"function_declaration": "func", "method_declaration": "method", "type_declaration": "type", "type_spec": "type"},
	"javascript": {"function_declaration": "func", "generator_function_declaration": "func", "class_declaration": "class", "method_definition": "method"},
	"typescript": {"function_declaration": "func", "generator_function_declaration": "func", "class_declaration": "class", "abstract_class_declaration": "class", "method_definition": "method", "interface_declaration": "interface", "type_alias_declaration": "type", "enum_declaration": "enum"},
	"tsx":        {"function_declaration": "func", "generator_function_declaration": "func", "class_declaration": "class", "abstract_class_declaration": "class", "method_definition": "method", "interface_declaration": "interface", "type_alias_declaration": "type", "enum_declaration": "enum"},
	"python":     {"function_definition": "func", "class_definition": "class"},
	"rust":       {"function_item": "func", "struct_item": "type", "enum_item": "enum", "trait_item": "trait", "type_item": "type"},
	"java":       {"class_declaration": "class", "interface_declaration": "interface", "enum_declaration": "enum", "record_declaration": "class", "method_declaration": "method", "constructor_declaration": "constructor"},
}

var nameKinds = map[string]map[string]bool{
	"go":         {"identifier": true, "type_identifier": true},
	"javascript": {"identifier": true, "property_identifier": true, "type_identifier": true},
	"typescript": {"identifier": true, "property_identifier": true, "type_identifier": true},
	"tsx":        {"identifier": true, "property_identifier": true, "type_identifier": true},
	"python":     {"identifier": true},
	"rust":       {"identifier": true, "type_identifier": true},
	"java":       {"identifier": true},
}

var decisionKinds = map[string]map[string]bool{
	"go":         {"if_statement": true, "for_statement": true, "expression_switch_statement": true, "expression_case": true, "default_case": true, "type_switch_statement": true, "type_case": true, "select_statement": true, "communication_case": true},
	"javascript": {"if_statement": true, "for_statement": true, "for_in_statement": true, "while_statement": true, "do_statement": true, "switch_statement": true, "switch_case": true, "catch_clause": true, "ternary_expression": true},
	"typescript": {"if_statement": true, "for_statement": true, "for_in_statement": true, "while_statement": true, "do_statement": true, "switch_statement": true, "switch_case": true, "catch_clause": true, "ternary_expression": true},
	"tsx":        {"if_statement": true, "for_statement": true, "for_in_statement": true, "while_statement": true, "do_statement": true, "switch_statement": true, "switch_case": true, "catch_clause": true, "ternary_expression": true},
	"python":     {"if_statement": true, "elif_clause": true, "for_statement": true, "while_statement": true, "try_statement": true, "except_clause": true},
	"rust":       {"if_expression": true, "if_let_expression": true, "for_expression": true, "while_expression": true, "loop_expression": true, "match_expression": true, "match_arm": true},
	"java":       {"if_statement": true, "for_statement": true, "enhanced_for_statement": true, "while_statement": true, "do_statement": true, "switch_statement": true, "switch_block_statement_group": true, "catch_clause": true, "ternary_expression": true},
}

// importKinds: 产生依赖边的导入节点类型
var importKinds = map[string]map[string]bool{
	"go":         {"import_declaration": true},
	"javascript": {"import_statement": true, "export_statement": true},
	"typescript": {"import_statement": true, "export_statement": true},
	"tsx":        {"import_statement": true, "export_statement": true},
	"python":     {"import_statement": true, "import_from_statement": true},
	"rust":       {"use_declaration": true},
	"java":       {"import_declaration": true},
}

// callKinds: 产生「调用边」的节点类型（节点名与各 grammar 版本实测一致，见 §10 组 7 测试）。
var callKinds = map[string]map[string]bool{
	"go":         {"call_expression": true},
	"javascript": {"call_expression": true, "new_expression": true},
	"typescript": {"call_expression": true, "new_expression": true},
	"tsx":        {"call_expression": true, "new_expression": true},
	"python":     {"call": true},
	"rust":       {"call_expression": true, "macro_invocation": true},
	"java":       {"method_invocation": true, "object_creation_expression": true},
}

// calleeFields: 调用节点 → 承载"被调名"的字段名（取该字段子树再取最后一段）。
var calleeFields = map[string]map[string]string{
	"go":         {"call_expression": "function"},
	"javascript": {"call_expression": "function", "new_expression": "constructor"},
	"typescript": {"call_expression": "function", "new_expression": "constructor"},
	"tsx":        {"call_expression": "function", "new_expression": "constructor"},
	"python":     {"call": "function"},
	"rust":       {"call_expression": "function", "macro_invocation": "macro"},
	"java":       {"method_invocation": "name", "object_creation_expression": "type"},
}

// nestedScopeKinds: 非 defKinds 的匿名函数作用域——收集调用时同样剪枝（内层闭包的调用不计入外层）。
var nestedScopeKinds = map[string]map[string]bool{
	"go":         {"func_literal": true},
	"javascript": {"function_expression": true, "arrow_function": true, "generator_function": true},
	"typescript": {"function_expression": true, "arrow_function": true, "generator_function": true},
	"tsx":        {"function_expression": true, "arrow_function": true, "generator_function": true},
	"python":     {"lambda": true},
	"rust":       {"closure_expression": true},
	"java":       {"lambda_expression": true},
}

// parseFile 解析单个文件并提取符号/导入/复杂度。langName 必须是 langs 里的逻辑名。
func parseFile(langName, path string, src []byte) (*FileInfo, error) {
	spec, ok := extSpecFor(langName)
	if !ok {
		return nil, fmt.Errorf("unsupported lang %q", langName)
	}
	parser := ts.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(spec.make()); err != nil {
		return nil, fmt.Errorf("set language %s: %w", langName, err)
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, fmt.Errorf("parse returned nil tree")
	}
	defer tree.Close()

	fi := &FileInfo{Path: path, Lang: langName}
	root := tree.RootNode()
	fi.HasErr = root.HasError()

	dec := decisionKinds[langName]
	imports := importKinds[langName]
	df := defKinds[langName]
	nk := nameKinds[langName]

	var walk func(n *ts.Node)
	walk = func(n *ts.Node) {
		k := n.Kind()
		if imports[k] {
			fi.Imports = append(fi.Imports, extractImports(langName, n, src)...)
		}
		if sk, isDef := df[k]; isDef {
			name := defName(n, nk, src)
			if name != "" {
				kind := sk
				if sk == "func" && isInsideKind(langName, n) {
					kind = "method"
				}
				cc := 1
				var calls []string
				if kind == "func" || kind == "method" || kind == "constructor" {
					cc = complexityOf(dec, n)
					calls = callsWithin(langName, n, src)
				}
				fi.Symbols = append(fi.Symbols, Symbol{
					File: path, Kind: kind, Lang: langName, Name: name,
					Line: int(n.StartPosition().Row) + 1, EndLine: int(n.EndPosition().Row) + 1,
					Complexity: cc,
					Signature:  firstLine(src, n),
					Calls:      calls,
				})
			}
			// 类/结构体内的定义继续下钻（如 python 类内 def → method）
			walkChildren(n, walk)
			return
		}
		walkChildren(n, walk)
	}
	walk(root)
	return fi, nil
}

func walkChildren(n *ts.Node, walk func(*ts.Node)) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		walk(n.NamedChild(uint(i)))
	}
}

// isInsideKind 函数定义是否位于"使其成为方法"的容器内（python class / rust impl,trait）。
func isInsideKind(lang string, n *ts.Node) bool {
	var parents []string
	switch lang {
	case "python":
		parents = []string{"class_definition"}
	case "rust":
		parents = []string{"impl_item", "trait_item"}
	default:
		return false
	}
	for p := n.Parent(); p != nil; p = p.Parent() {
		for _, k := range parents {
			if p.Kind() == k {
				return true
			}
		}
	}
	return false
}

// defName 取定义节点名字：优先 field "name"，否则在 named children 中找名字类节点。
func defName(n *ts.Node, nk map[string]bool, src []byte) string {
	if c := n.ChildByFieldName("name"); c != nil {
		return strings.TrimSpace(nodeText(src, c))
	}
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(uint(i))
		if nk[c.Kind()] {
			return strings.TrimSpace(nodeText(src, c))
		}
	}
	return ""
}

func nodeText(src []byte, n *ts.Node) string {
	if src == nil || n == nil {
		return ""
	}
	b, e := int(n.StartByte()), int(n.EndByte())
	if b < 0 || b > len(src) || e < b || e > len(src) {
		return ""
	}
	return string(src[b:e])
}

// complexityOf 圈复杂度：1 + 子树内决策点计数（启发式，按语言决策节点集）。
func complexityOf(dec map[string]bool, n *ts.Node) int {
	count := 1
	var walk func(x *ts.Node)
	walk = func(x *ts.Node) {
		if x != n && dec[x.Kind()] {
			count++
		}
		walkChildren(x, walk)
	}
	walk(n)
	return count
}

// callsWithin 以定义节点 defNode 为根收集其子树内的被调名（取最后一段；去重 + 排序）。
// 剪枝：子树内再遇到定义节点（defKinds）或匿名函数作用域（nestedScopeKinds）即不进入——
// 内层函数/闭包的调用不计入外层符号（范式同 complexityOf 的子树遍历）。
func callsWithin(lang string, defNode *ts.Node, src []byte) []string {
	cks := callKinds[lang]
	if cks == nil || defNode == nil {
		return nil
	}
	df := defKinds[lang]
	ns := nestedScopeKinds[lang]
	set := map[string]bool{}
	var walk func(x *ts.Node)
	walk = func(x *ts.Node) {
		k := x.Kind()
		if cks[k] {
			if name := callCalleeName(lang, x, src); name != "" {
				set[name] = true
			}
		}
		if _, isDef := df[k]; isDef || ns[k] {
			return
		}
		walkChildren(x, walk)
	}
	walkChildren(defNode, walk)
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// callCalleeName 取一次调用的被调名（最后一段；去泛型实参/下标等噪声）。
func callCalleeName(lang string, call *ts.Node, src []byte) string {
	var callee *ts.Node
	if f := calleeFields[lang][call.Kind()]; f != "" {
		callee = call.ChildByFieldName(f)
	}
	if callee == nil { // 回退：首个 named child（各 grammar 中被调子树均先于实参列表）
		if call.NamedChildCount() > 0 {
			callee = call.NamedChild(0)
		}
	}
	return lastSegmentText(lang, callee, src)
}

// lastSegmentText 从被调子树取"最后一段"名字：
// 成员/选择/作用域表达式递归其「最后一段」字段；泛型/带参文本剥离噪声；无法静态确定的调用（下标）返回 ""。
func lastSegmentText(lang string, n *ts.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Kind() {
	case "selector_expression": // go: operand.field
		return lastSegmentText(lang, n.ChildByFieldName("field"), src)
	case "member_expression": // js/ts/tsx: object.property
		return lastSegmentText(lang, n.ChildByFieldName("property"), src)
	case "attribute": // python: value.attribute
		return lastSegmentText(lang, n.ChildByFieldName("attribute"), src)
	case "field_expression": // rust: value.field
		return lastSegmentText(lang, n.ChildByFieldName("field"), src)
	case "scoped_identifier": // rust: path::name
		return lastSegmentText(lang, n.ChildByFieldName("name"), src)
	case "field_access": // java: object.field
		return lastSegmentText(lang, n.ChildByFieldName("field"), src)
	case "generic_type": // java: Base<Args> → Base（去泛型实参）
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(uint(i))
			if c.Kind() != "type_arguments" {
				return lastSegmentText(lang, c, src)
			}
		}
		return ""
	case "scoped_type_identifier": // java: a.b.C → C
		return lastSegmentText(lang, lastNamed(n), src)
	case "parenthesized_expression": // (expr)()
		return lastSegmentText(lang, firstNamedOf(n), src)
	case "index_expression": // go：Foo[T](a,b) 泛型实例化 → 取 operand（Foo）；fns[0]() 下标 → 无法静态取名
		index := n.ChildByFieldName("index")
		if index == nil {
			index = lastNamed(n)
		}
		if index != nil && !isLiteralNode(index.Kind()) {
			operand := n.ChildByFieldName("operand")
			if operand == nil {
				operand = firstNamedOf(n)
			}
			return lastSegmentText(lang, operand, src)
		}
		return ""
	case "subscript_expression": // js/ts/py：a["x"]() / a[0]() → 无法静态取名
		return ""
	}
	t := strings.TrimSpace(nodeText(src, n))
	if i := strings.IndexAny(t, "[(<"); i > 0 {
		t = strings.TrimSpace(t[:i])
	}
	if j := strings.LastIndexByte(t, '.'); j >= 0 {
		t = strings.TrimSpace(t[j+1:])
	}
	return t
}

// lastNamed 最后一个 named child。
func lastNamed(n *ts.Node) *ts.Node {
	if n == nil || n.NamedChildCount() == 0 {
		return nil
	}
	return n.NamedChild(n.NamedChildCount() - 1)
}

// isLiteralNode 字面量节点（用于把 go index_expression 的「下标访问」与「泛型实例化」区分开）。
func isLiteralNode(kind string) bool {
	switch kind {
	case "int_literal", "float_literal", "imaginary_literal", "rune_literal",
		"interpreted_string_literal", "raw_string_literal", "true", "false", "nil":
		return true
	}
	return false
}

// firstNamedOf 第一个 named child。
func firstNamedOf(n *ts.Node) *ts.Node {
	if n == nil || n.NamedChildCount() == 0 {
		return nil
	}
	return n.NamedChild(0)
}

// firstLine 定义首行文本（签名预览，截断 200）。
func firstLine(src []byte, n *ts.Node) string {
	b := int(n.StartByte())
	if b < 0 || b > len(src) {
		return ""
	}
	rest := src[b:]
	i := strings.IndexByte(string(rest), '\n')
	if i >= 0 {
		rest = rest[:i]
	}
	s := strings.TrimSpace(string(rest))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// extractImports 按语言提取 import 原语目标（返回原始串，解析交给 resolveImport）。
func extractImports(lang string, n *ts.Node, src []byte) []string {
	var out []string
	switch lang {
	case "go":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			c := n.NamedChild(uint(i))
			if c.Kind() != "import_spec" {
				continue
			}
			p := c.ChildByFieldName("path")
			if p == nil {
				continue
			}
			t := unquote(strings.TrimSpace(nodeText(src, p)))
			if t != "" && t != "_" && t != "." {
				out = append(out, t)
			}
		}
	case "javascript", "typescript", "tsx":
		if srcN := n.ChildByFieldName("source"); srcN != nil {
			if t := unquote(strings.TrimSpace(nodeText(src, srcN))); t != "" {
				out = append(out, t)
			}
		}
	case "python":
		if n.Kind() == "import_statement" {
			for i := 0; i < int(n.NamedChildCount()); i++ {
				c := n.NamedChild(uint(i))
				if c.Kind() == "dotted_name" {
					out = append(out, strings.TrimSpace(nodeText(src, c)))
				} else if c.Kind() == "aliased_import" {
					if d := firstNamed(c, "dotted_name"); d != nil {
						out = append(out, strings.TrimSpace(nodeText(src, d)))
					}
				}
			}
		} else { // import_from_statement
			if m := n.ChildByFieldName("module"); m != nil {
				if t := strings.TrimSpace(nodeText(src, m)); t != "" {
					out = append(out, t)
				}
			} else if d := firstNamed(n, "dotted_name"); d != nil { // 部分 grammar 无 module 字段
				if t := strings.TrimSpace(nodeText(src, d)); t != "" {
					out = append(out, t)
				}
			} else if r := firstNamed(n, "relative_import"); r != nil {
				out = append(out, strings.TrimSpace(nodeText(src, r)))
			}
		}
	case "rust":
		t := strings.TrimSpace(nodeText(src, n))
		t = strings.TrimSpace(strings.TrimPrefix(t, "use"))
		t = strings.TrimSuffix(strings.TrimSpace(t), ";")
		t = strings.Join(strings.Fields(t), "")
		if t != "" && t != "crate" && t != "self" && t != "super" {
			out = append(out, t)
		}
	case "java":
		t := strings.TrimSpace(nodeText(src, n))
		t = strings.TrimPrefix(t, "import")
		t = strings.TrimPrefix(strings.TrimSpace(t), "static")
		t = strings.TrimSuffix(strings.TrimSpace(t), ";")
		t = strings.TrimSpace(t)
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 {
		switch s[0] {
		case '"', '\'', '`':
			return s[1 : len(s)-1]
		}
	}
	return s
}

func firstNamed(n *ts.Node, kind string) *ts.Node {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		if c := n.NamedChild(uint(i)); c.Kind() == kind {
			return c
		}
	}
	return nil
}

// extSpecFor 由逻辑名反查 spec。
func extSpecFor(name string) (langSpec, bool) {
	for _, l := range langs {
		if l.name == name {
			return l, true
		}
	}
	return langSpec{}, false
}
