package server

import (
	"context"
	"testing"
)

// TestGoInterfaceDispatch 接口方法 + 具体实现：候选全部列出，dispatch=interface，
// possibleImplementations 给出具体类型上的同名方法。
func TestGoInterfaceDispatch(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": `package main

type Runner interface {
	Run()
}

type Impl struct{}

func (i *Impl) Run() {}

func Start(r Runner) {
	r.Run()
}
`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	runs := w.SearchSymbol("Run", "method", "", 10)
	if len(runs) != 2 {
		t.Fatalf("Run 应有接口方法+实现两个定义: %+v", runs)
	}
	owners := map[string]string{}
	for _, s := range runs {
		owners[s.Owner] = s.OwnerKind
	}
	if owners["Runner"] != "interface" || owners["Impl"] != "type" {
		t.Fatalf("owner 解析错误: %+v", owners)
	}

	start := w.SearchSymbol("Start", "func", "", 1)
	if len(start) != 1 {
		t.Fatalf("Start=%v", start)
	}
	refs, total := w.CalleesOf(start[0], 500, 0)
	if total != 1 || len(refs) != 1 {
		t.Fatalf("Start 应只有 Run 一个调用: %v", refs)
	}
	ref := refs[0]
	if ref.Name != "Run" || ref.Candidates != 2 || ref.Dispatch != "interface" {
		t.Fatalf("接口分派信息错误: %+v", ref)
	}
	if len(ref.CandidateSymbols) != 2 {
		t.Fatalf("应列出全部同名候选: %+v", ref.CandidateSymbols)
	}
	if len(ref.PossibleImplementations) != 1 || ref.PossibleImplementations[0].Owner != "Impl" {
		t.Fatalf("可能实现错误: %+v", ref.PossibleImplementations)
	}
}

// TestRecursionFlags 自递归与互递归（调用边回环）标记。
func TestRecursionFlags(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"r.go": `package main

func A() { B() }
func B() { A() }
func R() { R() }
`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	refFor := func(name string) CalleeRef {
		t.Helper()
		s := w.SearchSymbol(name, "func", "", 1)
		if len(s) != 1 {
			t.Fatalf("%s=%v", name, s)
		}
		refs, _ := w.CalleesOf(s[0], 500, 0)
		if len(refs) != 1 {
			t.Fatalf("%s callees=%v", name, refs)
		}
		return refs[0]
	}
	a := refFor("A")
	if a.Name != "B" || a.Recursive != true || a.SelfRecursive {
		t.Fatalf("A→B 应标记互递归: %+v", a)
	}
	r := refFor("R")
	if !r.SelfRecursive || !r.Recursive {
		t.Fatalf("R 应标记自递归: %+v", r)
	}
}

// TestTSInterfaceOwner TS 接口方法签名与类实现。
func TestTSInterfaceOwner(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.ts": `interface Runner { Run(): void }
class Impl implements Runner { Run() {} }
function Start(r: Runner) { r.Run(); }
`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	runs := w.SearchSymbol("Run", "method", "", 10)
	if len(runs) < 2 {
		t.Fatalf("接口方法+实现应各有一个定义: %+v", runs)
	}
	owners := map[string]string{}
	for _, s := range runs {
		owners[s.Owner] = s.OwnerKind
	}
	if owners["Runner"] != "interface" || owners["Impl"] != "class" {
		t.Fatalf("TS owner 解析错误: %+v", owners)
	}
	start := w.SearchSymbol("Start", "func", "", 1)
	refs, _ := w.CalleesOf(start[0], 500, 0)
	if len(refs) != 1 || refs[0].Dispatch != "interface" || len(refs[0].PossibleImplementations) != 1 {
		t.Fatalf("TS 分派信息错误: %+v", refs)
	}
}

// TestCalleesToolExposesDispatch 工具返回面包含新字段。
func TestCalleesToolExposesDispatch(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"a.go": "package main\ntype I interface{ Run() }\ntype T struct{}\nfunc (t T) Run() {}\nfunc S(i I) { i.Run() }\n",
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	s := w.SearchSymbol("S", "func", "", 1)
	out, err := toolCallees(context.Background(), map[string]any{"workdir": dir, "id": s[0].ID})
	if err != nil {
		t.Fatalf("toolCallees: %v", err)
	}
	m := out.(map[string]any)
	refs := m["callees"].([]CalleeRef)
	if len(refs) != 1 || refs[0].Dispatch != "interface" || len(refs[0].CandidateSymbols) != 2 {
		t.Fatalf("tool 返回缺少分派信息: %+v", refs)
	}
}

// TestOwnerKindsAcrossLanguages Java 抽象类 / Python 类 / Rust trait 的 owner 解析。
func TestOwnerKindsAcrossLanguages(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"Base.java": `abstract class Base { abstract void Run(); }
class Impl extends Base { void Run() {} }
class User { void Go(Base b) { b.Run(); } }
`,
		"a.py": `class A:
    def run(self):
        pass
def start(a):
    a.run()
`,
		"a.rs": `trait T { fn run(&self); }
struct S;
impl T for S { fn run(&self) {} }
fn start(s: &S) { s.run(); }
`,
	})
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dropWorkspace(w.Dir)
	if err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	ownerOfName := func(name, file string) map[string]string {
		t.Helper()
		m := map[string]string{}
		for _, s := range w.SearchSymbol(name, "method", file, 20) {
			m[s.Owner] = s.OwnerKind
		}
		return m
	}
	java := ownerOfName("Run", "Base.java")
	if java["Base"] != "abstract_class" || java["Impl"] != "class" {
		t.Fatalf("Java owner=%v", java)
	}
	py := ownerOfName("run", "a.py")
	if py["A"] != "class" {
		t.Fatalf("Python owner=%v", py)
	}
	rs := ownerOfName("run", "a.rs")
	if rs["T"] != "trait" || rs["S"] != "trait_impl" {
		t.Fatalf("Rust owner=%v", rs)
	}

	// Java 抽象方法调用：两个候选，dispatch=abstract，可能实现=Impl。
	user := w.SearchSymbol("Go", "method", "", 1)
	if len(user) != 1 {
		t.Fatalf("Go method=%v", user)
	}
	refs, _ := w.CalleesOfLang(user[0], "java", 500, 0)
	if len(refs) != 1 || refs[0].Dispatch != "abstract" {
		t.Fatalf("Java 抽象分派=%+v", refs)
	}
	if len(refs[0].PossibleImplementations) != 1 || refs[0].PossibleImplementations[0].Owner != "Impl" {
		t.Fatalf("Java 可能实现=%+v", refs[0].PossibleImplementations)
	}
}
