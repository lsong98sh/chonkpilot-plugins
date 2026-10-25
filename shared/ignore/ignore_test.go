package ignore

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ─── 辅助 ────────────────────────────────────────────────

func parseOne(t *testing.T, line string) Rule {
	t.Helper()
	rules := Parse(line, "")
	if len(rules) != 1 {
		t.Fatalf("Parse(%q) 应得 1 条规则，实际 %d", line, len(rules))
	}
	return rules[0]
}

func matchOne(t *testing.T, line, rel string, isDir bool) bool {
	t.Helper()
	return parseOne(t, line).match(rel, isDir)
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// walked 返回 stack/规则配置下被遍历到的文件（相对 root，'/' 分隔，已排序）。
func walked(t *testing.T, root string, opts Options) []string {
	t.Helper()
	var out []string
	err := WalkDir(root, opts, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	sort.Strings(out)
	return out
}

// isolateGlobal 把全局 ignore 指向空的临时目录（避免开发机 ~/.config/git/ignore 干扰断言）。
func isolateGlobal(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func assertFiles(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("遍历结果不符\n got=%v\nwant=%v", got, want)
	}
}

// ─── 规则语法（gitignore(5) 用例族）────────────────────────

func TestParseSkipsCommentsAndBlanks(t *testing.T) {
	for _, content := range []string{"", "\n\n", "   \n", "# 注释\n  \n", "\t\n"} {
		if got := Parse(content, ""); len(got) != 0 {
			t.Fatalf("Parse(%q) 应无规则，实际 %d 条", content, len(got))
		}
	}
}

func TestParseEscapes(t *testing.T) {
	if !matchOne(t, `\#notcomment`, "#notcomment", false) {
		t.Error(`'\#notcomment' 应匹配字面 '#notcomment'`)
	}
	if !matchOne(t, `\!notnegate`, "!notnegate", false) {
		t.Error(`'\!notnegate' 应匹配字面 '!notnegate'`)
	}
	// '!' 取反：取消忽略
	r := parseOne(t, "!keep.log")
	if !r.negate || r.match("keep.log", false) == false {
		t.Error("'!keep.log' 应为取反规则且匹配 keep.log")
	}
	// 行尾空格忽略；`\ ` 保留为字面空格
	if !matchOne(t, "foo   ", "foo", false) {
		t.Error("行尾未转义空格应被忽略 → 匹配 'foo'")
	}
	if matchOne(t, "foo   ", "foo ", false) {
		t.Error("'foo   ' 不应匹配 'foo '（空格已忽略）")
	}
	if !matchOne(t, `foo\ `, "foo ", false) {
		t.Error(`'foo\ ' 应匹配 'foo '（转义空格保留）`)
	}
}

func TestGlobStarNotCrossSlash(t *testing.T) {
	// 非锚定（无 '/'）→ 匹配任意层级同名条目
	if !matchOne(t, "*.log", "x.log", false) || !matchOne(t, "*.log", "a/b.log", false) {
		t.Error("'*.log' 应匹配任意层级 .log")
	}
	// 锚定（含 '/'）→ 相对根
	if !matchOne(t, "a/*.log", "a/x.log", false) {
		t.Error("'a/*.log' 应匹配 a/x.log")
	}
	if matchOne(t, "a/*.log", "a/b/x.log", false) {
		t.Error("'*' 不得跨 '/'：a/*.log 不应匹配 a/b/x.log")
	}
	if matchOne(t, "a*b", "a/b", false) {
		t.Error("'*' 不得跨 '/'：a*b 不应匹配 a/b")
	}
	if !matchOne(t, "a?b", "axb", false) || matchOne(t, "a?b", "a/b", false) {
		t.Error("'?' 应匹配单字符且不跨 '/'")
	}
}

func TestDoubleStarPositions(t *testing.T) {
	// 前导 '**/'
	for _, rel := range []string{"foo", "a/foo", "a/b/foo"} {
		if !matchOne(t, "**/foo", rel, false) {
			t.Errorf("'**/foo' 应匹配 %s", rel)
		}
	}
	if matchOne(t, "**/foo", "bar/foo2", false) {
		t.Error("'**/foo' 不应匹配 bar/foo2")
	}
	// 尾随 '/**'（匹配目录内全部，不含目录自身）
	for _, rel := range []string{"foo/a", "foo/a/b"} {
		if !matchOne(t, "foo/**", rel, false) {
			t.Errorf("'foo/**' 应匹配 %s", rel)
		}
	}
	if matchOne(t, "foo/**", "foo", true) {
		t.Error("'foo/**' 不应匹配 foo 自身")
	}
	// 中间 '/**/'
	for _, rel := range []string{"a/b", "a/x/b", "a/x/y/b"} {
		if !matchOne(t, "a/**/b", rel, false) {
			t.Errorf("'a/**/b' 应匹配 %s", rel)
		}
	}
	if matchOne(t, "a/**/b", "a/b/c", false) {
		t.Error("'a/**/b' 不应匹配 a/b/c")
	}
}

func TestCharClass(t *testing.T) {
	if !matchOne(t, "foo[0-9].txt", "foo1.txt", false) || matchOne(t, "foo[0-9].txt", "fooa.txt", false) {
		t.Error("'[0-9]' 字符类判定错误")
	}
	if !matchOne(t, "[!a]bc", "xbc", false) || matchOne(t, "[!a]bc", "abc", false) {
		t.Error("'[!a]' 取反字符类判定错误")
	}
	if !matchOne(t, "[a", "[a", false) {
		t.Error("未闭合 '[' 应按字面匹配")
	}
}

func TestDirOnly(t *testing.T) {
	if !matchOne(t, "build/", "build", true) || !matchOne(t, "build/", "a/build", true) {
		t.Error("尾 '/' 应匹配任意层级同名目录")
	}
	if matchOne(t, "build/", "build", false) {
		t.Error("尾 '/' 不应匹配同名文件")
	}
	if !matchOne(t, "build", "build", false) {
		t.Error("无尾 '/' 应同时匹配文件与目录")
	}
}

func TestAnchoredVsNonAnchored(t *testing.T) {
	if !matchOne(t, "/top.txt", "top.txt", false) || matchOne(t, "/top.txt", "a/top.txt", false) {
		t.Error("'/top.txt' 应仅匹配根层")
	}
	if !matchOne(t, "a/b", "a/b", false) || matchOne(t, "a/b", "x/a/b", false) {
		t.Error("含 '/' 的规则相对根锚定")
	}
	if !matchOne(t, "b", "b", false) || !matchOne(t, "b", "a/b", false) {
		t.Error("不含 '/' 的规则匹配任意层级")
	}
}

func TestOrderedLastMatchWins(t *testing.T) {
	rules := Parse("*.log\n!important.log\n", "")
	if !Match(nil, rules, "x.log", false) {
		t.Error("x.log 应被忽略")
	}
	if Match(nil, rules, "important.log", false) {
		t.Error("important.log 应被 '!' 反选（后者覆盖前者）")
	}
	// 顺序颠倒 → 取反在前、忽略在后 → 仍被忽略
	rev := Parse("!important.log\n*.log\n", "")
	if !Match(nil, rev, "important.log", false) {
		t.Error("顺序颠倒后 important.log 应被忽略（最后匹配者决定）")
	}
}

func TestBuiltinForcedNotNegatable(t *testing.T) {
	neg := Parse("!.git/\n!.chonkpilot/\n!.svn/\n", "")
	for _, rel := range []string{".git", ".chonkpilot", "a/.svn"} {
		if !Match(BuiltinRules(), neg, rel, true) {
			t.Errorf("内置强制排除 %s 不可被 '!' 反选", rel)
		}
	}
	// 默认排除（可反选）
	def := DefaultRules()
	all := append(append([]Rule{}, def...), Parse("!dist/\n", "")...)
	if Match(nil, all, "dist", true) {
		t.Error("默认排除 dist 应可被用户 '!' 反选")
	}
	if !Match(nil, def, "dist", true) {
		t.Error("默认排除 dist 应默认生效")
	}
}

// ─── 遍历（含逐级 .gitignore / exclude / 全局 ignore / 用户输入）────

// buildTree 造统一测试树（返回值 = root）。
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".gitignore"), strings.Join([]string{
		"*.log",
		"!keep.log",
		"dist2/",
		"secret.txt",
	}, "\n")+"\n")
	writeFileT(t, filepath.Join(root, "a.txt"), "a")
	writeFileT(t, filepath.Join(root, "x.log"), "x")
	writeFileT(t, filepath.Join(root, "keep.log"), "k")
	writeFileT(t, filepath.Join(root, "secret.txt"), "s")
	writeFileT(t, filepath.Join(root, "dist2", "y.txt"), "y")
	writeFileT(t, filepath.Join(root, "node_modules", "pkg", "i.js"), "n")
	writeFileT(t, filepath.Join(root, ".chonkpilot", "codegraph", "meta.json"), "{}")
	writeFileT(t, filepath.Join(root, ".git", "config"), "[core]")
	writeFileT(t, filepath.Join(root, "sub", ".gitignore"), "*.txt\n!needed.txt\n")
	writeFileT(t, filepath.Join(root, "sub", "b.txt"), "b")
	writeFileT(t, filepath.Join(root, "sub", "needed.txt"), "n")
	writeFileT(t, filepath.Join(root, "sub", "deep", ".gitignore"), "!b.txt\n")
	writeFileT(t, filepath.Join(root, "sub", "deep", "b.txt"), "d")
	return root
}

