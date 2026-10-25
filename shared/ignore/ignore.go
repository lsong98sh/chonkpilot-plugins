// Package ignore 实现 gitignore(5) 语义的路径排除匹配。
//
// 单一实现，供 codegraph / vfts 两个索引引擎与 vfts 插件的清单扫描共用——
// 三处过滤口径必然一致（引擎排除集与插件 file_list 清单不会出现两套判定）。
//
// 规则来源与优先级（低 → 高，最后一条匹配的规则决定，「!」= 取消忽略）：
//
//	内置强制排除（BuiltinRules，不可被任何 '!' 反选）
//	→ 默认排除（DefaultRules，可被 '!' 反选）
//	→ 全局 ignore（GlobalRules，仅 XDG 默认位置）
//	→ .git/info/exclude（ExcludeRules）
//	→ 各级 .gitignore（目录越深优先级越高，同目录内行序在后覆盖在前）
//	→ 用户输入（Options.UserRules，最高优先级，等价 git 命令行 --exclude）
//
// 目录被忽略 = 不下降（父目录命中忽略规则即 SkipDir，其子级 '!' 规则无法救回），
// 与 git 一致；文件命中即跳过（在扩展名判定之前）。
//
// 已知与 git 的差异：
//   - 不查 git 索引：已跟踪文件照样受忽略规则约束；
//   - 不解析 core.excludesFile（仅 XDG 默认位置）；
//   - 不要求 workdir 是 git 仓库（勾选即生效）。
package ignore

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// builtinPatterns 内置强制排除（不可反选）：VCS 元数据 + ChonkPilot 自身索引目录。
// 反选它们会导致索引 VCS 内部对象或索引自身产物（自指反馈）。
var builtinPatterns = []string{
	".git/", ".svn/", ".hg/", ".chonkpilot/",
}

// defaultPatterns 默认排除（可被用户 '!' 反选）。
var defaultPatterns = []string{
	"node_modules/", "__pycache__/", ".venv/", "venv/", ".trae/",
	"dist/", "build/", ".next/", ".nuxt/", "out/", "target/", "vendor/",
}

// Rule 单条编译后的 gitignore 规则（不可变）。
type Rule struct {
	negate  bool           // '!' 取反（取消忽略）
	dirOnly bool           // 尾 '/'：仅匹配目录
	base    string         // 规则来源目录（相对 root，'/' 分隔，"" = root）
	re      *regexp.Regexp // 相对 base 的匹配式（已含锚定与 glob 展开）
}

// Options 遍历排除配置。
type Options struct {
	// StackGitignore 是否叠加 gitignore 体系规则（全局 ignore / .git/info/exclude / 各级 .gitignore）。
	// false = 仅「内置强制排除 + 默认排除 + 用户输入」。
	StackGitignore bool
	// UserRules 用户输入的规则（设置页「排除的目录和文件」，每项一条 gitignore 规则；最高优先级）。
	UserRules []string
	// Root 是规则根目录（Clean 后使用）：Ignored / ConfigOptions 据此逐级读取 .gitignore。
	// WalkDir 的根以其入参为准，不使用本字段（两者共用同一 ruleSet 求值，语义一致）。
	Root string
	// rs 是惰性构造并缓存的规则求值上下文：一次请求内多次 Ignored 复用同一份
	// （逐级 .gitignore 只解析一次），使批量判定为 O(paths)。
	// 非并发安全：同一 Options 的 Ignored 只在单 goroutine 内调用（WalkDir 各自新建，不共享）。
	rs *ruleSet
}

