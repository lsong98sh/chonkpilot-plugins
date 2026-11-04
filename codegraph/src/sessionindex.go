// sessionindex.go —— 会话内容全文索引（SESSION-FTS，2B-vfts §3.5）的插件侧编排。
//
// 职责（引擎只做存取，编排全在插件）：
//   - 增量：订阅既有 `session-complete`（零新增字段）→ 只处理**主会话**（parents 为空，
//     且必要时 data-session-get 校验 parent_id 为空）→ 按会话去抖 → 按轮拉该轮消息正文
//     → 转 doc 调引擎 `vfts_session_index`（按 message_id 先删后插，幂等）。
//   - 全量：`vfts.session.reindex` → 后台扫当前项目全部主会话（data-session-list）→ 逐会话
//     `data-session-history` 分页（before_turn_id）→ 写会话集合；`state.json` 记每会话
//     `indexed_until_turn`（游标由引擎写入）→ 中断后再次重建可断点续建。
//   - 查询：`vfts.session.query`（UI 会话内查找）→ 直调引擎 `vfts_query{scope:session}`。
//   - 收口：配置键 `plugins.codegraph.vfts.sessions` 关闭 → 清空该 workdir 的会话集合。
//   - 对账：后台单协程低优先级定期核对「漏索引 / 删除 / 内容指纹」，见 sessionreconcile.go（D-74）。
//   - 进度：内存态（不回写任何运行时 prj 键），经插件动作 engine-status / badge 的 session 段透出；
//     完成/失败发既有通知面 `tool-notify`（notice=session-index-done / session-index-failed）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	vfts "github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/vfts/server"
)

// 消息面（61-消息一览）：会话索引相关。
const (
	subjectSessionHistory = "plugin/v1/session.history" // → data-session-history
	subjectSessionList    = "plugin/v1/session.list"    // → data-session-list
	subjectSessionGet     = "plugin/v1/session.get"     // → data-session-get（校验 parent_id）
	subjectToolNotify     = "plugin/v1/tool.notify"     // → tool-notify（既有通知面）

	noticeSessionIndexDone   = "session-index-done"
	noticeSessionIndexFailed = "session-index-failed"
)

const (
	// sessionDebounce 按会话合并连续轮末事件（一次对话里连续几轮只写一次索引窗口）。
	sessionDebounce = 1200 * time.Millisecond
	// 会话历史分页参数（足够大以免丢轮；按 has_more 继续向前翻页）。
	sessionPageMessages = 400
	sessionPageBytes    = 1 << 20
	// sessionRebuildTimeout 覆盖大项目全量会话索引。
	sessionRebuildTimeout = 20 * time.Minute
)

// sessionCompleteEvent 是 `session-complete` 载荷中本模块需要的字段（其余忽略）。
type sessionCompleteEvent struct {
	InstanceID string   `json:"instance_id"`
	WorkDir    string   `json:"work_dir"`
	Session    string   `json:"session"`
	Turn       string   `json:"turn"`
	DialogID   string   `json:"dialog_id"`
	Parents    []string `json:"parents"`
}

// sessionProgress 单 workdir 的会话索引进度（内存态；engine-status / badge 现取）。
type sessionProgress struct {
	State         string `json:"state"` // idle | indexing | ready | error
	DoneSessions  int    `json:"doneSessions"`
	TotalSessions int    `json:"totalSessions"`
	DoneTurns     int    `json:"doneTurns"`
	TotalTurns    int    `json:"totalTurns"`
	Err           string `json:"err,omitempty"`
}

// sessionTurnTask 去抖窗口内合并的待索引轮次。
type sessionTurnTask struct {
	InstanceID string
	WorkDir    string
	SessionID  string
	DialogID   string
	Turns      []string
}