func TestWalkDirStackGitignore(t *testing.T) {
	isolateGlobal(t)
	root := buildTree(t)
	got := walked(t, root, Options{StackGitignore: true})
	assertFiles(t, got, []string{
		".gitignore", "a.txt", "keep.log",
		"sub/.gitignore", "sub/deep/.gitignore", "sub/deep/b.txt", "sub/needed.txt",
	})
}

func TestWalkDirNoStack(t *testing.T) {
	isolateGlobal(t)
	root := buildTree(t)
	got := walked(t, root, Options{StackGitignore: false})
	// 不读任何 .gitignore：仅内置强制 + 默认排除生效（根 .gitignore 的 dist2/ 不再生效）
	assertFiles(t, got, []string{
		".gitignore", "a.txt", "dist2/y.txt", "keep.log", "secret.txt",
		"sub/.gitignore", "sub/b.txt", "sub/deep/.gitignore", "sub/deep/b.txt", "sub/needed.txt",
		"x.log",
	})
}

func TestWalkDirParentIgnoredChildNegationUseless(t *testing.T) {
	isolateGlobal(t)
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".gitignore"), "build/\n")
	writeFileT(t, filepath.Join(root, "build", ".gitignore"), "!keep.go\n")
	writeFileT(t, filepath.Join(root, "build", "keep.go"), "p")
	writeFileT(t, filepath.Join(root, "build", "other.go"), "p")
	writeFileT(t, filepath.Join(root, "main.go"), "p")

	got := walked(t, root, Options{StackGitignore: true})
	// build/ 被忽略 → 不下降 → 其子级 '!' 无法救回
	assertFiles(t, got, []string{".gitignore", "main.go"})
}

