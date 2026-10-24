// chain.go：检查点链的内核——打点（临时 index → write-tree → commit-tree → update-ref）、
// 修剪（重建保留段）、解析（相对步 / turn-start / 绝对 commit）、diff/show/restore、
// 以及 history.status / history.timeline 回写。
//
// **绝不碰 `.git/index`、绝不碰 HEAD**：所有写操作经 `GIT_INDEX_FILE=<workdir>/.chonkpilot/history/index`
// 与 `refs/chonkpilot/<slug>`。
package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chonkpilot/chonkpilot-lib/winproc"
	"github.com/chonkpilot/chonkpilot-plugin/dataclient"
)

// chainRec 链上一个检查点的原始信息（重建保留段时复用 tree/message/时间）。
type chainRec struct {
	id   string
	tree string
	at   time.Time // author time
	ct   time.Time // committer time
	msg  string
}

// retained 判定该检查点是否保留（idx = 新→旧 0 基序号）：
//
//	① 最新 keep 个 **且** ② 距锚点（链上最新点时间）不超 ttl 天 —— 两者同时满足；
//	③ 保底项（当前轮 / 上一轮起点）**永不删**。
//
// 「每会话最近 1 个」保底：一条链 = 一个根会话（chainSlug），其"最近 1 个"恒为链头，
// 已被规则 ① 覆盖，故无需单列（本函数只额外接收当前轮/上一轮锚点）。
func (r chainRec) retained(idx, keep int, limit time.Time, protected map[string]bool) bool {
	if protected[r.id] {
		return true
	}
	return idx < keep && !r.at.Before(limit)
}

// chainSlug 根会话 → ref slug（refs 组件须为安全字符；非 [A-Za-z0-9._-] 一律折为 '_'）。
func chainSlug(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range root {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" || s == "." || s == ".." {
		return "default"
	}
	return s
}

// ─── git 执行（统一注入临时 index + 固定身份，避免依赖用户 git config）──

func gitBaseEnv(index string) []string {
	return []string{
		"GIT_INDEX_FILE=" + index,
		"GIT_AUTHOR_NAME=chonkpilot",
		"GIT_AUTHOR_EMAIL=chonkpilot@chonkpilot.local",
		"GIT_COMMITTER_NAME=chonkpilot",
		"GIT_COMMITTER_EMAIL=chonkpilot@chonkpilot.local",
	}
}

// git 执行 git -C <workdir> <args...>（注入 GIT_INDEX_FILE），返回合并输出。
func (h *History) git(ws *workState, args ...string) (string, error) {
	return h.gitEnv(ws, nil, args...)
}

// gitEnv 同上，可追加 env（如 GIT_AUTHOR_DATE / GIT_COMMITTER_DATE）。
func (h *History) gitEnv(ws *workState, extra []string, args ...string) (string, error) {
	out, err := h.gitRun(ws, extra, args...)
	if err != nil {
		return out, err
	}
	return out, nil
}

// gitRun 底层执行：合并 stdout+stderr；失败时把输出并入 error（便于 status.lastError 诊断）。
// 带 gitTimeout 上限：git 卡死时不再无限持有 ws.mu（否则阻塞同 workdir 后续打点/查询）。
func (h *History) gitRun(ws *workState, extra []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	all := append([]string{"-C", ws.workDir}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Env = append(os.Environ(), gitBaseEnv(ws.index)...)
	cmd.Env = append(cmd.Env, extra...)
	// 宿主为 windowsgui 无控制台：隐藏 git.exe 的控制台窗口。
	cmd.SysProcAttr = winproc.SysProcAttr()
	out, err := cmd.CombinedOutput()
	s := string(out)
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(s))
	}
	return s, nil
}