// Keys 是一次「配置 → 排除规则」读取所用的**显式键名**（由调用方给出）。
//
// 语义（PLUGIN-UI-1 第三段起键命名不再由本库硬编码：插件配置已迁入
// `plugins.<id>.*` 命名空间，本库只按调用方给的键取值）：
//
//	Enabled        == "true" → enabled
//	SkipDirs                 → UserRules（逗号/分号/换行分隔，保序不去重，空串 = 无规则）
//	StackGitignore == "true" → StackGitignore（false → 不读任何 ignore 文件）
//
// 空键名 = 该项按缺省（enabled=false / 无规则 / 不叠加）。
type Keys struct {
	Enabled        string
	SkipDirs       string
	StackGitignore string
}

// ConfigOptions 由「配置读取器」构造遍历排除配置（键名由 keys 指定）：
//
//	get 返回 (值, 是否命中)；workdir 为规则根。仅当 workdir 为空（配置错误）→ 返回 err
//
// （opts / enabled 仍按已读到的配置填充）；workdir 不存在不报错（逐级 .gitignore 读取本就
// 静默容忍缺失，判定自然降级为「不排除」）。
func ConfigOptions(keys Keys, get func(key string) (string, bool), workdir string) (*Options, bool, error) {
	enabled := false
	if keys.Enabled != "" {
		if v, ok := get(keys.Enabled); ok {
			enabled = v == "true"
		}
	}
	opts := &Options{Root: filepath.Clean(workdir)}
	if keys.SkipDirs != "" {
		if v, ok := get(keys.SkipDirs); ok {
			opts.UserRules = splitRules(v)
		}
	}
	if keys.StackGitignore != "" {
		if v, ok := get(keys.StackGitignore); ok {
			opts.StackGitignore = v == "true"
		}
	}
	if strings.TrimSpace(workdir) == "" {
		return opts, enabled, errors.New("ignore: empty workdir")
	}
	return opts, enabled, nil
}

// splitRules 解析用户排除规则文本：按逗号/分号/换行分隔，去空白；
// **保序且保留重复项**（gitignore 语义下顺序有意义：'!' 取反 + 后一条覆盖前一条）。空串 → nil。
func splitRules(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

// WalkDir 深度优先遍历 root：跳过被忽略的目录（不下降）与文件，其余交给 fn
// （语义同 filepath.WalkDir / fs.WalkDirFunc）。
// root 自身恒不被忽略（始终以 rel="" 调用 fn）。
func WalkDir(root string, opts Options, fn fs.WalkDirFunc) error {
	rootAbs := filepath.Clean(root)
	rs := newRuleSet(rootAbs, opts)
	return filepath.WalkDir(rootAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fn(p, d, err)
		}
		rel := relOf(rootAbs, p)
		if rel != "" && rs.decide(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir // 目录被忽略 = 不下降
			}
			return nil
		}
		return fn(p, d, err)
	})
}

// Ignored 判定 rel（相对 o.Root，'/' 分隔）是否被排除：
// = **该路径自身命中**（按库内既有有序求值口径，'!' 反选在自身层级仍生效）
// **或任一祖先目录按「目录剪枝」命中**（git 语义：父目录被排除 → 其后代也不会被索引；
// 祖先剪枝不可被后代 '!' 救回）。根自身恒不排除。
//
// 与 WalkDir 严格同源：两者共用 ruleSet.decide，故 Ignored(p) ⇔ WalkDir 会跳过 p。
// 路径的目录性取磁盘真实类型（不存在 → 按文件；仅影响尾斜杠目录规则的匹配）。
func (o *Options) Ignored(rel string) bool {
	rel = normalizeRel(rel)
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	root := filepath.Clean(o.Root)
	rs := o.rules()
	if rs == nil {
		return false
	}
	isDir := false
	if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
		isDir = fi.IsDir()
	}
	for _, anc := range ancestorDirs(rel) {
		if rs.decide(anc, true) {
			return true // 祖先目录被剪枝
		}
	}
	return rs.decide(rel, isDir)
}

