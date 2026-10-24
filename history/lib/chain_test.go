// L1：检查点链内核白盒（真实 git 仓库：t.TempDir() + git init）。
//
// 覆盖：打点建链 / 不脏零调用 / 门控 / 工具注册门控 / 修剪（keep·ttl·锚点·保底）/
// 熔断 / 父子会话共享链 / restore 一致性校验与删除恢复 / diff·show / 消息驱动打点 /
// **绝不在用户分支产生提交（HEAD 与 .git/index 不变）**。
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// ─── 基础工具 ───────────────────────────────────────────────

func gitT(t *testing.T, wd string, args ...string) string {
	t.Helper()
	all := append([]string{"-C", wd}, args...)
	out, err := exec.Command("git", all...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git 不可用")
	}
	wd := t.TempDir()
	gitT(t, wd, "init", "-q")
	gitT(t, wd, "config", "user.email", "test@chonkpilot.local")
	gitT(t, wd, "config", "user.name", "chonkpilot-test")
	return wd
}

func refExists(t *testing.T, wd, slug string) bool {
	t.Helper()
	_, err := exec.Command("git", "-C", wd, "rev-parse", "--verify", "--quiet", chainRefPrefix+slug).CombinedOutput()
	return err == nil
}

func chainLen(t *testing.T, wd, slug string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", wd, "rev-list", "--count", chainRefPrefix+slug).CombinedOutput()
	if err != nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscan(strings.TrimSpace(string(out)), &n)
	return n
}

func writeFileT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileT(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ─── prj-config / gateway 桩 ───────────────────────────────

// regLog 记录回写的 prj 键与工具注册载荷。
type regLog struct {
	mu   sync.Mutex
	save map[string]string
	regs []map[string]any
	uns  int
}

func (r *regLog) recordSave(k, v string) {
	r.mu.Lock()
	r.save[k] = v
	r.mu.Unlock()
}

func (r *regLog) savedVal(k string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.save[k]
}

func (r *regLog) recordReg(p map[string]any) {
	r.mu.Lock()
	r.regs = append(r.regs, p)
	r.mu.Unlock()
}

func (r *regLog) regPayloads() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.regs))
	copy(out, r.regs)
	return out
}

func (r *regLog) unregCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.uns
}

