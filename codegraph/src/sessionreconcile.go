// sessionreconcile.go —— 会话内容索引的**后台对账同步**（D-74，2026-11-04）。
//
// 口径（用户 2026-11-04 拍板）：**不新增任何 MQ 事件**，插件内起一个**低优先级单协程**，
// 定期核对下列四项并当场修差异：
//  1. 已索引时间：引擎 `state.json` 每会话 `indexedAt`（UTC，仅日志 / 新鲜度，不判正确性）；
//  2. 内容指纹：消息 id + role + 正文的 sha256（与引擎存值不一致 → 重建该会话索引）；
//  3. 漏索引的会话 / 轮：数据面有、引擎里没有（或无指纹）→ 全量补索引该会话；
//  4. 已删除的记录：引擎里有、`data-session-list` 已无 → 清掉该会话索引（含其消息 doc）。
//
// 触发：插件启动后 sessionReconcileInitialDelay 首轮，之后每 sessionReconcileInterval 一轮。
// 让行：同一 workdir 单飞；该 workdir 有增量待处理 / 全量重建在跑 / 引擎未就绪 → 本轮跳过。
// 失败：只记日志 + 进度 error（沿用 sessionProgress），不阻断增量、不 disable；
// 重启后由首轮对账自然覆盖（`state.json` 的游标 / 指纹是唯一权威）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"

	vfts "github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/vfts/server"
)

const (
	// sessionReconcileInitialDelay 插件启动后首轮对账的延迟（避开启动期的全量索引）。
	sessionReconcileInitialDelay = 3 * time.Minute
	// sessionReconcileInterval 之后的对账周期。
	sessionReconcileInterval = 12 * time.Minute
	// sessionReconcileTimeout 单轮对账的超时（大项目全量核对）。
	sessionReconcileTimeout = 20 * time.Minute
	// sessionReconcileYield 每处理完一个会话的让路时长（低优先级，不抢增量索引）。
	sessionReconcileYield = 120 * time.Millisecond
	// noticeSessionIndexReconciled 对账通知（既有 tool-notify 主题里的 notice 取值，非新主题）。
	noticeSessionIndexReconciled = "session-index-reconciled"
)

// sessionReconcileStats 一轮对账的计数（只用于日志 / 通知，不落盘）。
type sessionReconcileStats struct {
	Sessions  int // 数据面主会话数
	Missing   int // 漏索引（引擎无游标 / 无指纹）→ 补索引
	Rebuilt   int // 指纹不一致 → 重建
	Removed   int // 已删除会话 → 清理
	Unchanged int // 指纹一致（不重复重建）
	Failed    int // 单会话读取失败（下一轮重试）
}

// changed 本轮是否有实际差异修复。
func (s sessionReconcileStats) changed() bool {
	return s.Missing+s.Rebuilt+s.Removed > 0
}

// startReconcileLoop 启动后台对账协程（单协程；ctx 结束即退出）。
func (x *sessionIndexer) startReconcileLoop(ctx context.Context) {
	if x == nil {
		return
	}
	go func() {
		timer := time.NewTimer(sessionReconcileInitialDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			x.reconcileAll(ctx)
			timer.Reset(sessionReconcileInterval)
		}
	}()
}

// reconcileAll 对全部已登记 workdir 逐个对账（同一 workdir 单飞）。
func (x *sessionIndexer) reconcileAll(ctx context.Context) {
	for _, wd := range x.m.allWorkdirs() {
		if ctx.Err() != nil {
			return
		}
		inst := x.m.instForWorkdir(wd)
		if inst == "" {
			continue
		}
		x.reconcileWorkdir(ctx, wd, inst)
	}
}

// reconcileWorkdir 对单个 workdir 做一轮对账（未被启用 / 让行 → 零动作）。
func (x *sessionIndexer) reconcileWorkdir(ctx context.Context, workdir, instanceID string) sessionReconcileStats {
	var st sessionReconcileStats
	wd := normalizeWorkdir(workdir)
	if wd == "" || instanceID == "" {
		return st
	}
	if !x.m.sessionsEnabled(ctx, instanceID) {
		return st
	}
	if !x.beginReconcile(wd) {
		return st // 单飞 / 全量重建在跑 / 增量待处理 → 本轮让行
	}
	defer x.endReconcile(wd)

	roundCtx, cancel := context.WithTimeout(ctx, sessionReconcileTimeout)
	defer cancel()
	if err := x.reconcileRound(roundCtx, wd, instanceID, &st); err != nil {
		x.m.logfFor(wd)("codegraph: 会话索引后台对账失败：%v（下轮重试，不影响增量索引）", err)
		x.setReconcileErr(wd, err)
		return st
	}
	x.clearReconcileErr(wd)
	x.m.logfFor(wd)("codegraph: 会话索引对账完成（会话 %d：补索引 %d / 重建 %d / 清理 %d / 一致 %d / 失败 %d）",
		st.Sessions, st.Missing, st.Rebuilt, st.Removed, st.Unchanged, st.Failed)
	if st.changed() {
		x.notify(instanceID, wd, noticeSessionIndexReconciled,
			fmt.Sprintf("会话内容索引已对账：补索引 %d，重建 %d，清理 %d", st.Missing, st.Rebuilt, st.Removed))
	}
	return st
}