// rules 返回（惰性构造并缓存的）规则求值上下文；Root 为空 → nil。
func (o *Options) rules() *ruleSet {
	if strings.TrimSpace(o.Root) == "" {
		return nil
	}
	if o.rs == nil || o.rs.root != filepath.Clean(o.Root) {
		o.rs = newRuleSet(filepath.Clean(o.Root), *o)
	}
	return o.rs
}

// ruleSet 是 WalkDir 与 Ignored 共用的规则求值上下文（同一份实现保证剪枝语义严格一致）。
type ruleSet struct {
	stack  bool
	root   string
	forced []Rule            // 内置强制（不可反选）
	base   []Rule            // 默认排除 + 全局 ignore + info/exclude（低 → 高优先级）
	user   []Rule            // 用户规则（最高优先级）
	own    map[string][]Rule // 各目录 .gitignore 规则（rel 目录 → 规则；惰性加载）
	loaded map[string]bool   // own 已加载标记（含空规则 → 不重复读盘）
}

// newRuleSet 组装规则集：低 → 高 = 内置强制 → 默认排除 → 全局 ignore → info/exclude
// → 各级 .gitignore（规则集内以 base 目录区分优先级）→ 用户规则。
func newRuleSet(root string, opts Options) *ruleSet {
	base := DefaultRules()
	if opts.StackGitignore {
		// 全局 ignore 与 info/exclude 位于默认排除**之后**（优先级更高），但仍在各级 .gitignore 之前。
		base = append(base, GlobalRules()...)
		base = append(base, ExcludeRules(root)...)
	}
	return &ruleSet{
		stack:  opts.StackGitignore,
		root:   root,
		forced: BuiltinRules(),
		base:   base,
		user:   Parse(strings.Join(opts.UserRules, "\n"), ""),
		own:    map[string][]Rule{},
		loaded: map[string]bool{},
	}
}

// loadOwn 惰性读取目录 dirRel 的 .gitignore（stack 关闭 / 已加载 → 跳过）。
func (rs *ruleSet) loadOwn(dirRel string) {
	if !rs.stack || rs.loaded[dirRel] {
		return
	}
	rs.loaded[dirRel] = true
	dir := rs.root
	if dirRel != "" {
		dir = filepath.Join(rs.root, filepath.FromSlash(dirRel))
	}
	if r := FileRules(filepath.Join(dir, gitignoreName), dirRel); len(r) > 0 {
		rs.own[dirRel] = r
	}
}

// decide 判定 rel（相对 root，'/' 分隔）是否被忽略：从最高优先级来源反向扫描，首个匹配者决定。
func (rs *ruleSet) decide(rel string, isDir bool) bool {
	if matchesAny(rs.forced, rel, isDir) {
		return true // 强制排除：不可被 '!' 反选
	}
	if m, ok := lastMatch(rs.user, rel, isDir); ok {
		return m
	}
	for dir := dirOf(rel); ; dir = dirOf(dir) {
		rs.loadOwn(dir)
		if m, ok := lastMatch(rs.own[dir], rel, isDir); ok {
			return m
		}
		if dir == "" {
			break
		}
	}
	m, _ := lastMatch(rs.base, rel, isDir)
	return m
}

// Match 判定 rel（相对 root，'/' 分隔）是否被忽略。
// forced 命中即忽略（不可反选）；否则 ordered 按「低 → 高」有序求值，最后一条匹配的规则决定。
// 供单测与外部按源分组的判定复用；WalkDir 内部按逐级目录分组，语义与之一致。
func Match(forced, ordered []Rule, rel string, isDir bool) bool {
	if matchesAny(forced, rel, isDir) {
		return true
	}
	m, _ := lastMatch(ordered, rel, isDir)
	return m
}

// Parse 解析一段 gitignore 语法文本为规则列表（保序；空行/注释/无效行丢弃）。
// base = 规则来源目录相对 root 的 '/' 路径（"" = root）——含 '/' 的规则相对 base 锚定。
func Parse(content, base string) []Rule {
	var out []Rule
	for _, line := range strings.Split(content, "\n") {
		if r, ok := parseLine(line, base); ok {
			out = append(out, r)
		}
	}
	return out
}