// stubData 桩：data-prj-config-load / -save + gateway tools/register|unregister。
func stubData(t *testing.T, bus mq.Bus, vals map[string]string, reg *regLog) {
	t.Helper()
	if _, err := bus.On(prjConfigLoadSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil || m["ok"] != nil {
			return nil
		}
		id := ""
		if d, ok := m["data"].(map[string]any); ok {
			id, _ = d["id"].(string)
		}
		reqID, _ := m["req_id"].(string)
		b, _ := json.Marshal(map[string]any{
			"req_id": reqID, "ok": true, "result": map[string]any{"data": vals[id]},
		})
		go bus.Emit(context.Background(), prjConfigLoadSubject, b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.On(prjConfigSaveSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil || m["ok"] != nil {
			return nil
		}
		if d, ok := m["data"].(map[string]any); ok {
			key, _ := d["key"].(string)
			value, _ := d["value"].(string)
			reg.recordSave(key, value)
		}
		reqID, _ := m["req_id"].(string)
		b, _ := json.Marshal(map[string]any{"req_id": reqID, "ok": true, "result": map[string]any{}})
		go bus.Emit(context.Background(), prjConfigSaveSubject, b)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.On(subjectToolRegister, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var p map[string]any
		_ = json.Unmarshal(v.Payload, &p)
		reg.recordReg(p)
		v.Result = map[string]any{"registered": true, "name": p["name"]}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.On(subjectToolUnregister, 0, func(_ context.Context, _ string, v *mq.Value) error {
		reg.mu.Lock()
		reg.uns++
		reg.mu.Unlock()
		v.Result = map[string]any{"unregistered": true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// ─── 插件装配 ───────────────────────────────────────────────

const testInstance = "ins-h2"

type harness struct {
	bus mq.Bus
	h   *History
	wd  string
	reg *regLog
}

// newHarnessWithWd 装配插件（wd 为 work_dir），并等门控回读落定。
func newHarnessWithWd(t *testing.T, wd string, enabled bool, keep, ttl string) *harness {
	t.Helper()
	bus, err := mq.New(mq.Options{Prefix: "chonk."})
	if err != nil {
		t.Fatalf("mq.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	vals := map[string]string{historyEnabledKey: boolStr(enabled)}
	if keep != "" {
		vals[keepKey] = keep
	}
	if ttl != "" {
		vals[ttlKey] = ttl
	}
	reg := &regLog{save: map[string]string{}}
	stubData(t, bus, vals, reg)
	h := New()
	if err := h.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("history Start: %v", err)
	}
	emitJSON(t, bus, instanceRegisterSubject, map[string]any{
		"instance_id": testInstance, "client_type": "unittest", "work_dir": wd,
	})
	waitGateT(t, h, testInstance)
	return &harness{bus: bus, h: h, wd: wd, reg: reg}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func emitJSON(t *testing.T, bus mq.Bus, subject string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	bus.Emit(context.Background(), subject, b)
}

func (x *harness) ws(t *testing.T) *workState {
	t.Helper()
	ws := x.h.work(x.wd)
	if ws == nil {
		t.Fatal("workState 未建立")
	}
	return ws
}

// cp 置脏并同步打点（白盒直调；force=false = 前置钩子路径）。
func (x *harness) cp(t *testing.T, root, tool, turn string) {
	t.Helper()
	ws := x.ws(t)
	ws.dirty.Store(true)
	if err := x.h.checkpointSync(ws, root, tool, turn, false); err != nil {
		t.Fatalf("checkpointSync: %v", err)
	}
}

// forceCp 直调轮末补点（force=true）：**不置脏标记**，模拟「工具改文件 → 轮次结束」间隔
// < filesys.changed 去抖窗口、脏位尚未置上的竞态。
func (x *harness) forceCp(t *testing.T, root, tool, turn string) {
	t.Helper()
	ws := x.ws(t)
	if err := x.h.checkpointSync(ws, root, tool, turn, true); err != nil {
		t.Fatalf("checkpointSync(force): %v", err)
	}
}

// callTool 经 gateway 回调主题驱动单个工具（链根 = root），返回（文本, isError）。
func (x *harness) callTool(t *testing.T, root, tool string, args map[string]any) (string, bool) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"tool": tool, "args": args,
		"context": map[string]any{"instance_id": testInstance, "session": root, "top_session": root},
	})
	v := &mq.Value{Payload: payload}
	if err := x.h.onToolCall(context.Background(), toolCallSubject, v); err != nil {
		t.Fatalf("onToolCall: %v", err)
	}
	m, _ := v.Result.(map[string]any)
	isErr, _ := m["isError"].(bool)
	text := ""
	if cs, ok := m["content"].([]any); ok && len(cs) > 0 {
		if c0, ok := cs[0].(map[string]any); ok {
			text, _ = c0["text"].(string)
		}
	}
	return text, isErr
}

func waitGateT(t *testing.T, h *History, id string) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.gateMu.Lock()
		v, ok := h.gate[id]
		h.gateMu.Unlock()
		if ok {
			return v
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待门控回读超时")
	return false
}

// buildDatedChain 用指定时间造一条链（绕过 doCheckpoint，便于构造"旧点/锚点"用例）。
func buildDatedChain(t *testing.T, h *History, ws *workState, slug string, times []time.Time) []chainRec {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(ws.index), 0o755); err != nil {
		t.Fatal(err)
	}
	h.ensureGitignore(ws.workDir)
	ref := chainRefPrefix + slug
	prev := ""
	var recs []chainRec
	for i, tm := range times {
		writeFileT(t, ws.workDir, fmt.Sprintf("f%d.txt", i), fmt.Sprintf("v%d", i))
		if _, err := h.git(ws, "add", "-A"); err != nil {
			t.Fatalf("add: %v", err)
		}
		tree, err := h.git(ws, "write-tree")
		if err != nil {
			t.Fatalf("write-tree: %v", err)
		}
		msg := fmt.Sprintf("chonk-ckpt: session=%s tool=tool%d ts=%s", slug, i, tm.Format(time.RFC3339))
		args := []string{"commit-tree", strings.TrimSpace(tree)}
		if prev != "" {
			args = append(args, "-p", prev)
		}
		args = append(args, "-m", msg)
		out, err := h.gitEnv(ws, []string{
			"GIT_AUTHOR_DATE=" + tm.Format(time.RFC3339),
			"GIT_COMMITTER_DATE=" + tm.Format(time.RFC3339),
		}, args...)
		if err != nil {
			t.Fatalf("commit-tree: %v", err)
		}
		prev = strings.TrimSpace(out)
		if _, err := h.git(ws, "update-ref", ref, prev); err != nil {
			t.Fatalf("update-ref: %v", err)
		}
		recs = append(recs, chainRec{id: prev, msg: msg, at: tm, ct: tm})
	}
	return recs
}

// chainMsgs 返回链上全部 message（新→旧）。
func chainMsgs(t *testing.T, wd, slug string, n int) []string {
	t.Helper()
	out := gitT(t, wd, "log", "--format=%s", "-n", fmt.Sprint(n), chainRefPrefix+slug)
	var msgs []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			msgs = append(msgs, l)
		}
	}
	return msgs
}

// ─── 用例 ───────────────────────────────────────────────────

// TestCheckpointChainNoUserBranchCommit：打点建链（ref/次序/message）+ **绝不在用户分支产生提交**。
func TestCheckpointChainNoUserBranchCommit(t *testing.T) {
	wd := newRepo(t)
	// 先造一条用户分支提交（.git/index 随之存在）
	writeFileT(t, wd, "README.md", "hello")
	gitT(t, wd, "add", ".")
	gitT(t, wd, "commit", "-q", "-m", "init")
	head0 := strings.TrimSpace(gitT(t, wd, "rev-parse", "HEAD"))
	idx0, err := os.ReadFile(filepath.Join(wd, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}

	x := newHarnessWithWd(t, wd, true, "", "")
	for i := 1; i <= 3; i++ {
		writeFileT(t, wd, fmt.Sprintf("a%d.txt", i), fmt.Sprintf("v%d", i))
		x.cp(t, "rootA", fmt.Sprintf("tool%d", i), "t1")
	}

	slug := chainSlug("rootA")
	if !refExists(t, wd, slug) {
		t.Fatal("检查点 ref 未建立")
	}
	if n := chainLen(t, wd, slug); n != 3 {
		t.Fatalf("链长 = %d，期望 3", n)
	}
	// 链线性连续：3 行、末行（根点）无 parent
	out := gitT(t, wd, "rev-list", "--parents", chainRefPrefix+slug)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("rev-list 行数 = %d，期望 3", len(lines))
	}
	for i, l := range lines {
		toks := strings.Fields(l)
		if i < len(lines)-1 && len(toks) != 2 {
			t.Fatalf("第 %d 行应有 1 个 parent: %q", i, l)
		}
		if i == len(lines)-1 && len(toks) != 1 {
			t.Fatalf("根点不应有 parent: %q", l)
		}
	}
	// message 含 session / tool / ts
	msgs := chainMsgs(t, wd, slug, 1)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "session=rootA") ||
		!strings.Contains(msgs[0], "tool=tool3") || !strings.Contains(msgs[0], "ts=") {
		t.Fatalf("检查点 message 不符: %v", msgs)
	}
	// 用户分支 HEAD 不变
	if got := strings.TrimSpace(gitT(t, wd, "rev-parse", "HEAD")); got != head0 {
		t.Fatalf("用户分支 HEAD 被改动: %s → %s", head0, got)
	}
	// .git/index 未被改动
	idx1, err := os.ReadFile(filepath.Join(wd, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(idx0, idx1) {
		t.Fatal(".git/index 被打点改动（应只写临时 index）")
	}
	// 状态回写：history.status.<slug> 含 checkpointCount=3（I-135 按会话键）
	var st chainStatus
	if err := json.Unmarshal([]byte(x.reg.savedVal(statusKeyPrefix+slug)), &st); err != nil {
		t.Fatalf("history.status 回写内容非法: %v", err)
	}
	if st.CheckpointCount != 3 || st.Mode != "active" {
		t.Fatalf("status 不符: %+v", st)
	}
	// 时间线回写：最新在前，n=-1..-3
	var tl []timelineEntry
	if err := json.Unmarshal([]byte(x.reg.savedVal(timelineKeyPrefix+slug)), &tl); err != nil {
		t.Fatalf("history.timeline 回写内容非法: %v", err)
	}
	if len(tl) != 3 || tl[0].N != -1 || tl[0].Tool != "tool3" {
		t.Fatalf("timeline 不符: %+v", tl)
	}
}

// TestCheckpointZeroGitWhenClean：不脏 → **零 git 调用**（git 不可用时亦不报错）。
func TestCheckpointZeroGitWhenClean(t *testing.T) {
	x := newHarnessWithWd(t, newRepo(t), true, "", "")
	ws := x.ws(t)
	old := os.Getenv("PATH")
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("PATH", old) }()
	ws.dirty.Store(false)
	if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err != nil {
		t.Fatalf("不脏时应零 git 调用（不得报错）: %v", err)
	}
	// 反证：脏 → 确有 git 调用（此处 git 不可用 → 报错）
	ws.dirty.Store(true)
	if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err == nil {
		t.Fatal("脏且 git 不可用时应报错（证明确有调用）")
	}
}

// TestCheckpointGateOff：history.enabled 非 "true" → 不打点、工具不注册。
func TestCheckpointGateOff(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, false, "", "")
	ws := x.ws(t)
	ws.dirty.Store(true)
	if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err != nil {
		t.Fatalf("未启用时应放行: %v", err)
	}
	if refExists(t, wd, chainSlug("r1")) {
		t.Fatal("未启用不应打点")
	}
	if x.h.registered {
		t.Fatal("未启用不应注册工具")
	}
}

// TestToolRegistrationGating：启用且 workdir 有 .git 才注册（且声明 pre_hook_subject）；否则摘除。
func TestToolRegistrationGating(t *testing.T) {
	// 有 .git + 启用 → 注册 4 个工具，均带 pre_hook_subject
	x := newHarnessWithWd(t, newRepo(t), true, "", "")
	if !x.h.registered {
		t.Fatal("启用且是 git 仓库时应注册工具")
	}
	regs := x.reg.regPayloads()
	if len(regs) != len(historyTools) {
		t.Fatalf("注册工具数 = %d，期望 %d", len(regs), len(historyTools))
	}
	for _, p := range regs {
		if p["pre_hook_subject"] != preHookSubject {
			t.Fatalf("注册载荷缺 pre_hook_subject: %+v", p)
		}
		if p["category"] != "self" {
			t.Fatalf("category 应为 self: %+v", p)
		}
	}
	// 无 .git + 启用 → 不注册（工具摘除）
	x2 := newHarnessWithWd(t, t.TempDir(), true, "", "")
	if x2.h.registered {
		t.Fatal("无 .git 不应注册工具")
	}
	if len(x2.reg.regPayloads()) != 0 {
		t.Fatal("无 .git 不应发注册载荷")
	}
}

// TestPruneKeepLimit：超过 keep → 修剪至 keep，链保持完整连续。
func TestPruneKeepLimit(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "3", "")
	for i := 0; i < 5; i++ {
		writeFileT(t, wd, "f.txt", fmt.Sprintf("v%d", i))
		// 每步一个独立轮次 → 保底项（当前轮/上一轮起点）落在最近两点内，只按 keep 修剪
		x.cp(t, "rootK", fmt.Sprintf("tool%d", i), fmt.Sprintf("turn%d", i))
	}
	slug := chainSlug("rootK")
	if n := chainLen(t, wd, slug); n != 3 {
		t.Fatalf("keep=3 修剪后链长 = %d，期望 3", n)
	}
	lines := strings.Split(strings.TrimRight(gitT(t, wd, "rev-list", "--parents", chainRefPrefix+slug), "\n"), "\n")
	if len(lines) != 3 || len(strings.Fields(lines[len(lines)-1])) != 1 {
		t.Fatalf("修剪后链不完整: %v", lines)
	}
}

// TestPruneKeepWindowPreservesIDs：连续前缀修剪走 `git replace --graft`（O(1)、不逐点重建）——
// 保留点的 commit id **不变**（旧的“按保留段重建”会换 id）。
func TestPruneKeepWindowPreservesIDs(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "3", "")
	for i := 0; i < 4; i++ {
		writeFileT(t, wd, "f.txt", fmt.Sprintf("v%d", i))
		x.cp(t, "rootID", fmt.Sprintf("tool%d", i), fmt.Sprintf("turn%d", i))
	}
	slug := chainSlug("rootID")
	before := strings.Fields(strings.TrimSpace(gitT(t, wd, "rev-list", chainRefPrefix+slug)))
	if len(before) != 3 {
		t.Fatalf("前置：keep=3 应有 3 点，got %d", len(before))
	}
	prevHead := before[0] // 最新点
	// 再打一点 → 触发连续前缀修剪；上一最新点作为保留点保留、其 id 必须不变。
	writeFileT(t, wd, "f.txt", "v4")
	x.cp(t, "rootID", "tool4", "turn4")
	after := strings.Fields(strings.TrimSpace(gitT(t, wd, "rev-list", chainRefPrefix+slug)))
	if len(after) != 3 {
		t.Fatalf("修剪后应保留 3 点，got %d", len(after))
	}
	if after[1] != prevHead {
		t.Fatalf("连续前缀修剪应保留原 commit id（graft 不换 id），got %s want %s", after[1], prevHead)
	}
}

// TestPruneTwoSessionsNoGraftUndo：多会话共享一个 workState——替换引用**按 slug 独立登记**，
// 修剪 B 不得误删 A 的 `git replace --graft` 引用（否则 A 的截断被撤销、链长回涨，两会话 ping-pong）。
func TestPruneTwoSessionsNoGraftUndo(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "2", "7")
	ws := x.ws(t)
	now := time.Now()
	times := []time.Time{
		now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute), now,
	}
	slugA, slugB := "rootAltA", "rootAltB"
	buildDatedChain(t, x.h, ws, slugA, times)
	buildDatedChain(t, x.h, ws, slugB, times)

	// 修剪 A：连续前缀 → `git replace --graft` 解链，链长收敛到 keep=2。
	if _, _, err := x.h.prune(ws, slugA, nil); err != nil {
		t.Fatalf("prune A: %v", err)
	}
	if n := chainLen(t, wd, slugA); n != 2 {
		t.Fatalf("修剪后 A 链长 = %d，期望 2", n)
	}
	// 修剪 B：A 的替换引用须保留（B 只能清自己的）→ A 链长不变（不被撤销截断）。
	if _, _, err := x.h.prune(ws, slugB, nil); err != nil {
		t.Fatalf("prune B: %v", err)
	}
	if n := chainLen(t, wd, slugB); n != 2 {
		t.Fatalf("修剪后 B 链长 = %d，期望 2", n)
	}
	if n := chainLen(t, wd, slugA); n != 2 {
		t.Fatalf("修剪 B 撤销了 A 的截断（A 链长 = %d，期望仍为 2）——替换引用未按 slug 分离", n)
	}
}