// sessionIndexer 会话索引编排器（挂在 configManager 上，共享其配置读取与引擎调用）。
type sessionIndexer struct {
	m *configManager

	mu       sync.Mutex
	pending  map[string]*sessionTurnTask   // wd|session → 待处理轮次
	timers   map[string]*time.Timer        // wd|session → 去抖计时器
	progress map[string]*sessionProgress   // wd → 进度
	cancels  map[string]context.CancelFunc // wd → 在跑的全量重建

	// reconciling 记录正在后台对账的 workdir（同一 workdir 同时只跑一个对账，D-74）。
	reconciling map[string]bool

	// 可替换依赖（生产 = 插件通道 / 引擎内部方法；测试注入内存桩）。
	dataCall  func(ctx context.Context, method string, payload map[string]any) (json.RawMessage, error)
	cursorsFn func(workdir string) (map[string]vfts.SessionCursor, error)
	recordFn  func(workdir string, fingerprints map[string]string) error
}

func newSessionIndexer(m *configManager) *sessionIndexer {
	return &sessionIndexer{
		m:           m,
		pending:     map[string]*sessionTurnTask{},
		timers:      map[string]*time.Timer{},
		progress:    map[string]*sessionProgress{},
		cancels:     map[string]context.CancelFunc{},
		reconciling: map[string]bool{},
		cursorsFn:   vfts.SessionCursors,
		recordFn:    vfts.RecordSessionFingerprints,
	}
}

// ---------------------------------------------------------------------------
// 配置读取
// ---------------------------------------------------------------------------

// sessionsEnabled 读该实例的 `plugins.codegraph.vfts.sessions`（缺省关闭）。
func (m *configManager) sessionsEnabled(ctx context.Context, instanceID string) bool {
	if instanceID == "" {
		return false
	}
	v, ok, err := m.store.Get(ctx, instanceID, KeyVftsSessions)
	if err != nil {
		return false
	}
	return ok && v == "true"
}

// ---------------------------------------------------------------------------
// 增量：session-complete
// ---------------------------------------------------------------------------

// onSessionComplete 处理轮末事件：只处理主会话（parents 为空），按会话去抖后增量写会话索引。
func (x *sessionIndexer) onSessionComplete(payload []byte) {
	var ev sessionCompleteEvent
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	if ev.InstanceID == "" || ev.Session == "" || ev.Turn == "" || ev.WorkDir == "" {
		return
	}
	if len(ev.Parents) > 0 {
		return // 子轮次（父链非空）不入会话正文索引
	}
	key := normalizeWorkdir(ev.WorkDir) + "|" + ev.Session
	x.mu.Lock()
	task := x.pending[key]
	if task == nil {
		task = &sessionTurnTask{
			InstanceID: ev.InstanceID, WorkDir: normalizeWorkdir(ev.WorkDir),
			SessionID: ev.Session, DialogID: ev.DialogID,
		}
		x.pending[key] = task
	}
	if task.DialogID == "" {
		task.DialogID = ev.DialogID
	}
	if !containsString(task.Turns, ev.Turn) {
		task.Turns = append(task.Turns, ev.Turn)
	}
	if old := x.timers[key]; old != nil {
		old.Stop()
	}
	x.timers[key] = time.AfterFunc(sessionDebounce, func() { x.flushTurnTask(key) })
	x.mu.Unlock()
}

// flushTurnTask 取走去抖窗口内合并的任务并增量写索引。
func (x *sessionIndexer) flushTurnTask(key string) {
	x.mu.Lock()
	delete(x.timers, key)
	task := x.pending[key]
	delete(x.pending, key)
	x.mu.Unlock()
	if task == nil {
		return
	}
	x.indexTurns(*task)
}