func TestWalkDirGitInfoExcludeAndGlobal(t *testing.T) {
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".git", "info", "exclude"), "excluded-by-info.txt\n")
	writeFileT(t, filepath.Join(root, "excluded-by-info.txt"), "e")
	writeFileT(t, filepath.Join(root, "keep.txt"), "k")
	writeFileT(t, filepath.Join(root, "global.tmp"), "g")

	xdg := t.TempDir()
	writeFileT(t, filepath.Join(xdg, "git", "ignore"), "*.tmp\n")
	t.Setenv("XDG_CONFIG_HOME", xdg)

	assertFiles(t, walked(t, root, Options{StackGitignore: true}), []string{"keep.txt"})
	// 不叠加时不读 exclude / 全局 ignore
	assertFiles(t, walked(t, root, Options{StackGitignore: false}),
		[]string{"excluded-by-info.txt", "global.tmp", "keep.txt"})
}

func TestWalkDirUserRulesHighestPriority(t *testing.T) {
	isolateGlobal(t)
	root := buildTree(t)
	got := walked(t, root, Options{
		StackGitignore: true,
		UserRules:      []string{"!x.log", "secret.txt", "sub/"},
	})
	// 用户规则最高优先级：x.log 反选回；sub/ 整目录排除（覆盖更深层 .gitignore）
	assertFiles(t, got, []string{".gitignore", "a.txt", "keep.log", "x.log"})
}

func TestWalkDirUserRuleFileLevel(t *testing.T) {
	isolateGlobal(t)
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, "a", "skip.go"), "p")
	writeFileT(t, filepath.Join(root, "a", "keep.go"), "p")
	got := walked(t, root, Options{UserRules: []string{"a/skip.go"}})
	assertFiles(t, got, []string{"a/keep.go"})
}

// ─── 配置 → 规则助手（ConfigOptions）────────────────────────