// cappedBuffer 是带上限的字节缓冲：写入超过上限的部分丢弃（仍返回已消费长度，避免 git 因
// 管道写满阻塞），把大 blob 的读取内存钉在上限内；over 标记是否发生过丢弃（= 被截断）。
type cappedBuffer struct {
	buf   bytes.Buffer
	limit int // >0 生效
	over  bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return w.buf.Write(p)
	}
	remain := w.limit - w.buf.Len()
	if remain <= 0 {
		w.over = true
		return len(p), nil
	}
	if len(p) > remain {
		w.buf.Write(p[:remain])
		w.over = true
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// gitRaw 执行只读 git 并以**原始字节**返回 stdout（用于读文件内容，避免编码转换）。
// maxBytes>0 时最多保留前 maxBytes 字节（超出丢弃，避免大文件整读入内存）；返回是否被截断。
// 带 gitTimeout 上限（同 gitRun）。
func (h *History) gitRaw(ws *workState, maxBytes int, args ...string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	all := append([]string{"-C", ws.workDir}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Env = append(os.Environ(), gitBaseEnv(ws.index)...)
	cmd.SysProcAttr = winproc.SysProcAttr()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out := &cappedBuffer{limit: maxBytes}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, false, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.buf.Bytes(), out.over, nil
}

// ─── 打点 ────────────────────────────────────────────────

// checkpointSync 同步打点（前置钩子 / 轮末补点共用）。
//
// 门控：未启用 / 非 git 仓库 → 直接返回 nil（**零 git 调用**，放行）；
// 前置打点钩子（force=false）额外要求脏=true（脏=false → 零 git 调用放行）；
// 轮末补点（force=true）**不看脏位**——避免被 `filesys.changed` 的 60ms 去抖竞态跳过，
// 强制走一次打点流程，由 doCheckpoint 以「新 tree == 链头 tree」跳过无变化的建点（不产生空点）。
// 熔断（fused）→ **仍尝试打点**（放行、不拦截工具调用；成功一次即复位），失败亦返回 nil；
// 非熔断态的真实打点失败 → 返回 error（前置钩子据此拒绝工具调用）。
func (h *History) checkpointSync(ws *workState, root, tool, turn string, force bool) error {
	ws.mu.Lock()
	if !ws.hasGit || !ws.enabled.Load() || (!force && !ws.dirty.Load()) {
		ws.mu.Unlock()
		return nil
	}
	wasFused := ws.fused
	slug := chainSlug(root)
	protected := map[string]bool{}
	if ws.turnStartID != "" {
		protected[ws.turnStartID] = true
	}
	if ws.prevTurnID != "" {
		protected[ws.prevTurnID] = true
	}
	start := time.Now()
	final, idmap, created, err := h.doCheckpoint(ws, slug, tool, protected, force)
	dur := time.Since(start)
	if err != nil {
		ws.failCount++
		if ws.failCount > fuseThreshold {
			ws.failCount = fuseThreshold
		}
		fails := ws.failCount
		ws.lastErr = err.Error()
		if fails >= fuseThreshold {
			ws.fused = true
		}
		ws.mu.Unlock()
		h.logf()("history: 打点失败（workdir=%s，连续 %d 次）：%v", ws.workDir, fails, err)
		h.writeback(ws, slug)
		if wasFused {
			return nil // 熔断态：放行（不拦截工具调用）；下次成功即复位
		}
		return err
	}
	ws.failCount = 0
	ws.fused = false
	ws.lastErr = ""
	if created {
		ws.lastAt = time.Now()
		ws.lastDurMs = dur.Milliseconds()
	}
	ws.lastSlug = slug    // 最近一次打点所属链（按会话回写状态的归属判定）
	ws.dirty.Store(false) // 打点流程完成（建点 / 确认无变化）→ 清脏位
	// 修剪重建后检查点 id 会变 → 按旧→新映射重定位轮次锚点。
	if idmap != nil {
		if n, ok := idmap[ws.turnStartID]; ok {
			ws.turnStartID = n
		}
		if n, ok := idmap[ws.prevTurnID]; ok {
			ws.prevTurnID = n
		}
	}
	if turn != "" && turn != ws.lastTurn {
		ws.prevTurnID = ws.turnStartID
		ws.lastTurn = turn
		ws.turnStartID = final
	} else if ws.turnStartID == "" {
		ws.turnStartID = final
	}
	ws.mu.Unlock()
	h.writeback(ws, slug)
	return nil
}

// doCheckpoint 执行一次打点（须持 ws.mu）：临时 index add → write-tree → commit-tree → update-ref
// → 修剪。返回链头（修剪后；**跳过建点时 = 现有链头**）、旧→新 id 映射（未修剪则 nil）、
// 是否真正建了点（false = 因内容未变被跳过）。
//
// skipIfUnchanged（轮末补点=true）：`git add -A` + `write-tree` 后，若新 tree == 链头 tree
// → 直接返回，**不建 commit、不 update-ref、不产生空点**；否则照常建点。
// 前置钩子（false）保持原语义：脏即建点，不做内容比较（避免额外 git 调用）。
func (h *History) doCheckpoint(ws *workState, slug, tool string, protected map[string]bool, skipIfUnchanged bool) (string, map[string]string, bool, error) {
	if err := os.MkdirAll(filepath.Dir(ws.index), 0o755); err != nil {
		return "", nil, false, fmt.Errorf("建临时 index 目录失败：%w", err)
	}
	h.ensureGitignore(ws.workDir)
	// 尊重 .gitignore（含删除）；gitlink 不递归（git add 默认不进入子模块）。
	if _, err := h.git(ws, "add", "-A"); err != nil {
		return "", nil, false, err
	}
	tree, err := h.git(ws, "write-tree")
	if err != nil {
		return "", nil, false, err
	}
	tree = strings.TrimSpace(tree)
	if tree == "" {
		return "", nil, false, errors.New("write-tree 返回空 tree")
	}
	ref := chainRefPrefix + slug
	prev := ""
	if out, err := h.git(ws, "rev-parse", "--verify", "--quiet", ref); err == nil {
		prev = strings.TrimSpace(out)
	}
	// 内容未变（新 tree == 链头 tree）→ 跳过建点（不产生空点；链头即返回值）。
	if skipIfUnchanged && prev != "" {
		if out, err := h.git(ws, "rev-parse", "--verify", "--quiet", prev+"^{tree}"); err == nil &&
			strings.TrimSpace(out) == tree {
			return prev, nil, false, nil
		}
	}
	msg := fmt.Sprintf("chonk-ckpt: session=%s tool=%s ts=%s", slug, tool, time.Now().Format(time.RFC3339))
	args := []string{"commit-tree", tree}
	if prev != "" {
		args = append(args, "-p", prev)
	}
	args = append(args, "-m", msg)
	out, err := h.git(ws, args...)
	if err != nil {
		return "", nil, false, err
	}
	commit := strings.TrimSpace(out)
	if _, err := h.git(ws, "update-ref", ref, commit); err != nil {
		return "", nil, false, err
	}
	// 修剪（best-effort：修剪失败不回滚本次打点，仅记日志）。
	final := commit
	var idmap map[string]string
	if nh, im, perr := h.prune(ws, slug, protected); perr != nil {
		h.logf()("history: 修剪失败（workdir=%s）：%v", ws.workDir, perr)
	} else if nh != "" {
		final, idmap = nh, im
	}
	return final, idmap, true, nil
}

// ─── 修剪（解链截断 / 重建保留段）────────────────────────

// prune 执行修剪：丢弃链上最老、超出保留集的点（ref 保持指向链头；旧点不可达 → 交给 git 自动 gc，
// **不主动跑 gc/prune**）。无需修剪 → 返回 ("", nil, nil)。
//
// 常见情形（保留集 = 最新连续前缀，仅按 keep / ttl 截断）：用一次 `git replace --graft` 把最老保留点
// 与将被丢弃的旧前缀解链即可（O(1)，不逐个 commit-tree 重放）——解链后更老祖先不可达，链长即 == 保留数。
// 少见情形（保底项比 keep 窗口更旧 → 保留集非连续）：回退为“重建保留段”（复用 tree/message/时间，
// ref 指向新链头，旧→新 id 映射经 idmap 回传）。
func (h *History) prune(ws *workState, slug string, protected map[string]bool) (string, map[string]string, error) {
	ref := chainRefPrefix + slug
	keep := h.keepVal()
	ttl := h.ttlVal()
	recs, err := h.fetchChain(ws, ref, keep, protected)
	if err != nil {
		return "", nil, err
	}
	if len(recs) == 0 {
		return "", nil, nil
	}
	limit := recs[0].at.Add(-time.Duration(ttl) * 24 * time.Hour)
	need := len(recs) > keep
	for i, r := range recs {
		if !r.retained(i, keep, limit, protected) {
			need = true
			break
		}
	}
	if !need {
		return "", nil, nil
	}
	// 保留集是否为「最新连续前缀」（idx < k 全保留、其余全不保留）。
	k := 0
	for k < len(recs) && recs[k].retained(k, keep, limit, protected) {
		k++
	}
	contiguous := k > 0
	for i := k; i < len(recs); i++ {
		if recs[i].retained(i, keep, limit, protected) {
			contiguous = false
			break
		}
	}
	if contiguous {
		if err := h.graftTruncate(ws, slug, recs[k-1].id); err != nil {
			return "", nil, err
		}
		h.logf()("history: 修剪完成（workdir=%s，slug=%s）：保留 %d 个检查点（解链最老前缀，未重放）", ws.workDir, slug, k)
		return recs[0].id, nil, nil // 链头未变（id 不变）→ 无需 idmap
	}
	// 保留段（旧→新）重建。
	idmap := map[string]string{}
	newHead := ""
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		if !r.retained(i, keep, limit, protected) {
			continue
		}
		args := []string{"commit-tree", r.tree}
		if newHead != "" {
			args = append(args, "-p", newHead)
		}
		args = append(args, "-m", r.msg)
		env := []string{
			"GIT_AUTHOR_DATE=" + r.at.Format(time.RFC3339),
			"GIT_COMMITTER_DATE=" + r.ct.Format(time.RFC3339),
		}
		out, err := h.gitEnv(ws, env, args...)
		if err != nil {
			return "", nil, err
		}
		nc := strings.TrimSpace(out)
		idmap[r.id] = nc
		newHead = nc
	}
	if newHead == "" {
		return "", nil, nil
	}
	if _, err := h.git(ws, "update-ref", ref, newHead); err != nil {
		return "", nil, err
	}
	// 重建段为**新建的线性链**（不再需要任何 graft）：清理本会话遗留的替换引用（不依赖内存映射）。
	h.cleanStaleGrafts(ws, slug, "")
	h.logf()("history: 修剪完成（workdir=%s，slug=%s）：保留 %d 个检查点（含保底项）", ws.workDir, slug, len(idmap))
	return newHead, idmap, nil
}

// graftTruncate 用 `git replace --graft <oldest>`（无父）把最老保留点 oldest 与其更老的祖先解链：
// 仅一次 git 调用即截断整段旧前缀（被丢弃段随即不可达 → 交 git 自动 gc）。
//
// 替换引用 `refs/replace/<oldest>` **按 slug 独立登记**（多会话共享一个 workState），故修剪某会话
// 不会误删其它会话的替换引用；只动本插件本会话自己的引用，**绝不触碰用户的 `refs/replace/*`**。
//
// 清理**不依赖内存记状态**：跨进程重启后内存映射丢失，上一轮留下的替换引用会成为无界残留，
// 故每次修剪**枚举 `refs/replace/*`**、按被替换对象 commit message 的 `session=<slug>` 识别本插件
// 本会话条目，除即将重建的 oldest 外一并删除（见 cleanStaleGrafts）。
func (h *History) graftTruncate(ws *workState, slug, oldest string) error {
	h.cleanStaleGrafts(ws, slug, oldest)
	if _, err := h.git(ws, "replace", "-f", "--graft", oldest); err != nil {
		return err
	}
	return nil
}

// cleanStaleGrafts 删除本插件为 slug 会话登记的**陈旧** `refs/replace/*` 条目（keepID 非空时保留
// 该条目本身，因其即将被重建）。枚举 refs/replace/*，逐个读被替换 commit 的 message，`session=<slug>`
// 即属本插件本会话——重启后内存映射丢失也能清理，消除跨进程残留的无界累积。
// **绝不触碰非本插件（用户）条目**：识别失败 / 对象不可读一律保守跳过。
func (h *History) cleanStaleGrafts(ws *workState, slug, keepID string) {
	out, err := h.git(ws, "for-each-ref", "--format=%(refname)", "refs/replace/")
	if err != nil {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		ref := strings.TrimSpace(line)
		if !strings.HasPrefix(ref, "refs/replace/") {
			continue
		}
		sha := strings.TrimPrefix(ref, "refs/replace/")
		if sha == "" || sha == keepID {
			continue
		}
		if h.graftRefBelongsTo(ws, sha, slug) {
			_, _ = h.git(ws, "replace", "-d", sha)
		}
	}
}

// graftRefBelongsTo 判定 `refs/replace/<sha>` 是否本插件为 slug 会话所建：被替换对象 commit 的
// message 形如 `chonk-ckpt: session=<slug> tool=... ts=...`，解析出的 session 等于 slug 即命中。
// 对象不可读（如已被 gc）→ 视为不属于，保守跳过（绝不误删）。
func (h *History) graftRefBelongsTo(ws *workState, sha, slug string) bool {
	out, err := h.git(ws, "log", "-1", "--format=%s", sha)
	if err != nil {
		return false
	}
	msg := strings.TrimSpace(out)
	// 先要求本插件检查点前缀：否则用户自己的 `refs/replace/*`（commit message 恰好含 session=）
	// 会被 parseCkptMsg 误判归属 → 误删其替换引用。非本插件条目一律保守跳过。
	if !strings.HasPrefix(msg, "chonk-ckpt: ") {
		return false
	}
	_, s := parseCkptMsg(msg)
	return s == slug
}

// fetchChain 取链上前若干检查点（新→旧）：至少覆盖"最新 keep 个"；若保底项的 id 不在其中
// （少见：保底项比窗口更旧）则倍增窗口继续取，直至覆盖全部保底项或取尽整条链。
func (h *History) fetchChain(ws *workState, ref string, keep int, protected map[string]bool) ([]chainRec, error) {
	n := keep + 1
	for {
		recs, err := h.chainRecs(ws, ref, n)
		if err != nil {
			return nil, err
		}
		missing := false
		for id := range protected {
			found := false
			for _, r := range recs {
				if r.id == id {
					found = true
					break
				}
			}
			if !found {
				missing = true
				break
			}
		}
		if !missing || len(recs) < n {
			return recs, nil
		}
		n *= 2
	}
}

// chainRecs 取链上前 n 个检查点（新→旧）。
func (h *History) chainRecs(ws *workState, ref string, n int) ([]chainRec, error) {
	if n <= 0 {
		n = 1
	}
	out, err := h.git(ws, "log", "--format=%H%x1f%T%x1f%at%x1f%ct%x1f%s", "-n", strconv.Itoa(n), ref)
	if err != nil {
		return nil, err
	}
	var recs []chainRec
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x1f", 5)
		if len(f) < 5 {
			continue
		}
		at, _ := strconv.ParseInt(f[2], 10, 64)
		ct, _ := strconv.ParseInt(f[3], 10, 64)
		recs = append(recs, chainRec{
			id: f[0], tree: f[1], at: time.Unix(at, 0), ct: time.Unix(ct, 0), msg: f[4],
		})
	}
	return recs, nil
}