// indexTurns 按轮拉正文 → 转 doc → 调引擎写会话索引。
func (x *sessionIndexer) indexTurns(task sessionTurnTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !x.m.sessionsEnabled(ctx, task.InstanceID) {
		return
	}
	// 实例登记的工作区必须与事件一致（避免把别的项目写进本目录）
	if wd := x.m.workdirOf(task.InstanceID); wd != "" && !samePath(wd, task.WorkDir) {
		return
	}
	// 兜底校验：会话行 parent_id 非空 = 子会话 → 不入会话正文索引（主会话才索引）。
	if parent, err := x.sessionParent(ctx, task.InstanceID, task.SessionID, task.DialogID); err == nil && parent != "" {
		return
	}
	docs := make([]vfts.SessionDoc, 0, 8)
	for _, turn := range task.Turns {
		msgs, err := x.fetchTurnMessages(ctx, task.InstanceID, task.SessionID, task.DialogID, turn)
		if err != nil {
			x.m.logfFor(task.WorkDir)("codegraph: 会话索引读取轮次 %s 消息失败：%v", turn, err)
			continue
		}
		docs = append(docs, sessionDocsOf(task.SessionID, task.DialogID, msgs)...)
	}
	if len(docs) == 0 {
		return
	}
	out, err := x.m.engine(ctx, "vfts_session_index", map[string]any{
		"workdir": task.WorkDir,
		"docs":    docs,
	})
	if err != nil {
		x.m.logfFor(task.WorkDir)("codegraph: 会话索引写入失败：%v", err)
		return
	}
	x.m.logfFor(task.WorkDir)("codegraph: 会话索引已增量写入 %d 条消息（%v）", len(docs), out)
}

// call 经插件桥调用宿主能力面（未装配插件通道 → 明确错误，不 panic）。
func (x *sessionIndexer) call(ctx context.Context, method string, payload map[string]any) (json.RawMessage, error) {
	if x.dataCall != nil {
		return x.dataCall(ctx, method, payload)
	}
	if x.m == nil || x.m.p == nil {
		return nil, fmt.Errorf("插件通道未装配")
	}
	return x.m.p.Call(ctx, method, payload)
}

// sessionParent 经 `plugin/v1/session.get` 读会话父 id（空 = 主会话；读失败 → error）。
func (x *sessionIndexer) sessionParent(ctx context.Context, instanceID, sessionID, dialogID string) (string, error) {
	payload := map[string]any{"instance_id": instanceID, "session_id": sessionID}
	if dialogID != "" {
		payload["dialog_id"] = dialogID
	}
	raw, err := x.call(ctx, subjectSessionGet, payload)
	if err != nil {
		return "", err
	}
	var root struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", err
	}
	return pluginsdk.Str(root.Data["parent_id"]), nil
}

