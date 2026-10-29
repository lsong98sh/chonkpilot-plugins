// Package ignore 是 gitignore(5) 语义排除的**I/O 外壳**：
// 纯解析/匹配内核已抽到 chonkpilot-util/ignore，本包只保留读盘、逐级 .gitignore
// 叠加、目录遍历（WalkDir）与按 instance 的规则缓存。
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
	"path/filepath"
	"strings"

	uignore "github.com/lsong98sh/chonkpilot-util/ignore"
	"github.com/lsong98sh/chonkpilot-util/strlist"
)

// Rule 是已编译的 gitignore 规则（别名到纯内核，字段不导出）。
type Rule = uignore.Rule

// Parse 解析一段 gitignore 语法文本为有序规则列表（空行 / 注释 / 无效行丢弃）。
func Parse(content, base string) []Rule { return uignore.Parse(content, base) }

// BuiltinRules 内置强制排除规则（不可被 '!' 反选）。
func BuiltinRules() []Rule { return uignore.BuiltinRules() }

// DefaultRules 默认排除规则（可被 '!' 反选）。
func DefaultRules() []Rule { return uignore.DefaultRules() }

// Match 按「最后一条匹配的规则决定」判定 rel 是否被忽略（forced 恒优先且不可反选）。
func Match(forced, ordered []Rule, rel string, isDir bool) bool {
	return uignore.Match(forced, ordered, rel, isDir)
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

// Keys 是一次「配置 → 排除规则」读取所用的显式键名（由调用方给出）。
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

// ConfigOptions 由「配置读取器」构造遍历排除配置：
//
//	get 返回 (值, 是否命中)；workdir 为规则根。仅当 workdir 为空（配置错误）→ 返回 err
//	（opts / enabled 仍按已读到的配置填充）；workdir 不存在不报错。
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

// splitRules 解析用户排除规则文本（逗号/分号/换行分隔、去空白、保序保重复）。
func splitRules(s string) []string {
	return strlist.Split(s)
}

// WalkDir 深度优先遍历 root：跳过被忽略的目录（不下降）与文件，其余交给 fn。
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
// = 该路径自身命中，或任一祖先目录按「目录剪枝」命中。根自身恒不排除。
// 与 WalkDir 严格同源（共用 ruleSet.decide）。
func (o *Options) Ignored(rel string) bool {
	rel = uignore.NormalizeRel(rel)
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
	for _, anc := range uignore.AncestorDirs(rel) {
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

// ruleSet 是 WalkDir 与 Ignored 共用的规则求值上下文。
type ruleSet struct {
	stack  bool
	root   string
	forced []Rule            // 内置强制（不可反选）
	base   []Rule            // 默认排除 + 全局 ignore + info/exclude（低 → 高优先级）
	user   []Rule            // 用户规则（最高优先级）
	own    map[string][]Rule // 各目录 .gitignore 规则（惰性加载）
	loaded map[string]bool   // own 已加载标记（含空规则 → 不重复读盘）
}

// newRuleSet 组装规则集：内置强制 → 默认排除 → 全局 ignore → info/exclude
// → 各级 .gitignore（按 base 目录区分优先级）→ 用户规则。
func newRuleSet(root string, opts Options) *ruleSet {
	base := DefaultRules()
	if opts.StackGitignore {
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

// decide 判定 rel 是否被忽略：从最高优先级来源反向扫描，首个匹配者决定。
func (rs *ruleSet) decide(rel string, isDir bool) bool {
	if uignore.MatchesAny(rs.forced, rel, isDir) {
		return true // 强制排除：不可被 '!' 反选
	}
	if m, ok := uignore.LastMatch(rs.user, rel, isDir); ok {
		return m
	}
	for dir := dirOf(rel); ; dir = dirOf(dir) {
		rs.loadOwn(dir)
		if m, ok := uignore.LastMatch(rs.own[dir], rel, isDir); ok {
			return m
		}
		if dir == "" {
			break
		}
	}
	m, _ := uignore.LastMatch(rs.base, rel, isDir)
	return m
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