// ─── 解析（相对步 / turn-start / 绝对 commit）──────────────

// headRef 返回链头 commit（无链 → error）。
func (h *History) headRef(ws *workState, slug string) (string, error) {
	if slug == "" {
		return "", errors.New("尚无检查点（历史为空）")
	}
	out, err := h.git(ws, "rev-parse", "--verify", "--quiet", chainRefPrefix+slug)
	if err != nil {
		return "", errors.New("尚无检查点（历史为空）")
	}
	head := strings.TrimSpace(out)
	if head == "" {
		return "", errors.New("尚无检查点（历史为空）")
	}
	return head, nil
}

// resolveTo 把 to 参数归一为链上某检查点 commit：
// nil / 缺省 → 链头；负整数 → 相对步（-1 = 最近一步）；"turn-start" → 当前轮起点；
// 其它字符串 → 绝对 commit id（须在链上）。
func (h *History) resolveTo(ws *workState, slug string, to any) (string, error) {
	head, err := h.headRef(ws, slug)
	if err != nil {
		return "", err
	}
	switch v := to.(type) {
	case nil:
		return head, nil
	case float64:
		return h.relativeStep(ws, slug, head, int(v))
	case int:
		return h.relativeStep(ws, slug, head, v)
	case string:
		s := strings.TrimSpace(v)
		switch s {
		case "", "-1":
			return head, nil
		case "turn-start":
			id := ws.turnStartID // 须持 ws.mu（dispatch 持有）
			if id == "" {
				return "", errors.New("本轮尚无检查点（turn-start 不可用）")
			}
			return id, nil
		}
		if n, err := strconv.Atoi(s); err == nil {
			return h.relativeStep(ws, slug, head, n)
		}
		if _, err := h.git(ws, "rev-parse", "--verify", "--quiet", s+"^{commit}"); err != nil {
			return "", fmt.Errorf("未知检查点 %q", s)
		}
		out, err := h.git(ws, "merge-base", "--is-ancestor", s, head)
		if err != nil {
			return "", fmt.Errorf("检查点 %q 不在本链上（%s）", s, strings.TrimSpace(out))
		}
		return s, nil
	}
	return "", fmt.Errorf("to 参数非法（%v）", to)
}