// fetchTurnMessages 读某轮的消息视图行（含 message_id / created_at / type / content）。
//
// 说明（偏差，见交付报告）：契约写「按轮 data-session-load-messages」，但该主题返回的
// ChatMsg 不含 message_id / created_at / type，无法满足「主键 s:<sid>:<mid>」与 ts 升序。
// 故改用同域既有 `data-session-history`（取最近一页后按 turn_id 过滤）—— 消息 id / 创建时间 /
// 视图类型由它提供，且不需要额外新增消息面。
func (x *sessionIndexer) fetchTurnMessages(ctx context.Context, instanceID, sessionID, dialogID, turnID string) ([]map[string]any, error) {
	if dialogID == "" {
		return nil, fmt.Errorf("缺 dialog_id（会话消息在对话库）")
	}
	res, err := x.call(ctx, subjectSessionHistory, map[string]any{
		"instance_id":     instanceID,
		"session_id":      sessionID,
		"dialog_id":       dialogID,
		"target_messages": sessionPageMessages,
		"target_bytes":    sessionPageBytes,
	})
	if err != nil {
		return nil, err
	}
	page, err := parseHistoryPage(res)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(page.Messages))
	for _, m := range page.Messages {
		if pluginsdk.Str(m["turn_id"]) == turnID {
			out = append(out, m)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 全量重建
// ---------------------------------------------------------------------------

// fullRebuild 后台重建某 workdir 的全部主会话索引（可被新的重建覆盖/取消）。
func (x *sessionIndexer) fullRebuild(workdir, instanceID string) {
	wd := normalizeWorkdir(workdir)
	if wd == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionRebuildTimeout)
	defer cancel()
	x.mu.Lock()
	if old := x.cancels[wd]; old != nil {
		old() // 新的重建覆盖旧的（后台执行、可取消）
	}
	x.cancels[wd] = cancel
	x.progress[wd] = &sessionProgress{State: "indexing"}
	x.mu.Unlock()

	err := x.runFullRebuild(ctx, wd, instanceID)

	x.mu.Lock()
	delete(x.cancels, wd)
	p := x.progress[wd]
	if p == nil {
		p = &sessionProgress{}
		x.progress[wd] = p
	}
	if err != nil {
		p.State = "error"
		p.Err = err.Error()
	} else {
		p.State = "ready"
		p.Err = ""
	}
	x.mu.Unlock()

	if err != nil {
		x.m.logfFor(wd)("codegraph: 会话索引全量重建失败：%v", err)
		x.notify(instanceID, wd, noticeSessionIndexFailed, "会话内容索引重建失败："+err.Error())
		return
	}
	x.m.logfFor(wd)("codegraph: 会话索引全量重建完成（%d/%d 会话，%d/%d 轮）",
		p.DoneSessions, p.TotalSessions, p.DoneTurns, p.TotalTurns)
	x.notify(instanceID, wd, noticeSessionIndexDone, "会话内容索引已重建完成")
}

// runFullRebuild 逐会话（data-session-list → data-session-history 分页）重建。
func (x *sessionIndexer) runFullRebuild(ctx context.Context, workdir, instanceID string) error {
	sessions, err := x.mainSessions(ctx, instanceID)
	if err != nil {
		return err
	}
	cursors, _ := vfts.SessionCursors(workdir)
	x.setProgress(workdir, func(p *sessionProgress) {
		p.State = "indexing"
		p.TotalSessions = len(sessions)
		p.DoneSessions = 0
		p.DoneTurns = 0
		p.TotalTurns = 0
	})
	for i, s := range sessions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pages, err := x.sessionPages(ctx, instanceID, s)
		if err != nil {
			x.m.logfFor(workdir)("codegraph: 会话 %s 读取历史失败：%v", s.SessionID, err)
		} else {
			x.indexSessionPages(ctx, workdir, s, pages, cursors[s.SessionID].IndexedUntilTurn)
		}
		x.setProgress(workdir, func(p *sessionProgress) { p.DoneSessions = i + 1 })
	}
	return nil
}

// sessionRef 会话行（主会话）的最小字段。
type sessionRef struct {
	SessionID string
	DialogID  string
}

// mainSessions 取当前项目全部**主会话**（parent_id 为空）。
func (x *sessionIndexer) mainSessions(ctx context.Context, instanceID string) ([]sessionRef, error) {
	res, err := x.call(ctx, subjectSessionList, map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, err
	}
	var root struct {
		List []map[string]any `json:"list"`
	}
	if err := json.Unmarshal(res, &root); err != nil {
		return nil, fmt.Errorf("data-session-list 应答解析失败: %w", err)
	}
	out := make([]sessionRef, 0, len(root.List))
	for _, row := range root.List {
		if pluginsdk.Str(row["parent_id"]) != "" {
			continue // 子会话不入会话正文索引
		}
		sid := pluginsdk.Str(row["session_id"])
		if sid == "" {
			continue
		}
		out = append(out, sessionRef{SessionID: sid, DialogID: pluginsdk.Str(row["dialog_id"])})
	}
	return out, nil
}

// historyPage 一页历史（消息视图行按轮分组前的原始数组 + 该页轮次 id 升序）。
type historyPage struct {
	Messages []map[string]any
	Turns    []string
	HasMore  bool
}