// TestPruneTTLWindow：以**链上最新点**为锚点，超 ttl 窗口的旧点被清（keep 未超也会触发）。
func TestPruneTTLWindow(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "100", "7")
	ws := x.ws(t)
	now := time.Now()
	times := []time.Time{
		now.AddDate(0, 0, -30), now.AddDate(0, 0, -29), now.AddDate(0, 0, -28), now, now,
	}
	// 造 5 点：前 3 个 30/29/28 天前（在 now-7d 之外）→ 应清；后 2 个 now → 保留。
	recs := buildDatedChain(t, x.h, ws, "rootT", times)
	if len(recs) != 5 {
		t.Fatal("前置：应造 5 个点")
	}
	if _, _, err := x.h.prune(ws, "rootT", nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := chainLen(t, wd, "rootT"); n != 2 {
		t.Fatalf("ttl 修剪后链长 = %d，期望 2", n)
	}
	// 被保留的是最近两点（message 含 tool3/tool4）
	msgs := chainMsgs(t, wd, "rootT", 5)
	for _, m := range msgs {
		if strings.Contains(m, "tool0") || strings.Contains(m, "tool1") || strings.Contains(m, "tool2") {
			t.Fatalf("超窗口的旧点应被清: %s", m)
		}
	}
}

// TestPruneAnchorIsLatestPoint：整条链都很旧（2 个月前）但**锚点 = 链上最新点** → 不得被清。
func TestPruneAnchorIsLatestPoint(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "100", "7")
	ws := x.ws(t)
	base := time.Now().AddDate(0, 0, -60)
	times := make([]time.Time, 0, 5)
	for i := 0; i < 5; i++ {
		times = append(times, base.Add(time.Duration(i)*time.Minute))
	}
	buildDatedChain(t, x.h, ws, "rootOld", times)
	// 若锚点是 now（错误口径），5 个点全在 now-7d 之外 → 会被清空；锚点=最新点 → 全部保留。
	if _, _, err := x.h.prune(ws, "rootOld", nil); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := chainLen(t, wd, "rootOld"); n != 5 {
		t.Fatalf("闲置链不得被清（锚点=最新点）：链长 = %d，期望 5", n)
	}
}