// relativeStep 负整数相对步：-1 = 链头、-2 = 上一…
func (h *History) relativeStep(ws *workState, slug, head string, n int) (string, error) {
	if n >= 0 {
		return "", errors.New("to 必须为负整数（相对步）、\"turn-start\" 或绝对 commit id")
	}
	idx := -n - 1
	if idx == 0 {
		return head, nil
	}
	out, err := h.git(ws, "rev-list", "--max-count=1", "--skip="+strconv.Itoa(idx), chainRefPrefix+slug)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("相对步 %d 超出链范围", n)
	}
	return id, nil
}

// ─── diff / show / restore ─────────────────────────────────

// diffText 返回该检查点相对其父（根点 = 相对空树）的 diff 文本；path 非空则限定单文件。
// 超 diffMaxBytes 截断并标注。
func (h *History) diffText(ws *workState, ckpt, rel string) (string, error) {
	args := []string{"diff-tree", "--no-commit-id", "-p", "-r", "--root", ckpt}
	if rel != "" {
		args = append(args, "--", rel)
	}
	out, err := h.git(ws, args...)
	if err != nil {
		return "", err
	}
	if len(out) > diffMaxBytes {
		cut := out[:diffMaxBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1] // 回退到合法 UTF-8 边界，避免截断多字节字符
		}
		return cut + fmt.Sprintf("\n…（diff 超 %d 字节已截断；请用 path 参数缩小范围）", diffMaxBytes), nil
	}
	return out, nil
}