// parseHistoryPage 解析 `plugin/v1/session.history` 应答（{messages:{turns,messages,has_more}}）。
func parseHistoryPage(raw []byte) (historyPage, error) {
	var root struct {
		Messages struct {
			Turns    []map[string]any `json:"turns"`
			Messages []map[string]any `json:"messages"`
			HasMore  bool             `json:"has_more"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return historyPage{}, fmt.Errorf("data-session-history 应答解析失败: %w", err)
	}
	page := historyPage{Messages: root.Messages.Messages, HasMore: root.Messages.HasMore}
	for _, t := range root.Messages.Turns {
		if id := pluginsdk.Str(t["turn_id"]); id != "" {
			page.Turns = append(page.Turns, id)
		}
	}
	return page, nil
}

// sessionPages 从最新往旧翻页取该会话全部历史页（返回顺序 = 新 → 旧）。
func (x *sessionIndexer) sessionPages(ctx context.Context, instanceID string, s sessionRef) ([]historyPage, error) {
	if s.DialogID == "" {
		return nil, fmt.Errorf("缺 dialog_id（会话消息在对话库）")
	}
	var pages []historyPage
	before := ""
	for {
		if ctx.Err() != nil {
			return pages, ctx.Err()
		}
		res, err := x.call(ctx, subjectSessionHistory, map[string]any{
			"instance_id":     instanceID,
			"session_id":      s.SessionID,
			"dialog_id":       s.DialogID,
			"before_turn_id":  before,
			"target_messages": sessionPageMessages,
			"target_bytes":    sessionPageBytes,
		})
		if err != nil {
			return pages, err
		}
		page, err := parseHistoryPage(res)
		if err != nil {
			return pages, err
		}
		pages = append(pages, page)
		if !page.HasMore || len(page.Turns) == 0 {
			return pages, nil
		}
		before = page.Turns[0] // 该页最旧的轮 = 下一页的 before_turn_id
	}
}

// indexSessionPages 把某会话的历史页写入会话集合（按旧 → 新；游标之前的轮跳过 = 断点续建）。
func (x *sessionIndexer) indexSessionPages(ctx context.Context, workdir string, s sessionRef, pages []historyPage, cursorTurn string) {
	// 轮次升序索引（用于判定「游标之后」）
	order := map[string]int{}
	total := 0
	for i := len(pages) - 1; i >= 0; i-- {
		for _, t := range pages[i].Turns {
			if _, ok := order[t]; !ok {
				order[t] = total
				total++
			}
		}
	}
	cursorIdx := -1
	if cursorTurn != "" {
		if v, ok := order[cursorTurn]; ok {
			cursorIdx = v
		}
	}
	x.setProgress(workdir, func(p *sessionProgress) { p.TotalTurns += total })

	for i := len(pages) - 1; i >= 0; i-- { // 旧 → 新
		if ctx.Err() != nil {
			return
		}
		page := pages[i]
		docs := make([]vfts.SessionDoc, 0, len(page.Messages))
		for _, m := range page.Messages {
			turnID := pluginsdk.Str(m["turn_id"])
			if cursorIdx >= 0 {
				if idx, ok := order[turnID]; !ok || idx <= cursorIdx {
					continue // 已索引过的轮 → 跳过（断点续建）
				}
			}
			if d, ok := sessionDocOf(s.SessionID, s.DialogID, m); ok {
				docs = append(docs, d)
			}
		}
		if len(docs) == 0 {
			continue
		}
		args := map[string]any{"workdir": workdir, "docs": docs}
		if _, err := x.m.engine(ctx, "vfts_session_index", args); err != nil {
			x.m.logfFor(workdir)("codegraph: 会话 %s 分页写入失败：%v", s.SessionID, err)
			return
		}
		done := 0
		for _, t := range page.Turns {
			if idx, ok := order[t]; ok && idx > cursorIdx {
				done++
			}
		}
		x.setProgress(workdir, func(p *sessionProgress) { p.DoneTurns += done })
	}
}

// sessionDocsOf 把消息视图行数组转 doc（过滤 role/type/空正文）。
func sessionDocsOf(sessionID, dialogID string, msgs []map[string]any) []vfts.SessionDoc {
	out := make([]vfts.SessionDoc, 0, len(msgs))
	for _, m := range msgs {
		if d, ok := sessionDocOf(sessionID, dialogID, m); ok {
			out = append(out, d)
		}
	}
	return out
}

// sessionDocOf 单条消息视图行 → doc；不入索引 → (zero,false)。
//
// 过滤：role ∈ {user, assistant} 且 type=text（工具卡片 type=tool_pair、思维链 type=reasoning、
// system 全部排除）；正文为空 → 不入索引。
func sessionDocOf(sessionID, dialogID string, m map[string]any) (vfts.SessionDoc, bool) {
	role := strings.ToLower(pluginsdk.Str(m["role"]))
	if role != "user" && role != "assistant" {
		return vfts.SessionDoc{}, false
	}
	switch typ := pluginsdk.Str(m["type"]); typ {
	case "", "text":
	default:
		return vfts.SessionDoc{}, false
	}
	text := strings.TrimSpace(pluginsdk.Str(m["content"]))
	messageID := pluginsdk.Str(m["message_id"])
	if text == "" || messageID == "" {
		return vfts.SessionDoc{}, false
	}
	if did := pluginsdk.Str(m["dialog_id"]); did != "" {
		dialogID = did
	}
	return vfts.SessionDoc{
		SessionID: sessionID, DialogID: dialogID,
		TurnID:    pluginsdk.Str(m["turn_id"]),
		MessageID: messageID,
		Role:      role,
		TS:        parseCreatedAt(pluginsdk.Str(m["created_at"])),
		Text:      text,
	}, true
}

// parseCreatedAt RFC3339Nano → UnixMilli（解析失败 → 0）。
func parseCreatedAt(s string) int64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UnixMilli()
	}
	return 0
}

// ---------------------------------------------------------------------------
// 清理 / 进度 / 通知
// ---------------------------------------------------------------------------

// clear 清空某 workdir 的会话集合（开关关闭时的收口；幂等）。
func (x *sessionIndexer) clear(workdir string) {
	wd := normalizeWorkdir(workdir)
	if wd == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := x.m.engine(ctx, "vfts_session_clear", map[string]any{"workdir": wd}); err != nil {
		x.m.logfFor(wd)("codegraph: 清空会话索引失败：%v", err)
	}
	x.mu.Lock()
	delete(x.progress, wd)
	x.mu.Unlock()
}

// setProgress 更新某 workdir 的进度（不存在则新建）。
func (x *sessionIndexer) setProgress(workdir string, fn func(p *sessionProgress)) {
	x.mu.Lock()
	p := x.progress[workdir]
	if p == nil {
		p = &sessionProgress{}
		x.progress[workdir] = p
	}
	fn(p)
	x.mu.Unlock()
}

// progressOf 取某 workdir 的进度快照（含引擎侧状态合并前的插件态）。
func (x *sessionIndexer) progressOf(workdir string) sessionProgress {
	x.mu.Lock()
	defer x.mu.Unlock()
	if p := x.progress[workdir]; p != nil {
		return *p
	}
	return sessionProgress{}
}

// notify 经既有通知面 tool-notify 发一条会话索引通知（不阻塞、不抛出）。
func (x *sessionIndexer) notify(instanceID, workdir, notice, message string) {
	if x.m.p == nil || instanceID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := fnv.New32a()
	_, _ = h.Write([]byte(workdir))
	_, _ = x.m.p.Call(ctx, subjectToolNotify, map[string]any{
		"instance_id": instanceID,
		"session_id":  "",
		"turn_id":     "",
		"notice":      notice,
		"message":     message,
		"message_id":  fmt.Sprintf("msg-%s-%08x", notice, h.Sum32()),
		"plugin":      "codegraph",
	})
}

// ---------------------------------------------------------------------------
// 消息面：vfts.session.query / vfts.session.reindex
// ---------------------------------------------------------------------------

// handleSessionQuery 处理 `vfts.session.query`（UI 会话内查找：直调引擎会话集合）。
func (x *sessionIndexer) handleSessionQuery(ctx context.Context, payload map[string]any) (any, error) {
	inst := strings.TrimSpace(pluginsdk.Str(payload["instance_id"]))
	sessionID := strings.TrimSpace(pluginsdk.Str(payload["session_id"]))
	match := strings.TrimSpace(pluginsdk.Str(payload["query"]))
	if match == "" {
		match = strings.TrimSpace(pluginsdk.Str(payload["match"])) // 兼容早期草稿字段名
	}
	if inst == "" || sessionID == "" || match == "" {
		return map[string]any{"ok": false, "reason": "instance_id / session_id / query 必填"}, nil
	}
	if !x.m.sessionsEnabled(ctx, inst) {
		return map[string]any{"ok": false, "reason": "会话内容检索未启用（plugins.codegraph.vfts.sessions）"}, nil
	}
	wd := x.m.workdirOf(inst)
	if wd == "" {
		return map[string]any{"ok": false, "reason": "未找到该实例登记的工作区"}, nil
	}
	args := map[string]any{
		"workdir":    wd,
		"scope":      "session",
		"session_id": sessionID,
		"match":      match,
	}
	if v := pluginsdk.Int(payload["limit"]); v > 0 {
		args["topK"] = v
	} else if v := pluginsdk.Int(payload["topK"]); v > 0 {
		args["topK"] = v
	}
	if role := strings.TrimSpace(pluginsdk.Str(payload["role"])); role != "" {
		args["role"] = role
	}
	out, err := x.m.engine(ctx, "vfts_query", args)
	if err != nil {
		return map[string]any{"ok": false, "reason": err.Error()}, nil
	}
	obj, _ := out.(map[string]any)
	if obj == nil {
		return map[string]any{"ok": false, "reason": "引擎应答格式异常"}, nil
	}
	if st := statusString(obj, "status"); st == "not_initialized" {
		// 索引目录丢失/损坏（或从未建立）→ 自动后台全量重建（2B §3.5「删除收口」）。
		go x.fullRebuild(wd, inst)
		reason := statusString(obj, "message")
		if reason == "" {
			reason = "会话内容索引未初始化"
		}
		return map[string]any{"ok": false, "reason": reason, "rebuilding": true}, nil
	}
	return map[string]any{"ok": true, "results": obj["hits"], "total": obj["total"]}, nil
}

// handleSessionReindex 处理 `vfts.session.reindex`：**立即回执**，后台逐会话重建（可被覆盖）。
func (x *sessionIndexer) handleSessionReindex(ctx context.Context, payload map[string]any) (any, error) {
	inst := strings.TrimSpace(pluginsdk.Str(payload["instance_id"]))
	if inst == "" {
		if wd := normalizeWorkdir(pluginsdk.Str(payload["workdir"])); wd != "" {
			inst = x.m.instForWorkdir(wd)
		}
	}
	if inst != "" {
		if !x.m.sessionsEnabled(ctx, inst) {
			return map[string]any{"ok": false, "reason": "会话内容检索未启用（plugins.codegraph.vfts.sessions）"}, nil
		}
		wd := x.m.workdirOf(inst)
		if wd == "" {
			return map[string]any{"ok": false, "reason": "未找到该实例登记的工作区"}, nil
		}
		go x.fullRebuild(wd, inst)
		return map[string]any{"ok": true, "started": true}, nil
	}
	started := false
	for _, wd := range x.m.allWorkdirs() {
		in := x.m.instForWorkdir(wd)
		if in == "" || !x.m.sessionsEnabled(ctx, in) {
			continue
		}
		go x.fullRebuild(wd, in)
		started = true
	}
	if !started {
		return map[string]any{"ok": false, "reason": "没有启用会话内容检索的项目"}, nil
	}
	return map[string]any{"ok": true, "started": true}, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