// TestPruneProtectedNeverDeleted：保底项（当前轮/上一轮）永不删。
func TestPruneProtectedNeverDeleted(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "1", "7")
	ws := x.ws(t)
	now := time.Now()
	recs := buildDatedChain(t, x.h, ws, "rootP", []time.Time{now.Add(-4 * time.Minute), now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now.Add(-1 * time.Minute)})
	// recs 为**旧→新**序：recs[0] = 最旧点（模拟"上一轮"保底项）
	protected := map[string]bool{recs[0].id: true}
	if _, _, err := x.h.prune(ws, "rootP", protected); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := chainLen(t, wd, "rootP"); n != 2 {
		t.Fatalf("keep=1 + 1 个保底项 → 链长应为 2，got %d", n)
	}
	msgs := strings.Join(chainMsgs(t, wd, "rootP", 5), "\n")
	if !strings.Contains(msgs, "tool0") {
		t.Fatalf("保底项（tool0）不得被删: %s", msgs)
	}
}

// TestFuseOnConsecutiveFailures：连续 3 次失败 → fused（放行）；成功后复位。
func TestFuseOnConsecutiveFailures(t *testing.T) {
	x := newHarnessWithWd(t, newRepo(t), true, "", "")
	ws := x.ws(t)
	old := os.Getenv("PATH")
	if err := os.Setenv("PATH", t.TempDir()); err != nil { // git 不可用 → 制造失败
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ws.dirty.Store(true)
		if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err == nil {
			t.Fatalf("第 %d 次应失败", i+1)
		}
	}
	if !ws.fused {
		t.Fatal("连续 3 次失败应进入 fused")
	}
	// 熔断 → 放行（不再拦截，即使脏）
	ws.dirty.Store(true)
	if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err != nil {
		t.Fatalf("熔断后应放行: %v", err)
	}
	// 恢复 git → 成功一次即复位
	if err := os.Setenv("PATH", old); err != nil {
		t.Fatal(err)
	}
	ws.dirty.Store(true)
	if err := x.h.checkpointSync(ws, "r1", "tool", "t1", false); err != nil {
		t.Fatalf("恢复后应成功: %v", err)
	}
	if ws.fused || ws.failCount != 0 {
		t.Fatalf("成功后应复位: fused=%v failCount=%d", ws.fused, ws.failCount)
	}
}