// beginReconcile 申请该 workdir 的对账权（拿不到 → false = 本轮让行）。
//
// 让行条件：已有对账在跑（单飞）；插件侧全量重建在跑；该 workdir 有增量待处理（增量优先）。
func (x *sessionIndexer) beginReconcile(wd string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.reconciling == nil {
		x.reconciling = map[string]bool{}
	}
	if x.reconciling[wd] {
		return false
	}
	if x.cancels[wd] != nil {
		return false
	}
	if x.hasPendingLocked(wd, "") {
		return false
	}
	x.reconciling[wd] = true
	return true
}

// endReconcile 释放对账权。
func (x *sessionIndexer) endReconcile(wd string) {
	x.mu.Lock()
	delete(x.reconciling, wd)
	x.mu.Unlock()
}

// hasPendingFor 判定该 workdir（sessionID 空 = 任意会话）是否有增量待处理。
func (x *sessionIndexer) hasPendingFor(wd, sessionID string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.hasPendingLocked(wd, sessionID)
}

// hasPendingLocked 同 hasPendingFor（调用方须持 mu）。
func (x *sessionIndexer) hasPendingLocked(wd, sessionID string) bool {
	if sessionID != "" {
		return x.pending[wd+"|"+sessionID] != nil
	}
	prefix := wd + "|"
	for key := range x.pending {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// reconcileRound 一轮核对：漏索引 → 补；内容变更 → 重建；已删除 → 清理。
func (x *sessionIndexer) reconcileRound(ctx context.Context, wd, instanceID string, st *sessionReconcileStats) error {
	// 引擎未就绪（未初始化 / 建库中 / 出错）→ 本轮不动：建库由首轮全量重建负责。
	if state := x.sessionEngineState(ctx, wd); state != "ready" {
		return nil
	}
	cursors, err := x.cursorsFn(wd)
	if err != nil {
		return fmt.Errorf("读取已索引会话游标失败：%w", err)
	}
	sessions, err := x.mainSessions(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("读取项目主会话失败：%w", err)
	}
	st.Sessions = len(sessions)

	live := make(map[string]bool, len(sessions))
	fresh := make(map[string]string, len(sessions)) // 指纹一致者 → 仅刷新 indexedAt
	for _, s := range sessions {
		live[s.SessionID] = true
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if x.hasPendingFor(wd, s.SessionID) {
			continue // 该会话有增量待处理 → 本会话让行，下一轮再来
		}
		pages, perr := x.sessionPages(ctx, instanceID, s)
		if perr != nil {
			x.m.logfFor(wd)("codegraph: 对账读取会话 %s 历史失败：%v", s.SessionID, perr)
			st.Failed++
			x.yieldReconcile()
			continue
		}
		docs := sessionDocsOfPages(s, pages)
		fp := sessionFingerprint(docs)
		switch cur := cursors[s.SessionID]; {
		case cur.Fingerprint == fp:
			st.Unchanged++
			fresh[s.SessionID] = fp
		case cur.Fingerprint == "":
			// 引擎无该会话 / 未记录指纹 → 视为漏索引：全量补索引后记指纹。
			if err := x.rebuildSessionIndex(ctx, wd, s, docs); err != nil {
				x.m.logfFor(wd)("codegraph: 对账补索引会话 %s 失败：%v", s.SessionID, err)
				st.Failed++
				x.yieldReconcile()
				continue
			}
			st.Missing++
			x.recordFingerprints(wd, map[string]string{s.SessionID: fp})
		default:
			// 内容变了（正文编辑 / 消息增删）→ 清旧 doc 后重建（先删后插，幂等）。
			if err := x.rebuildSessionIndex(ctx, wd, s, docs); err != nil {
				x.m.logfFor(wd)("codegraph: 对账重建会话 %s 失败：%v", s.SessionID, err)
				st.Failed++
				x.yieldReconcile()
				continue
			}
			st.Rebuilt++
			x.recordFingerprints(wd, map[string]string{s.SessionID: fp})
		}
		x.yieldReconcile()
	}

	// 已删除的记录：引擎里有、数据面已无 → 清掉该会话索引。
	for sid := range cursors {
		if live[sid] {
			continue
		}
		if err := x.clearSession(ctx, wd, sid); err != nil {
			x.m.logfFor(wd)("codegraph: 对账清理已删除会话 %s 失败：%v", sid, err)
			st.Failed++
			continue
		}
		st.Removed++
		x.yieldReconcile()
	}

	// 指纹一致者只刷新 indexedAt（记录「多久未对账」，不参与正确性判定）。
	x.recordFingerprints(wd, fresh)
	return nil
}

// rebuildSessionIndex 先清该会话旧 doc（消息被删 / 正文变了都要清干净），再全量写入。
func (x *sessionIndexer) rebuildSessionIndex(ctx context.Context, wd string, s sessionRef, docs []vfts.SessionDoc) error {
	if err := x.clearSession(ctx, wd, s.SessionID); err != nil {
		return err
	}
	if len(docs) == 0 {
		return nil
	}
	if _, err := x.m.engine(ctx, "vfts_session_index", map[string]any{
		"workdir": wd,
		"docs":    docs,
	}); err != nil {
		return fmt.Errorf("写入会话索引：%w", err)
	}
	return nil
}

// clearSession 清掉单个会话的索引（引擎既有 vfts_session_clear{workdir, session_id}）。
func (x *sessionIndexer) clearSession(ctx context.Context, wd, sessionID string) error {
	if _, err := x.m.engine(ctx, "vfts_session_clear", map[string]any{
		"workdir":    wd,
		"session_id": sessionID,
	}); err != nil {
		return fmt.Errorf("清理会话索引：%w", err)
	}
	return nil
}

// recordFingerprints 回写会话指纹（失败只记日志：下一轮会再重建 / 再记录）。
func (x *sessionIndexer) recordFingerprints(wd string, fps map[string]string) {
	if len(fps) == 0 {
		return
	}
	if err := x.recordFn(wd, fps); err != nil {
		x.m.logfFor(wd)("codegraph: 记录会话索引指纹失败：%v", err)
	}
}

// sessionEngineState 读引擎侧会话索引状态（"" 未初始化 / ready / indexing / error）。
func (x *sessionIndexer) sessionEngineState(ctx context.Context, wd string) string {
	out, err := x.m.engine(ctx, "vfts_session_status", map[string]any{"workdir": wd})
	if err != nil {
		return ""
	}
	return statusString(out, "state")
}

// yieldReconcile 低优先级让路（每个会话一次，避免与增量索引抢资源）。
func (x *sessionIndexer) yieldReconcile() {
	runtime.Gosched()
	time.Sleep(sessionReconcileYield)
}

// setReconcileErr 对账失败落到进度（正在全量重建时不覆盖其状态）。
func (x *sessionIndexer) setReconcileErr(wd string, err error) {
	x.setProgress(wd, func(p *sessionProgress) {
		if p.State == "indexing" {
			return
		}
		p.State = "error"
		p.Err = "对账失败：" + err.Error()
	})
}

// clearReconcileErr 对账成功后清掉上一次的对账错误（不碰全量重建的错误态）。
func (x *sessionIndexer) clearReconcileErr(wd string) {
	x.setProgress(wd, func(p *sessionProgress) {
		if p.State == "error" && strings.HasPrefix(p.Err, "对账失败：") {
			p.State = ""
			p.Err = ""
		}
	})
}

// sessionDocsOfPages 逐页（旧 → 新）取该会话可入索引的消息 doc。
func sessionDocsOfPages(s sessionRef, pages []historyPage) []vfts.SessionDoc {
	out := make([]vfts.SessionDoc, 0, len(pages)*8)
	for i := len(pages) - 1; i >= 0; i-- {
		out = append(out, sessionDocsOf(s.SessionID, s.DialogID, pages[i].Messages)...)
	}
	return out
}

// sessionFingerprint 计算会话已索引内容的稳定指纹（sha256 hex，Go 标准库）：
// 每行 `message_id \x00 role \x00 正文`，按行排序后拼接 —— 与消息顺序 / 分页无关。
func sessionFingerprint(docs []vfts.SessionDoc) string {
	lines := make([]string, 0, len(docs))
	for _, d := range docs {
		lines = append(lines, d.MessageID+"\x00"+d.Role+"\x00"+d.Text)
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, line := range lines {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