// FileRules 读取 path 并按 gitignore 语法解析（base 语义同 Parse）。
// 文件不存在/不可读 → nil（静默，不中断索引）。
func FileRules(path, base string) []Rule {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return Parse(string(b), base)
}

// BuiltinRules 内置强制排除规则（恒先生效，不可被 '!' 反选）。
func BuiltinRules() []Rule { return parsePatterns(builtinPatterns) }

// DefaultRules 默认排除规则（可被用户 '!' 反选）。
func DefaultRules() []Rule { return parsePatterns(defaultPatterns) }

// GlobalRules 全局 ignore 规则：$XDG_CONFIG_HOME/git/ignore 或 ~/.config/git/ignore（缺失 → nil）。
// 刻意不解析 ~/.gitconfig 的 core.excludesFile（已知差异，见包注释）。
func GlobalRules() []Rule {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return nil
		}
		dir = filepath.Join(home, ".config")
	}
	return FileRules(filepath.Join(dir, "git", "ignore"), "")
}

// ExcludeRules <root>/.git/info/exclude 规则（缺失 → nil）。
func ExcludeRules(root string) []Rule {
	return FileRules(filepath.Join(root, ".git", "info", "exclude"), "")
}

// ─── 内部 ────────────────────────────────────────────────

const gitignoreName = ".gitignore"

func parsePatterns(patterns []string) []Rule {
	out := make([]Rule, 0, len(patterns))
	for _, p := range patterns {
		if r, ok := parseLine(p, ""); ok {
			out = append(out, r)
		}
	}
	return out
}

// parseLine 解析单行 gitignore 规则；非规则行（空行/注释/无效）→ ok=false。
func parseLine(raw, base string) (Rule, bool) {
	line := trimTrailingSpace(strings.TrimSuffix(raw, "\r"))
	if line == "" {
		return Rule{}, false
	}
	if strings.HasPrefix(line, `\#`) {
		line = line[1:] // 转义 '#' = 字面 '#'
	} else if strings.HasPrefix(line, "#") {
		return Rule{}, false // 注释
	}
	negate := false
	if strings.HasPrefix(line, `\!`) {
		line = line[1:] // 转义 '!' = 字面 '!'
	} else if strings.HasPrefix(line, "!") {
		negate = true
		line = line[1:]
	}
	dirOnly := false
	if strings.HasSuffix(line, "/") {
		dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return Rule{}, false
	}
	anchored := false
	if strings.HasPrefix(line, "/") {
		line = strings.TrimPrefix(line, "/")
		anchored = true
	}
	if line == "" {
		return Rule{}, false
	}
	if strings.Contains(line, "/") {
		anchored = true // 含 '/'（非仅尾斜杠）= 相对 base 锚定
	}
	return Rule{negate: negate, dirOnly: dirOnly, base: base, re: compile(line, anchored)}, true
}

// trimTrailingSpace 去掉行尾未被 '\' 转义的空白（' ' / '\t'）；`\ ` 保留为字面空格。
func trimTrailingSpace(s string) string {
	i := len(s)
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
		bs := 0
		for j := i - 2; j >= 0 && s[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 1 {
			break // 被转义 → 字面空白
		}
		i--
	}
	return s[:i]
}

// compile 把（去锚点/去目录标记后的）pattern 编译为相对 base 的匹配式。
// anchored=false → 可匹配任意层级（前缀 (?:[^/]+/)*）。
func compile(pattern string, anchored bool) *regexp.Regexp {
	parts := strings.Split(pattern, "/")
	var b strings.Builder
	b.WriteString(`\A`)
	if !anchored {
		b.WriteString(`(?:[^/]+/)*`)
	}
	for i, p := range parts {
		if i > 0 && parts[i-1] != "**" {
			b.WriteString("/")
		}
		if p == "**" {
			if len(parts) == 1 || i == len(parts)-1 {
				b.WriteString(`.*`) // 尾随 '**'（或整条 '**'）：匹配其下全部
			} else {
				b.WriteString(`(?:[^/]+/)*`) // 前导/中间 '**'：匹配零或多个目录层
			}
			continue
		}
		b.WriteString(globSegment(p))
	}
	b.WriteString(`\z`)
	return regexp.MustCompile(b.String())
}