// TestParentChildSessionShareChain：top_session 优先 → 父子会话共享一条链。
func TestParentChildSessionShareChain(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	// 父子会话：callback 的 top_session 均为 rootShared → 写同一条链
	writeFileT(t, wd, "p.txt", "v1")
	x.cp(t, "rootShared", "toolA", "turn1")
	writeFileT(t, wd, "p.txt", "v2")
	x.cp(t, "rootShared", "toolB", "turn1")
	if !refExists(t, wd, "rootShared") {
		t.Fatal("共享链 ref 未建立")
	}
	if chainLen(t, wd, "rootShared") != 2 {
		t.Fatalf("共享链长应为 2")
	}
	if refExists(t, wd, "sub1") || refExists(t, wd, "sub2") {
		t.Fatal("不应为子会话单独建链")
	}
}

// TestRestoreSingleFileAndGuards：单文件回滚成功 / 一致性不通过拒绝 / 删除恢复 / 空路径与目录拒绝。
func TestRestoreSingleFileAndGuards(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	writeFileT(t, wd, "a.txt", "v1")
	x.cp(t, "rootR", "tool1", "t1") // 检查点 A
	writeFileT(t, wd, "a.txt", "v2")
	x.cp(t, "rootR", "tool2", "t1") // 检查点 B（链头）

	// ① 单文件成功：磁盘 v2 == 链头内容 → 允许；回到 A（v1）
	text, isErr := x.callTool(t, "rootR", "history_restore", map[string]any{"path": "a.txt", "to": -2})
	if isErr {
		t.Fatalf("应成功，got: %s", text)
	}
	if got := readFileT(t, wd, "a.txt"); got != "v1" {
		t.Fatalf("回滚后内容 = %q，期望 v1", got)
	}

	// ② 一致性不通过：磁盘被本会话之外改动（v9 != 链头 v2）→ 拒绝
	writeFileT(t, wd, "a.txt", "v9")
	text, isErr = x.callTool(t, "rootR", "history_restore", map[string]any{"path": "a.txt", "to": -2})
	if !isErr || !strings.Contains(text, "在本会话之外被改动过") {
		t.Fatalf("应因一致性校验拒绝，got isErr=%v text=%s", isErr, text)
	}
	if got := readFileT(t, wd, "a.txt"); got != "v9" {
		t.Fatal("拒绝时不得改动文件")
	}

	// ③ 删除恢复：文件被删除 → 允许从检查点恢复（链头 B 的 v2）
	if err := os.Remove(filepath.Join(wd, "a.txt")); err != nil {
		t.Fatal(err)
	}
	text, isErr = x.callTool(t, "rootR", "history_restore", map[string]any{"path": "a.txt"})
	if isErr {
		t.Fatalf("删除恢复应成功，got: %s", text)
	}
	if got := readFileT(t, wd, "a.txt"); got != "v2" {
		t.Fatalf("删除恢复后内容 = %q，期望 v2", got)
	}

	// ④ path 空 → 拒绝
	if _, isErr := x.callTool(t, "rootR", "history_restore", map[string]any{"path": ""}); !isErr {
		t.Fatal("空 path 应拒绝")
	}
	// ⑤ path 为目录 → 拒绝
	if err := os.MkdirAll(filepath.Join(wd, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, isErr := x.callTool(t, "rootR", "history_restore", map[string]any{"path": "sub"}); !isErr {
		t.Fatal("目录应拒绝")
	}
}

// TestRestoreCRLFAutocrlfConsistency：`core.autocrlf=true`（Git for Windows 默认）下工作区为 CRLF、
// 检查点 blob 为 LF——一致性校验须**交给 git 判定**（git 自身视为未改动），
// 不得因原始字节差异误判"链外改动"而恒拒回滚。
func TestRestoreCRLFAutocrlfConsistency(t *testing.T) {
	wd := newRepo(t)
	gitT(t, wd, "config", "core.autocrlf", "true")
	x := newHarnessWithWd(t, wd, true, "", "")

	// 造链：A = v1（CRLF），B = v2（CRLF，链头）；git add 时行尾归一 → blob 为 LF。
	writeFileT(t, wd, "crlf.txt", "v1\r\n")
	x.cp(t, "rootCRLF", "tool1", "t1") // A
	writeFileT(t, wd, "crlf.txt", "v2\r\n")
	x.cp(t, "rootCRLF", "tool2", "t1") // B（链头）
	head := strings.TrimSpace(gitT(t, wd, "rev-parse", chainRefPrefix+chainSlug("rootCRLF")))

	// 复现根因：盘面 CRLF（v2\r\n）与链头 blob LF（v2\n）**原始字节不等**——旧实现据此误判"有改动"。
	if disk := readFileT(t, wd, "crlf.txt"); disk != "v2\r\n" {
		t.Fatalf("前置：盘面应为 CRLF v2，实际 %q", disk)
	}
	if blob := gitT(t, wd, "show", head+":crlf.txt"); blob != "v2\n" {
		t.Fatalf("前置：链头 blob 应为 LF v2（行尾已被 git 归一），实际 %q", blob)
	}
	// 无链外改动 → git 判一致 → 允许回滚（回到 A 的 v1；写回 = blob 原始字节 LF）。
	text, isErr := x.callTool(t, "rootCRLF", "history_restore", map[string]any{"path": "crlf.txt", "to": -2})
	if isErr {
		t.Fatalf("CRLF 工作区在无链外改动时应成功回滚，got: %s", text)
	}
	if got := readFileT(t, wd, "crlf.txt"); got != "v1\n" {
		t.Fatalf("回滚后内容 = %q，期望 %q", got, "v1\n")
	}

	// 反向：真的有链外改动（v9，归一后仍 != 链头 v2）→ 仍拒绝，且不落盘。
	writeFileT(t, wd, "crlf.txt", "v9\r\n")
	text, isErr = x.callTool(t, "rootCRLF", "history_restore", map[string]any{"path": "crlf.txt", "to": -2})
	if !isErr || !strings.Contains(text, "在本会话之外被改动过") {
		t.Fatalf("链外改动应拒绝，got isErr=%v text=%s", isErr, text)
	}
	if got := readFileT(t, wd, "crlf.txt"); got != "v9\r\n" {
		t.Fatalf("拒绝时不得改动文件，实际 %q", got)
	}

	// 删除恢复仍通过：文件被删除 → 允许从链头（B = v2）恢复。
	if err := os.Remove(filepath.Join(wd, "crlf.txt")); err != nil {
		t.Fatal(err)
	}
	text, isErr = x.callTool(t, "rootCRLF", "history_restore", map[string]any{"path": "crlf.txt"})
	if isErr {
		t.Fatalf("删除恢复应成功，got: %s", text)
	}
	if got := readFileT(t, wd, "crlf.txt"); got != "v2\n" {
		t.Fatalf("删除恢复后内容 = %q，期望 %q", got, "v2\n")
	}
}

// TestRestoreGitDiffSemanticsWhenHeadLacksFile：工作区存在该文件、链头未收录（相对链头为未跟踪）时，
// 一致性判定**完全交给 git**（`git diff --quiet <链头> -- <path>` 对未跟踪路径不报差异 → 退出码 0）
// → 允许回滚，与 spec §3.2「一致性校验 = 文件已删除 **或** git diff --quiet 为 0」口径一致。
func TestRestoreGitDiffSemanticsWhenHeadLacksFile(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	writeFileT(t, wd, "u.txt", "v1")
	x.cp(t, "rootU", "tool1", "t1") // A：收录 u.txt
	if err := os.Remove(filepath.Join(wd, "u.txt")); err != nil {
		t.Fatal(err)
	}
	x.cp(t, "rootU", "tool2", "t1") // B（链头）：不含 u.txt
	// 会话外重建（相对链头未跟踪；git diff 不报差异）
	writeFileT(t, wd, "u.txt", "v1")

	if _, isErr := x.callTool(t, "rootU", "history_restore", map[string]any{"path": "u.txt", "to": -2}); isErr {
		t.Fatal("链头未收录时 git diff 判一致 → 应允许（spec §3.2 口径）")
	}
	if got := readFileT(t, wd, "u.txt"); got != "v1" {
		t.Fatalf("应写回目标检查点（A）内容 v1，实际 %q", got)
	}
}

// TestBackfillBypassesDirtyDebounceAndSkipsUnchanged：轮末补点**不依赖脏位**（force）——
// 覆盖「工具改文件 → 轮次结束」间隔 < filesys.changed 去抖窗口、脏位尚未置上的竞态；
// 并断言**确实无变化时**不产生空点（链长不变）。
func TestBackfillBypassesDirtyDebounceAndSkipsUnchanged(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	ws := x.ws(t)
	slug := chainSlug("rootB")

	// 前置钩子路径建首点。
	writeFileT(t, wd, "b.txt", "v1")
	x.cp(t, "rootB", "tool1", "turn1")
	if n := chainLen(t, wd, slug); n != 1 {
		t.Fatalf("前置：链长应为 1，got %d", n)
	}

	// 模拟去抖竞态：改文件后**不置脏位**直接轮末补点 → 仍应产生新点且内容正确。
	writeFileT(t, wd, "b.txt", "v2")
	ws.dirty.Store(false) // 补点前置条件不成立（去抖窗口内的真实状态）
	x.forceCp(t, "rootB", "session-complete", "turn1")
	if n := chainLen(t, wd, slug); n != 2 {
		t.Fatalf("轮末补点应不依赖脏位、产生新点，got 链长 %d", n)
	}
	head := strings.TrimSpace(gitT(t, wd, "rev-parse", chainRefPrefix+slug))
	if blob := gitT(t, wd, "show", head+":b.txt"); blob != "v2" {
		t.Fatalf("新点内容应为 v2，got %q", blob)
	}

	// 确实无任何变化 → 轮末补点不建空点（链长不变）。
	ws.dirty.Store(false)
	x.forceCp(t, "rootB", "session-complete", "turn1")
	if n := chainLen(t, wd, slug); n != 2 {
		t.Fatalf("无变化时轮末补点不得产生空点，got 链长 %d", n)
	}
	if msgs := chainMsgs(t, wd, slug, 5); len(msgs) != 2 {
		t.Fatalf("链上不应有空点，msgs=%v", msgs)
	}
}

// TestDiffAndShow：diff 文本 / show 完整内容 / 相对步与 turn-start。
func TestDiffAndShow(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	writeFileT(t, wd, "a.txt", "v1")
	x.cp(t, "rootD", "tool1", "turn1")
	writeFileT(t, wd, "a.txt", "v2")
	x.cp(t, "rootD", "tool2", "turn1")

	text, isErr := x.callTool(t, "rootD", "history_diff", map[string]any{"to": -1})
	if isErr || !strings.Contains(text, "-v1") || !strings.Contains(text, "+v2") {
		t.Fatalf("diff 不符: isErr=%v text=%s", isErr, text)
	}
	text, isErr = x.callTool(t, "rootD", "history_show", map[string]any{"to": -2, "path": "a.txt"})
	if isErr || strings.TrimSpace(text) != "v1" {
		t.Fatalf("show 不符: isErr=%v text=%q", isErr, text)
	}
	// turn-start：本轮首点 = tool1 所在检查点 → 该文件内容 v1
	text, isErr = x.callTool(t, "rootD", "history_show", map[string]any{"to": "turn-start", "path": "a.txt"})
	if isErr || strings.TrimSpace(text) != "v1" {
		t.Fatalf("turn-start 解析不符: isErr=%v text=%q", isErr, text)
	}
	// 状态工具：链 2 步、相对编号 -1/-2
	text, isErr = x.callTool(t, "rootD", "history_status", map[string]any{})
	if isErr || !strings.Contains(text, "\"checkpointCount\": 2") || !strings.Contains(text, "\"n\": -1") {
		t.Fatalf("status 不符: isErr=%v text=%s", isErr, text)
	}
}

// TestMessageDrivenCheckpointing：经 filesys.changed 置脏 + 前置钩子打点；session-complete 轮末补点。
func TestMessageDrivenCheckpointing(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	// filesys.changed 置脏
	writeFileT(t, wd, "m.txt", "v1")
	emitJSON(t, x.bus, filesysChangedSubject, map[string]any{"work_dir": wd, "path": "m.txt", "operation": "write"})
	// 前置钩子（主会话，无 top_session → 用 session 作根）
	emitJSON(t, x.bus, preHookSubject, map[string]any{
		"tool": "filesys_run",
		"context": map[string]any{
			"instance_id": testInstance, "session": "sessM", "turn": "turnM",
		},
	})
	if n := chainLen(t, wd, chainSlug("sessM")); n != 1 {
		t.Fatalf("前置钩子应打点 1 次，got %d", n)
	}
	// 再变更 + session-complete（主轮次）→ 轮末补点
	writeFileT(t, wd, "m.txt", "v2")
	emitJSON(t, x.bus, filesysChangedSubject, map[string]any{"work_dir": wd, "path": "m.txt", "operation": "write"})
	emitJSON(t, x.bus, completeSubject, map[string]any{"session": "sessM", "turn": "turnM", "status": "complete"})
	if n := chainLen(t, wd, chainSlug("sessM")); n != 2 {
		t.Fatalf("轮末补点后链长应为 2，got %d", n)
	}
	// 子会话（top_session != session）不补点
	writeFileT(t, wd, "m.txt", "v3")
	emitJSON(t, x.bus, filesysChangedSubject, map[string]any{"work_dir": wd, "path": "m.txt"})
	emitJSON(t, x.bus, preHookSubject, map[string]any{
		"tool": "filesys_run",
		"context": map[string]any{
			"instance_id": testInstance, "session": "sessSub", "top_session": "sessM", "turn": "turnM",
		},
	})
	before := chainLen(t, wd, chainSlug("sessM"))
	emitJSON(t, x.bus, completeSubject, map[string]any{"session": "sessSub", "turn": "turnM", "status": "complete"})
	if after := chainLen(t, wd, chainSlug("sessM")); after != before {
		t.Fatalf("子会话不应触发轮末补点: %d → %d", before, after)
	}
}

// TestTouchFilesSkipCheckpoint：「涉及文件变动」= false → 前置钩子放行、不打点；true / 缺省 → 打点；
// 轮末补点仍发生（保证总有产像）。用户口径 2026-09-28。
func TestTouchFilesSkipCheckpoint(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	ws := x.ws(t)
	slug := chainSlug("sessTF")

	// ① 不涉及（touch_files=false）：脏位已置 → 若未跳过必然打点
	writeFileT(t, wd, "t.txt", "v1")
	ws.dirty.Store(true)
	emitJSON(t, x.bus, preHookSubject, map[string]any{
		"tool": "self_file_read", "touch_files": false,
		"context": map[string]any{"instance_id": testInstance, "session": "sessTF", "turn": "t1"},
	})
	if refExists(t, wd, slug) {
		t.Fatal("touch_files=false 不应打点（省 git 进程）")
	}

	// ② 已登记会话归属 → 轮末补点（force）照常保底
	emitJSON(t, x.bus, completeSubject, map[string]any{"session": "sessTF", "turn": "t1", "status": "complete"})
	if n := chainLen(t, wd, slug); n != 1 {
		t.Fatalf("轮末补点应保底产生 1 个检查点，got %d", n)
	}

	// ③ 涉及（touch_files=true）→ 打点
	writeFileT(t, wd, "t.txt", "v2")
	ws.dirty.Store(true)
	emitJSON(t, x.bus, preHookSubject, map[string]any{
		"tool": "self_filesys_run", "touch_files": true,
		"context": map[string]any{"instance_id": testInstance, "session": "sessTF", "turn": "t2"},
	})
	if n := chainLen(t, wd, slug); n != 2 {
		t.Fatalf("touch_files=true 应打点：链长 = %d，期望 2", n)
	}

	// ④ 缺省（载荷无 touch_files 字段）→ 与改前一致（打点）
	writeFileT(t, wd, "t.txt", "v3")
	ws.dirty.Store(true)
	emitJSON(t, x.bus, preHookSubject, map[string]any{
		"tool":    "self_unknown_tool",
		"context": map[string]any{"instance_id": testInstance, "session": "sessTF", "turn": "t3"},
	})
	if n := chainLen(t, wd, slug); n != 3 {
		t.Fatalf("缺省（无 touch_files）应按涉及打点：链长 = %d，期望 3", n)
	}
}

// TestStatusTimelinePerSessionAndClearTarget：会话 A/B 各自独立回写 status/timeline（互不覆盖）；
// history.clear（JSON `{ts,session}`）只清**目标会话**的链，其它链保留（I-135 / I-136 闭环）。
func TestStatusTimelinePerSessionAndClearTarget(t *testing.T) {
	wd := newRepo(t)
	x := newHarnessWithWd(t, wd, true, "", "")
	writeFileT(t, wd, "a.txt", "a1")
	x.cp(t, "rootA", "toolA", "t1")
	writeFileT(t, wd, "b.txt", "b1")
	x.cp(t, "rootB", "toolB", "t1")
	slugA, slugB := chainSlug("rootA"), chainSlug("rootB")

	var stA, stB chainStatus
	if err := json.Unmarshal([]byte(x.reg.savedVal(statusKeyPrefix+slugA)), &stA); err != nil {
		t.Fatalf("A 状态应回写到 status.<slugA>：%v", err)
	}
	if err := json.Unmarshal([]byte(x.reg.savedVal(statusKeyPrefix+slugB)), &stB); err != nil {
		t.Fatalf("B 状态应回写到 status.<slugB>：%v", err)
	}
	if stA.CheckpointCount != 1 || stB.CheckpointCount != 1 {
		t.Fatalf("A/B 各自 status 应独立（互不覆盖）：A=%+v B=%+v", stA, stB)
	}
	var tlA, tlB []timelineEntry
	_ = json.Unmarshal([]byte(x.reg.savedVal(timelineKeyPrefix+slugA)), &tlA)
	_ = json.Unmarshal([]byte(x.reg.savedVal(timelineKeyPrefix+slugB)), &tlB)
	if len(tlA) != 1 || tlA[0].Tool != "toolA" || tlA[0].Session != slugA {
		t.Fatalf("A 时间线不符：%+v", tlA)
	}
	if len(tlB) != 1 || tlB[0].Tool != "toolB" || tlB[0].Session != slugB {
		t.Fatalf("B 时间线不符：%+v", tlB)
	}

	// history.clear = {ts, session:rootA} → 只清 A
	emitJSON(t, x.bus, prjConfigRefreshSubject, map[string]any{
		"id": clearKey, "op": "save",
		"list": map[string]any{clearKey: `{"ts":"2026-09-28T00:00:00Z","session":"rootA"}`},
	})
	if refExists(t, wd, slugA) {
		t.Fatal("history.clear 应清空目标会话的链（rootA）")
	}
	if !refExists(t, wd, slugB) {
		t.Fatal("其它会话的链应保留（rootB）")
	}
	// 清空后回写该会话空状态/空时间线（且不携带其它会话的最近打点时间）；B 不受影响
	var stA2 chainStatus
	if err := json.Unmarshal([]byte(x.reg.savedVal(statusKeyPrefix+slugA)), &stA2); err != nil {
		t.Fatalf("清空后应回写 status.<slugA>：%v", err)
	}
	if stA2.CheckpointCount != 0 {
		t.Fatalf("清空后 A 的 checkpointCount 应为 0，got %d", stA2.CheckpointCount)
	}
	if stA2.LastCheckpointAt != "" {
		t.Fatalf("清空后 A 不应显示其它会话的最近打点时间，got %q", stA2.LastCheckpointAt)
	}
	var tlA2 []timelineEntry
	if err := json.Unmarshal([]byte(x.reg.savedVal(timelineKeyPrefix+slugA)), &tlA2); err != nil {
		t.Fatalf("清空后应回写 timeline.<slugA>：%v", err)
	}
	if len(tlA2) != 0 {
		t.Fatalf("清空后 A 的时间线应为空，got %+v", tlA2)
	}
	if x.reg.savedVal(timelineKeyPrefix+slugB) == "" {
		t.Fatal("B 的时间线不应被清空动作覆盖")
	}
}