// showBlob 返回检查点中该文件的内容字节。
// maxBytes>0 时最多读取前 maxBytes 字节（history_show 展示面，防大文件撑爆插件内存/LLM 上下文），
// 返回 truncated=true；maxBytes<=0 读全量（history_restore 写回必须全量，截断即数据损坏）。
func (h *History) showBlob(ws *workState, ckpt, rel string, maxBytes int) ([]byte, bool, error) {
	kind, err := h.git(ws, "cat-file", "-t", ckpt+":"+rel)
	if err != nil {
		return nil, false, fmt.Errorf("检查点 %s 中不存在 %s", shortID(ckpt), rel)
	}
	if strings.TrimSpace(kind) != "blob" {
		return nil, false, fmt.Errorf("%s 不是文件（目录/子模块不支持）", rel)
	}
	return h.gitRaw(ws, maxBytes, "show", ckpt+":"+rel)
}

// restoreFile 把检查点中的单文件写回工作区（**只写该文件，不碰 .git**）。
// 强制一致性校验见 checkConsistency；path 必填、单文件、禁止批量。
func (h *History) restoreFile(ws *workState, slug, rel, ckpt string) error {
	if fi, err := os.Stat(filepath.Join(ws.workDir, filepath.FromSlash(rel))); err == nil && fi.IsDir() {
		return fmt.Errorf("%s 是目录，拒绝回滚（只支持单文件）", rel)
	}
	if err := h.checkConsistency(ws, slug, rel); err != nil {
		return err
	}
	// 写回必须全量内容（maxBytes=0 不限流，截断即数据损坏）。
	blob, _, err := h.showBlob(ws, ckpt, rel, 0)
	if err != nil {
		return err
	}
	diskPath := filepath.Join(ws.workDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode, err := h.git(ws, "ls-tree", ckpt, "--", rel); err == nil && strings.HasPrefix(strings.TrimSpace(mode), "100755") {
		perm = 0o755
	}
	if err := os.WriteFile(diskPath, blob, perm); err != nil {
		return fmt.Errorf("写回 %s 失败：%w", rel, err)
	}
	ws.dirty.Store(true) // 工作区已变更 → 置脏（后续打点捕获）
	return nil
}

// errOutOfSessionChanged 一致性校验未过（工作区该文件在链头之外被改动过）。
var errOutOfSessionChanged = errors.New("该文件在本会话之外被改动过，已拒绝回滚；请用 history_diff 查看后用文件工具自行修改。")

// checkConsistency 强制一致性校验：该文件「工作区当前内容」必须等于「链头点里该文件的内容」，
// **或**该文件在当前工作区已被删除（允许从检查点恢复回来 = 删除恢复的主要用途）。
//
// 判定**交给 git**（`git diff --quiet <链头> -- <path>`，退出码 0 = 工作区与链头一致），
// 不自行比较原始字节：行尾归一（`core.autocrlf` / `.gitattributes` / filter）由 git 处理，
// 避免 Git for Windows 默认 `core.autocrlf=true`（工作区 CRLF、blob LF）下恒判"已改动"。
// `git diff` 只读，且 index 已由 GIT_INDEX_FILE 指向插件临时 index，**不碰 `.git/index`**。
func (h *History) checkConsistency(ws *workState, slug, rel string) error {
	head, err := h.headRef(ws, slug)
	if err != nil {
		return err
	}
	diskPath := filepath.Join(ws.workDir, filepath.FromSlash(rel))
	if _, statErr := os.Stat(diskPath); statErr != nil {
		if os.IsNotExist(statErr) {
			return nil // 工作区已删除 → 允许（删除恢复）
		}
		return fmt.Errorf("读取工作区文件失败：%v", statErr)
	}
	// 工作区存在该文件：一致性**完全交给 git 判定**（退出码 0 = 与链头一致 → 允许）。
	if _, err := h.git(ws, "diff", "--quiet", head, "--", rel); err != nil {
		return errOutOfSessionChanged
	}
	return nil
}

// safeRel 校验并归一 workdir 相对路径（拒绝空 / 绝对 / 越界）。
func safeRel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path 必填（单文件、相对 workdir）")
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") || (len(p) >= 2 && p[1] == ':') {
		return "", errors.New("path 必须是 workdir 相对路径")
	}
	clean := path.Clean(p)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path 非法（越出 workdir）")
	}
	return clean, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ─── 状态 / 时间线（history.status / history.timeline 回写）──