// mapGet 由 map 造 get（未命中 → ok=false，等价「键不存在」）。
func mapGet(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

func TestConfigOptions(t *testing.T) {
	root := t.TempDir()
	keys := Keys{
		Enabled:        "plugins.demo.codegraph.enabled",
		SkipDirs:       "plugins.demo.codegraph.skip-dirs",
		StackGitignore: "plugins.demo.codegraph.stack-gitignore",
	}
	opts, enabled, err := ConfigOptions(keys, mapGet(map[string]string{
		keys.Enabled:        "true",
		keys.SkipDirs:       "dist/, !dist/keep.txt\nlogs;  \n!logs/a,dist/",
		keys.StackGitignore: "true",
	}), root)
	if err != nil {
		t.Fatalf("ConfigOptions: %v", err)
	}
	if !enabled || !opts.StackGitignore || opts.Root != filepath.Clean(root) {
		t.Fatalf("enabled/stack/root 不符：enabled=%v stack=%v root=%q", enabled, opts.StackGitignore, opts.Root)
	}
	want := []string{"dist/", "!dist/keep.txt", "logs", "!logs/a", "dist/"}
	if strings.Join(opts.UserRules, "|") != strings.Join(want, "|") {
		t.Fatalf("规则拆分应保序不去重：got=%v want=%v", opts.UserRules, want)
	}

	// 缺省键（get 未命中）→ enabled=false、无用户规则、stack=false
	opts2, enabled2, _ := ConfigOptions(keys, mapGet(nil), root)
	if enabled2 || opts2.StackGitignore || len(opts2.UserRules) != 0 {
		t.Fatalf("缺省应关闭：enabled=%v stack=%v rules=%v", enabled2, opts2.StackGitignore, opts2.UserRules)
	}

	// 空键名 = 该配置项不参与（全空 → 缺省）
	opts4, enabled4b, _ := ConfigOptions(Keys{}, mapGet(map[string]string{
		"some.other.key": "true",
	}), root)
	if enabled4b || opts4.StackGitignore {
		t.Fatalf("空键名应按缺省：enabled=%v stack=%v", enabled4b, opts4.StackGitignore)
	}

	// 空串规则 → 无用户规则（非 nil 也可，长度必须为 0）
	opts3, _, _ := ConfigOptions(keys, mapGet(map[string]string{keys.SkipDirs: "  \n , ; "}), root)
	if len(opts3.UserRules) != 0 {
		t.Fatalf("全空白规则应无规则：%v", opts3.UserRules)
	}

	// 空 workdir → err（配置错误），enabled 仍按配置读出（调用方可降级）
	_, enabled4, err4 := ConfigOptions(keys, mapGet(map[string]string{
		keys.Enabled: "true",
	}), "")
	if err4 == nil || !enabled4 {
		t.Fatalf("空 workdir 应报错且 enabled 仍按配置：err=%v enabled=%v", err4, enabled4)
	}
	// workdir 不存在 → 不报错（逐级 .gitignore 读取静默容忍缺失，判定降级为「不排除」）
	if _, _, err := ConfigOptions(keys, mapGet(nil), filepath.Join(root, "does-not-exist")); err != nil {
		t.Fatalf("workdir 不存在不应报错：%v", err)
	}
}

// ─── 含祖先剪枝的判定（Ignored）────────────────────────────

// buildIgnoredTree 造 Ignored 用例树（返回值 = root）：
//
//	.gitignore: *.gen.go / !special.gen.go / sub/ / anchored/only.txt
//	a.gen.go（忽略）· special.gen.go（'!' 反选）· main.go（保留）
//	anchored/only.txt（锚定忽略）· anchored/other.txt（保留）
//	sub/.gitignore: !x.go · sub/x.go —— sub/ 被剪枝 → 子项不可救回
func buildIgnoredTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileT(t, filepath.Join(root, ".gitignore"), strings.Join([]string{
		"*.gen.go",
		"!special.gen.go",
		"sub/",
		"anchored/only.txt",
	}, "\n")+"\n")
	writeFileT(t, filepath.Join(root, "main.go"), "p")
	writeFileT(t, filepath.Join(root, "a.gen.go"), "p")
	writeFileT(t, filepath.Join(root, "special.gen.go"), "p")
	writeFileT(t, filepath.Join(root, "anchored", "only.txt"), "p")
	writeFileT(t, filepath.Join(root, "anchored", "other.txt"), "p")
	writeFileT(t, filepath.Join(root, "sub", ".gitignore"), "!x.go\n")
	writeFileT(t, filepath.Join(root, "sub", "x.go"), "p")
	writeFileT(t, filepath.Join(root, "sub", "deep", "y.go"), "p")
	return root
}