// globSegment 把单个路径段（不含 '/'）的 glob 转为正则片段：
// '*' / '?' 不跨 '/'；'[...]' 字符类（'!' / '^' 取反）；'\x' 转义为字面 x。
func globSegment(seg string) string {
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch c := seg[i]; c {
		case '\\':
			if i+1 < len(seg) {
				b.WriteString(regexp.QuoteMeta(string(seg[i+1])))
				i++
			} else {
				b.WriteString(`\\`)
			}
		case '*':
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			j := i + 1
			if j < len(seg) && (seg[j] == '!' || seg[j] == '^') {
				j++
			}
			if j < len(seg) && seg[j] == ']' {
				j++
			}
			for j < len(seg) && seg[j] != ']' {
				j++
			}
			if j >= len(seg) { // 未闭合 → 字面 '['
				b.WriteString(`\[`)
				break
			}
			cls := seg[i+1 : j]
			if strings.HasPrefix(cls, "!") {
				cls = "^" + cls[1:]
			}
			cls = strings.ReplaceAll(cls, `\`, `\\`)
			b.WriteString("[")
			b.WriteString(cls)
			b.WriteString("]")
			i = j
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// match 判定单条规则是否匹配 rel（相对 root，'/' 分隔）。
func (r Rule) match(rel string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	sub := rel
	if r.base != "" {
		if !strings.HasPrefix(rel, r.base+"/") {
			return false // 规则不适用于其来源目录本身及外部路径
		}
		sub = rel[len(r.base)+1:]
	}
	return r.re.MatchString(sub)
}

// matchesAny forced 规则中任一命中即忽略（不可反选）。
func matchesAny(rules []Rule, rel string, isDir bool) bool {
	for i := range rules {
		if rules[i].match(rel, isDir) {
			return true
		}
	}
	return false
}

// lastMatch 从后往前取首个命中规则（= 有序列表中最后一条匹配）：
// 命中负向规则 → 忽略；命中 '!' 规则 → 不忽略；无命中 → (false, false)。
func lastMatch(rules []Rule, rel string, isDir bool) (ignored, matched bool) {
	for i := len(rules) - 1; i >= 0; i-- {
		if rules[i].match(rel, isDir) {
			return !rules[i].negate, true
		}
	}
	return false, false
}

// relOf root → p 的相对路径（'/' 分隔；root 自身 → ""）。
func relOf(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return ""
	}
	r = filepath.ToSlash(r)
	if r == "." {
		return ""
	}
	return r
}

// dirOf rel 的父目录（'/' 分隔；顶层 → ""）。
func dirOf(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return ""
}

// normalizeRel 归一「相对 root」路径：'\\' → '/'，去首 '/' 与 './'，折叠 '.' 段与多余分隔；
// "."（= root 自身）→ ""。
func normalizeRel(rel string) string {
	rel = strings.TrimSpace(rel)
	rel = strings.ReplaceAll(rel, "\\", "/")
	rel = path.Clean(strings.TrimPrefix(rel, "/"))
	if rel == "." {
		return ""
	}
	return rel
}

// ancestorDirs 返回 rel 的全部祖先目录（自浅至深，'/' 分隔；根 "" 不含）。
// 例："a/b/c.go" → ["a", "a/b"]；"a" → nil。
func ancestorDirs(rel string) []string {
	var out []string
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			out = append(out, rel[:i])
		}
	}
	return out
}