// chainStatus 是 history.status 的 JSON 形状（内部键，见 64 §4.2）。
type chainStatus struct {
	Enabled          bool   `json:"enabled"`
	Mode             string `json:"mode"` // active | fused | off
	Repo             bool   `json:"repo"`
	Dirty            bool   `json:"dirty"`
	FailCount        int    `json:"failCount"`
	CheckpointCount  int    `json:"checkpointCount"`
	Bytes            int64  `json:"bytes"`
	LastCheckpointAt string `json:"lastCheckpointAt"`
	LastDurationMs   int64  `json:"lastDurationMs"`
	LastError        string `json:"lastError"`
}

// timelineEntry 是 history.timeline 的单条（最新在前，相对编号 -1 起）。
type timelineEntry struct {
	N       int    `json:"n"`
	ID      string `json:"id"`
	TS      string `json:"ts"`
	Tool    string `json:"tool"`
	Session string `json:"session"`
	Files   int    `json:"files"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

// baseStatus 读 workState 的非 git 派生状态（须持 ws.mu）。
//
// `slug` = 目标会话链（根会话）：`lastAt`/`lastDurMs` 是 **workdir 级**的最近一次打点，
// 仅当它属于该 slug（`ws.lastSlug == slug`）时才透出 —— 否则该会话自身尚无检查点，
// 显示「—」而非别会话的最近打点（I-135 按会话呈现）。
func (ws *workState) baseStatus(slug string) chainStatus {
	mode := "active"
	switch {
	case !ws.enabled.Load():
		mode = "off"
	case ws.fused:
		mode = "fused"
	}
	st := chainStatus{
		Enabled:   ws.enabled.Load(),
		Mode:      mode,
		Repo:      ws.hasGit,
		Dirty:     ws.dirty.Load(),
		FailCount: ws.failCount,
		LastError: ws.lastErr,
	}
	if !ws.lastAt.IsZero() && ws.lastSlug == slug {
		st.LastCheckpointAt = ws.lastAt.Format(time.RFC3339)
		st.LastDurationMs = ws.lastDurMs
	}
	return st
}

// buildStatus 组装该 workdir 指定链的状态 + 时间线（slug 空 → 空链）。
func (h *History) buildStatus(ws *workState, slug string) (chainStatus, []timelineEntry) {
	st := ws.baseStatus(slug)
	entries := []timelineEntry{}
	if slug == "" || !ws.hasGit {
		return st, entries
	}
	ref := chainRefPrefix + slug
	if _, err := h.git(ws, "rev-parse", "--verify", "--quiet", ref); err != nil {
		return st, entries
	}
	if out, err := h.git(ws, "rev-list", "--count", ref); err == nil {
		st.CheckpointCount, _ = strconv.Atoi(strings.TrimSpace(out))
	}
	entries = h.timeline(ws, ref)
	if len(entries) > 0 {
		st.LastCheckpointAt = entries[0].TS
	}
	if head, err := h.headRef(ws, slug); err == nil {
		st.Bytes = h.treeBytes(ws, head)
	}
	return st, entries
}

// timeline 由 `git log --numstat` 一次性推导时间线（最新在前，≤ timelineMax）。
func (h *History) timeline(ws *workState, ref string) []timelineEntry {
	out, err := h.git(ws, "log", "--format=%x01%H %cI %s", "--numstat", "-n", strconv.Itoa(timelineMax), ref)
	if err != nil {
		return []timelineEntry{}
	}
	entries := []timelineEntry{}
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); {
		line := strings.TrimRight(lines[i], "\r")
		if !strings.HasPrefix(line, "\x01") {
			i++
			continue
		}
		f := strings.SplitN(strings.TrimPrefix(line, "\x01"), " ", 3)
		if len(f) < 3 {
			i++
			continue
		}
		tool, session := parseCkptMsg(f[2])
		e := timelineEntry{
			N:       -(len(entries) + 1),
			ID:      f[0],
			TS:      f[1],
			Tool:    tool,
			Session: session,
		}
		i++
		for i < len(lines) && !strings.HasPrefix(lines[i], "\x01") {
			stat := strings.TrimRight(lines[i], "\r")
			i++
			if stat == "" {
				continue
			}
			cols := strings.SplitN(stat, "\t", 3)
			if len(cols) != 3 {
				continue
			}
			a, e1 := strconv.Atoi(cols[0])
			r, e2 := strconv.Atoi(cols[1])
			if e1 != nil || e2 != nil {
				continue // 二进制（-\t-）
			}
			e.Added += a
			e.Removed += r
			e.Files++
		}
		entries = append(entries, e)
	}
	return entries
}

// parseCkptMsg 从检查点 message 解析 tool / session（message 形如
// `chonk-ckpt: session=<slug> tool=<name> ts=<RFC3339>`）。
func parseCkptMsg(msg string) (tool, session string) {
	rest := strings.TrimPrefix(msg, "chonk-ckpt: ")
	for _, kv := range strings.Fields(rest) {
		switch {
		case strings.HasPrefix(kv, "session="):
			session = strings.TrimPrefix(kv, "session=")
		case strings.HasPrefix(kv, "tool="):
			tool = strings.TrimPrefix(kv, "tool=")
		}
	}
	return tool, session
}

// treeBytes 链头 tree 收录文件的总字节数（`git ls-tree -r -l`；history.status.bytes）。
func (h *History) treeBytes(ws *workState, commit string) int64 {
	out, err := h.git(ws, "ls-tree", "-r", "-l", commit)
	if err != nil {
		return 0
	}
	var sum int64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		head := line
		if i := strings.IndexByte(line, '\t'); i >= 0 {
			head = line[:i]
		}
		f := strings.Fields(head)
		if len(f) < 4 {
			continue
		}
		if n, err := strconv.ParseInt(f[3], 10, 64); err == nil {
			sum += n
		}
	}
	return sum
}

// writeback 回写**该会话**的 history.status.<slug> / history.timeline.<slug>
// （须在**不持有** ws.mu 时调用；slug 空 → 不动作；内部仅在组装状态时短暂持 ws.mu）。
//
// 键名带会话后缀（slug = 根会话）→ 多会话并发时各自独立、互不覆盖（I-135）；
// 落 **prjusr**（persist `localRuntimeKeys` 前缀匹配）——属本机可重建派生物。
func (h *History) writeback(ws *workState, slug string) {
	if slug == "" {
		return
	}
	inst := h.instanceForWorkdir(ws.workDir)
	if inst == "" {
		return
	}
	ws.mu.Lock()
	st, entries := h.buildStatus(ws, slug)
	ws.mu.Unlock()
	if b, err := json.Marshal(st); err == nil {
		if err := dataclient.SaveKey(h.deps.Bus, inst, statusKeyPrefix+slug, string(b)); err != nil {
			h.logf()("history: 回写 %s 失败（instance=%s）：%v", statusKeyPrefix+slug, inst, err)
		}
	}
	if b, err := json.Marshal(entries); err == nil {
		if err := dataclient.SaveKey(h.deps.Bus, inst, timelineKeyPrefix+slug, string(b)); err != nil {
			h.logf()("history: 回写 %s 失败（instance=%s）：%v", timelineKeyPrefix+slug, inst, err)
		}
	}
}