func TestIgnoredStackGitignore(t *testing.T) {
	isolateGlobal(t)
	root := buildIgnoredTree(t)
	opts := &Options{Root: root, StackGitignore: true}

	for _, tc := range []struct {
		rel  string
		want bool
		why  string
	}{
		{"main.go", false, "无规则命中"},
		{"a.gen.go", true, "非锚定文件规则"},
		{"special.gen.go", false, "同级 '!' 反选正常生效"},
		{"anchored/only.txt", true, "含 '/' 的规则相对根锚定"},
		{"anchored/other.txt", false, "锚定规则不误伤同目录其它文件"},
		{"sub", true, "尾斜杠目录规则（自身）"},
		{"sub/x.go", true, "祖先剪枝：sub/ 被排除，子级 '!' 反选不可救回"},
		{"sub/deep/y.go", true, "祖先剪枝（多级）"},
		{".git/config", true, "内置强制排除（.git/）"},
		{".chonkpilot/x", true, "内置强制排除（.chonkpilot/）"},
		{"", false, "根自身恒不排除"},
		{"sub/", true, "带尾斜杠入参归一后仍判剪枝"},
	} {
		if got := opts.Ignored(tc.rel); got != tc.want {
			t.Errorf("Ignored(%q)=%v，期望 %v（%s）", tc.rel, got, tc.want, tc.why)
		}
	}
}

func TestIgnoredNoStackReadsNoIgnoreFiles(t *testing.T) {
	isolateGlobal(t)
	root := buildIgnoredTree(t)
	writeFileT(t, filepath.Join(root, "node_modules", "pkg", "i.js"), "n")
	// stack=false：不读任何 ignore 文件（根 .gitignore / 各级 .gitignore 均不生效），
	// 但内置强制排除与默认排除（node_modules/ 等）照旧 —— 与 WalkDir 口径一致。
	opts := &Options{Root: root, StackGitignore: false}
	if opts.Ignored("a.gen.go") {
		t.Error("stack=false 不应读根 .gitignore（a.gen.go 不该被排除）")
	}
	if opts.Ignored("sub/x.go") {
		t.Error("stack=false 不应读各级 .gitignore（sub/x.go 不该被排除）")
	}
	if !opts.Ignored("node_modules/pkg/i.js") {
		t.Error("默认排除（node_modules/）在 stack=false 时仍生效")
	}
	if !opts.Ignored(".git/objects/ab") {
		t.Error("内置强制排除在 stack=false 时仍生效")
	}
}

func TestIgnoredUserRulesAndBuiltinForced(t *testing.T) {
	isolateGlobal(t)
	root := buildIgnoredTree(t)
	// 用户规则最高优先级：'!' 反选回被 .gitignore 排除的 a.gen.go；同时不可反选内置强制。
	opts := &Options{Root: root, StackGitignore: true,
		UserRules: []string{"!a.gen.go", "!.git/", "!.chonkpilot/"}}
	if opts.Ignored("a.gen.go") {
		t.Error("用户 '!' 规则应反选回 a.gen.go")
	}
	if !opts.Ignored(".git/config") {
		t.Error("内置强制排除 .git/ 不可被用户 '!' 反选")
	}
	if !opts.Ignored(".chonkpilot/x") {
		t.Error("内置强制排除 .chonkpilot/ 不可被用户 '!' 反选")
	}
	// 用户规则新增文件级排除
	opts2 := &Options{Root: root, UserRules: []string{"anchored/other.txt"}}
	if !opts2.Ignored("anchored/other.txt") {
		t.Error("用户文件级规则应生效")
	}
}

// TestIgnoredMatchesWalkDir 交叉验证：磁盘上每个路径，Ignored(p) ⇔ WalkDir 会跳过 p
// （两者共用同一 ruleSet.decide 与祖先剪枝口径）。
func TestIgnoredMatchesWalkDir(t *testing.T) {
	isolateGlobal(t)
	root := buildIgnoredTree(t)
	writeFileT(t, filepath.Join(root, "node_modules", "pkg", "i.js"), "n")
	opts := Options{Root: root, StackGitignore: true, UserRules: []string{"!special.gen.go", "anchored/other.txt"}}

	kept := map[string]bool{}
	err := WalkDir(root, opts, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel != "." {
			kept[rel] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir 枚举: %v", err)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if got, want := opts.Ignored(rel), !kept[rel]; got != want {
			t.Errorf("Ignored(%q)=%v，WalkDir 保留=%v（期望 Ignored=%v）", rel, got, kept[rel], want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir 枚举: %v", err)
	}
}
