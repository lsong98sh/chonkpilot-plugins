// session.go —— 定时任务的**会话归属**（SANDBOX-WRITE-1 P4 / 方案 §5.5.3，C11）。
//
// 层级（每项目一棵树）：
//
//	cron 对话（dialog，kind="crontab"，标题「定时任务」）
//	└─ 根会话 id = "crontab-xxxxxxxx"（parent_id=""）
//	   └─ 每任务 1 个子会话 id = "crontab-<task_id>"（parent_id = 根会话 id）
//	        └─ 每轮 = 一个 turn
//
// 约束（C8）：插件**不直接开 bbolt** —— 建会话 / 建轮次 / 收轮次一律经宿主能力面
// （`plugin/v1/session.{ensure,ensure-turn,complete-turn}`，UDS 方法面，零新增 MQ 主题）。
// 执行主体标记 `origin=scheduler`（审计 / UI；不冒充用户）。
//
// 失败语义：会话归属是**审计附加物**，失败**不阻断任务执行**（RunRecord.Detail 留痕
// `session_bind_failed: …`）。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

// 宿主桥方法（UDS 方法面；非 MQ 主题）。
const (
	methodSessionEnsure       = "plugin/v1/session.ensure"
	methodSessionEnsureTurn   = "plugin/v1/session.ensure-turn"
	methodSessionCompleteTurn = "plugin/v1/session.complete-turn"
	methodSessionAppendNotice = "plugin/v1/session.append-notice"
	methodSessionHistory      = "plugin/v1/session.history"
	methodSessionGet          = "plugin/v1/session.get"
	methodInstanceResolve     = "plugin/v1/instance.resolve"
	methodAgentRun            = "plugin/v1/agent.run"
)

// 会话归属常量（与 data 侧 facade.SessionKind* / SessionOriginScheduler 同值）。
const (
	cronDialogTitle        = "定时任务"
	sessionOriginScheduler = "scheduler"
	sessionKindCrontab     = "crontab"
	sessionKindCrontabTask = "crontab_task"
	// sessionKindCrontabOwn 是 session_mode=own 的任务顶层会话（独立 dialog，进会话列表；
	// 非 crontab 保留类型，故不被 IsHiddenDialogKind 排除）。
	sessionKindCrontabOwn = "crontab_own"
)

// sessionTitleMax 是会话标题上限（data 侧校验 ≤128 字符；此处留余量）。
const sessionTitleMax = 100

// rootSessionID 返回该项目的根会话 id（形如 `crontab-xxxxxxxx`）。
//
// 由 work_dir 确定性派生 → 同一项目恒同一 id、重复调用幂等（不同项目不撞）。
func rootSessionID(workDir string) string {
	key := strings.ToLower(filepath.Clean(strings.TrimSpace(workDir)))
	sum := sha256.Sum256([]byte("chonkpilot-crontab:" + key))
	return "crontab-" + hex.EncodeToString(sum[:4])
}

// taskSessionID 返回某任务的子会话 id（稳定：同任务跨轮次复用同一会话）。
func taskSessionID(taskID string) string { return "crontab-" + taskID }

// sessionRef 是一轮执行挂靠的会话位置（空字段 = 该步未成功）。
type sessionRef struct {
	DialogID  string
	SessionID string
	TurnID    string
}

// sessionEnsureReq 是 `plugin/v1/session.ensure` 的入参（宿主按 work_dir 解析实例）。
type sessionEnsureReq struct {
	WorkDir         string
	SessionID       string
	ParentSessionID string
	DialogID        string
	Kind            string
	Title           string
}

// sessionHost 是宿主「会话归属」能力面。nil = 宿主未提供（旧宿主 / 单测）→ 跳过归属，
// 任务照常执行（与 `workdir.ensure` 的注入式降级同口径）。
type sessionHost interface {
	EnsureSession(ctx context.Context, req sessionEnsureReq) (dialogID string, err error)
	EnsureTurn(ctx context.Context, workDir string, ref sessionRef) error
	CompleteTurn(ctx context.Context, workDir string, ref sessionRef, status, finishReason string) error
}

// noticeHost 是「向既有会话追加通知」的可选能力。宿主未提供（旧宿主 / 单测 fake）→
// 仅系统通知 + 任务页结果，不阻断任务。
type noticeHost interface {
	AppendNotice(ctx context.Context, req noticeReq) (noticeResult, error)
}

type noticeReq struct {
	InstanceID string
	WorkDir    string
	SessionID  string
	TaskID     string
	Message    string
}

type noticeResult struct {
	SessionID string
	TurnID    string
	MessageID string
	DialogID  string
}

// ── 宿主桥实现 ────────────────────────────────────────────────

// bridgeSessions 是 sessionHost 的宿主桥实现（独立进程插件）。
type bridgeSessions struct{ p *pluginsdk.Plugin }

// newBridgeSessions 构造会话能力客户端（p 为 nil → nil，调用方按「宿主不支持」处理）。
func newBridgeSessions(p *pluginsdk.Plugin) *bridgeSessions {
	if p == nil {
		return nil
	}
	return &bridgeSessions{p: p}
}

// EnsureSession 幂等建会话，返回其所属对话 id（根会话由宿主自举建对话并回带）。
func (b *bridgeSessions) EnsureSession(ctx context.Context, req sessionEnsureReq) (string, error) {
	payload := map[string]any{
		"work_dir":   req.WorkDir,
		"session_id": req.SessionID,
		"kind":       req.Kind,
		"origin":     sessionOriginScheduler,
	}
	if req.ParentSessionID != "" {
		payload["parent_session_id"] = req.ParentSessionID
	}
	if req.DialogID != "" {
		payload["dialog_id"] = req.DialogID
	}
	if req.Title != "" {
		payload["title"] = req.Title
	}
	var out struct {
		DialogID string `json:"dialog_id"`
	}
	if err := b.p.CallInto(ctx, methodSessionEnsure, payload, &out); err != nil {
		return "", err
	}
	if out.DialogID == "" {
		return "", errors.New("宿主未返回 dialog_id")
	}
	return out.DialogID, nil
}

// EnsureTurn 幂等建本轮（已存在复用）。
func (b *bridgeSessions) EnsureTurn(ctx context.Context, workDir string, ref sessionRef) error {
	var out struct {
		OK bool `json:"ok"`
	}
	return b.p.CallInto(ctx, methodSessionEnsureTurn, map[string]any{
		"work_dir":   workDir,
		"session_id": ref.SessionID,
		"dialog_id":  ref.DialogID,
		"turn_id":    ref.TurnID,
	}, &out)
}

// RunAgent 经宿主 `plugin/v1/agent.run` 启动完整智能体轮次并等待收尾。
func (b *bridgeSessions) RunAgent(ctx context.Context, req agentRunReq) (agentRunResult, error) {
	payload := map[string]any{
		"work_dir":     req.WorkDir,
		"session_id":   req.SessionID,
		"dialog_id":    req.DialogID,
		"kind":         req.SessionKind,
		"prompt":       req.Prompt,
		"title":        req.Title,
		"display_name": req.DisplayName,
		"timeout_sec":  req.TimeoutSec,
	}
	if req.InstanceID != "" {
		payload["instance_id"] = req.InstanceID
	}
	if req.AgentName != "" {
		payload["agent"] = req.AgentName
	}
	var out struct {
		OK        bool   `json:"ok"`
		State     string `json:"state"`
		Finish    string `json:"finish"`
		Text      string `json:"text"`
		Error     string `json:"error"`
		MS        int64  `json:"ms"`
		SessionID string `json:"session_id"`
		DialogID  string `json:"dialog_id"`
		TurnID    string `json:"turn_id"`
		Partial   bool   `json:"partial"`
	}
	// 桥调用超时必须覆盖宿主侧的等待上限（宿主按 timeout_sec 收尾；桥缺省约 15s）。
	wait := time.Duration(req.TimeoutSec) * time.Second
	if wait > 0 {
		wait += 15 * time.Second
	}
	if err := b.p.CallIntoWithTimeout(ctx, wait, methodAgentRun, payload, &out); err != nil {
		return agentRunResult{}, err
	}
	return agentRunResult{
		OK: out.OK, Status: out.State, Finish: out.Finish, Text: out.Text, Error: out.Error,
		MS: out.MS, SessionID: out.SessionID, DialogID: out.DialogID, TurnID: out.TurnID,
	}, nil
}

// CompleteTurn 收本轮终态（complete / error）。
func (b *bridgeSessions) CompleteTurn(ctx context.Context, workDir string, ref sessionRef, status, finishReason string) error {
	var out struct {
		OK bool `json:"ok"`
	}
	return b.p.CallInto(ctx, methodSessionCompleteTurn, map[string]any{
		"work_dir":      workDir,
		"turn_id":       ref.TurnID,
		"dialog_id":     ref.DialogID,
		"status":        status,
		"finish_reason": finishReason,
	}, &out)
}

// AppendNotice 向既有会话追加一条 kind=notify 通知消息，并让宿主广播
// tool-notify{notice=cron-complete}（前端即时显示；历史从 DB 回填）。
func (b *bridgeSessions) AppendNotice(ctx context.Context, req noticeReq) (noticeResult, error) {
	var out struct {
		OK        bool   `json:"ok"`
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
		MessageID string `json:"message_id"`
		DialogID  string `json:"dialog_id"`
	}
	err := b.p.CallInto(ctx, methodSessionAppendNotice, map[string]any{
		"instance_id": req.InstanceID,
		"work_dir":    req.WorkDir,
		"session_id":  req.SessionID,
		"task_id":     req.TaskID,
		"message":     req.Message,
	}, &out)
	return noticeResult{SessionID: out.SessionID, TurnID: out.TurnID,
		MessageID: out.MessageID, DialogID: out.DialogID}, err
}

// notifyCreator 把任务完成摘要写回**主会话**（失败只留痕，不阻断任务）。
//
// 目标 = `created_by.top_session`（gateway 注入的顶层会话）→ 回退 `created_by.session_id`
// （旧任务 / 无 gateway 上下文）。session_mode=own 的任务自带结果会话，**不通知**。
func (r *runner) notifyCreator(ctx context.Context, t Task, rec RunRecord, body string) {
	if r.sessions == nil || sessionModeOf(t) == SessionModeOwn {
		return
	}
	target := strings.TrimSpace(t.CreatedBy.TopSession)
	if target == "" {
		target = strings.TrimSpace(t.CreatedBy.SessionID)
	}
	if target == "" {
		return
	}
	msg := strings.TrimSpace(body)
	if msg == "" {
		msg = "定时任务已完成：" + t.Description
	}
	msg += "\n任务 ID：" + t.ID
	if rec.SessionID != "" {
		msg += "\n任务会话：" + rec.SessionID
	}
	nh, ok := r.sessions.(noticeHost)
	if !ok {
		return
	}
	_, err := nh.AppendNotice(ctx, noticeReq{
		InstanceID: t.CreatedBy.InstanceID,
		WorkDir:    strings.TrimSpace(t.CreatedBy.WorkDir),
		SessionID:  target,
		TaskID:     t.ID,
		Message:    msg,
	})
	if err != nil && r.logf != nil {
		r.logf("crontab: 主会话通知失败（task=%s session=%s）: %v", t.ID, target, err)
	}
}

// ── 执行前 / 执行后 ──────────────────────────────────────────

// beginCronSession 确保「项目根会话 → 任务子会话」存在（P4 / C11），不建轮次。
// 返回 (ref, detail)：detail 非空 = 归属未完成的原因（并入 RunRecord.Detail，**不阻断执行**）。
func (r *runner) beginCronSession(ctx context.Context, t Task) (sessionRef, string) {
	if r.sessions == nil {
		return sessionRef{}, ""
	}
	wd := strings.TrimSpace(t.CreatedBy.WorkDir)
	if wd == "" {
		return sessionRef{}, "session_bind_skipped: work_dir 为空"
	}
	if sessionModeOf(t) == SessionModeOwn {
		// own：任务自己就是**顶层会话**（无父、独立 dialog；宿主自举目录行 → 进会话列表）。
		// 不发完成通知（结果就在该会话里，用户从会话一览直接打开）。
		sessionID := taskSessionID(t.ID)
		dialogID, err := r.sessions.EnsureSession(ctx, sessionEnsureReq{
			WorkDir: wd, SessionID: sessionID, Kind: sessionKindCrontabOwn,
			Title: taskSessionTitle(t),
		})
		if err != nil {
			return sessionRef{}, "session_bind_failed: " + err.Error()
		}
		return sessionRef{DialogID: dialogID, SessionID: sessionID}, ""
	}
	root := rootSessionID(wd)
	dialogID, err := r.sessions.EnsureSession(ctx, sessionEnsureReq{
		WorkDir: wd, SessionID: root, Kind: sessionKindCrontab, Title: cronDialogTitle,
	})
	if err != nil {
		return sessionRef{}, "session_bind_failed: " + err.Error()
	}
	sub := taskSessionID(t.ID)
	subDialog, err := r.sessions.EnsureSession(ctx, sessionEnsureReq{
		WorkDir: wd, SessionID: sub, ParentSessionID: root, DialogID: dialogID,
		Kind: sessionKindCrontabTask, Title: taskSessionTitle(t),
	})
	if err != nil {
		return sessionRef{}, "session_bind_failed: " + err.Error()
	}
	if subDialog != "" {
		dialogID = subDialog
	}
	return sessionRef{DialogID: dialogID, SessionID: sub}, ""
}

// beginCronRound 在命令执行前把本轮挂到「该任务的子会话」下（P4 / C11）。
//
// 返回 (ref, detail)：detail 非空 = 归属未完成的原因（并入 RunRecord.Detail，**不阻断执行**）。
func (r *runner) beginCronRound(ctx context.Context, t Task) (sessionRef, string) {
	ref, detail := r.beginCronSession(ctx, t)
	if ref.SessionID == "" {
		return ref, detail
	}
	ref.TurnID = newCronTurnID()
	if err := r.sessions.EnsureTurn(ctx, strings.TrimSpace(t.CreatedBy.WorkDir), ref); err != nil {
		return sessionRef{SessionID: ref.SessionID, DialogID: ref.DialogID}, "session_bind_failed: " + err.Error()
	}
	return ref, ""
}

// finishCronRound 收本轮终态（best-effort；失败只记日志，不改运行结果）。
func (r *runner) finishCronRound(ctx context.Context, workDir string, ref sessionRef, rec RunRecord) {
	if r.sessions == nil || ref.TurnID == "" {
		return
	}
	status, reason := "complete", ""
	if !rec.OK {
		status = "error"
		reason = strx.TruncRunes(rec.Error, 200)
	}
	if err := r.sessions.CompleteTurn(ctx, workDir, ref, status, reason); err != nil && r.logf != nil {
		r.logf("crontab: 收轮次失败（task=%s turn=%s）: %v", rec.TaskID, ref.TurnID, err)
	}
}

// taskSessionTitle 返回任务子会话的标题（任务简述；≤128 字符校验余量）。
func taskSessionTitle(t Task) string {
	desc := strings.TrimSpace(t.Description)
	if desc == "" {
		desc = t.ID
	}
	return strx.TruncRunes(desc, sessionTitleMax)
}

// newCronTurnID 生成一轮的 turn id（进程内单调；同任务串行执行，不重入）。
func newCronTurnID() string {
	return fmt.Sprintf("turn-cron-%d", time.Now().UnixNano())
}

// ── 任务详情页：会话只读展示（C12）─────────────────────────────

// openableSessions 返回任务**可打开**的会话列表（G-99：详情页「打开会话内容」入口）。
//
// 每任务恒 1 个子会话（`kind=crontab_task`）；未执行过（无 session/dialog）→ 空。
// 只读：不建会话、不改任务。
func openableSessions(t Task) []map[string]any {
	if strings.TrimSpace(t.SessionID) == "" || strings.TrimSpace(t.DialogID) == "" {
		return nil
	}
	return []map[string]any{{
		"session_id": t.SessionID,
		"dialog_id":  t.DialogID,
		"title":      taskSessionTitle(t),
	}}
}

// resolveInstance 经宿主 `plugin/v1/instance.resolve` 取该项目主窗口实例（未命中 → 空）。
func resolveInstance(ctx context.Context, p *pluginsdk.Plugin, workDir string) string {
	if p == nil || strings.TrimSpace(workDir) == "" {
		return ""
	}
	var out struct {
		Found      bool   `json:"found"`
		InstanceID string `json:"instance_id"`
	}
	if err := p.CallInto(ctx, methodInstanceResolve, map[string]any{"work_dir": workDir}, &out); err != nil || !out.Found {
		return ""
	}
	return out.InstanceID
}

// sessionInfo 汇总任务的会话展示信息（任务详情页用；**只读**，不建会话）。
//
// 数据来源 = 既有只读能力面：`plugin/v1/session.get`（标题 / 创建时间）+
// `plugin/v1/session.history`（轮次列表）。任何一步失败只降级为 error 字段，不报错。
func sessionInfo(ctx context.Context, p *pluginsdk.Plugin, t Task) map[string]any {
	info := map[string]any{
		"available":  false,
		"session_id": t.SessionID,
		"dialog_id":  t.DialogID,
	}
	if p == nil || t.SessionID == "" || t.DialogID == "" {
		// 尚未执行过（会话在首次 run 时建立）→ available=false，UI 显示「尚无会话」。
		return info
	}
	instanceID := resolveInstance(ctx, p, t.CreatedBy.WorkDir)
	if instanceID == "" {
		info["error"] = "instance_not_found"
		return info
	}
	info["available"] = true
	info["instance_id"] = instanceID
	// 可打开会话（G-99）：任务详情页「打开会话内容」入口按此列表逐项发起
	// `gui.window.open-chat{session_id}`（前端既有窗口能力；不新增 MQ 主题/payload）。
	if open := openableSessions(t); len(open) > 0 {
		info["openable_sessions"] = open
	}

	var sess struct {
		Data map[string]any `json:"data"`
	}
	if err := p.CallInto(ctx, methodSessionGet, map[string]any{
		"instance_id": instanceID, "id": t.SessionID, "dialog_id": t.DialogID,
	}, &sess); err == nil && sess.Data != nil {
		info["title"] = pluginsdk.Str(sess.Data["title"])
		info["created_at"] = pluginsdk.Str(sess.Data["created_at"])
		info["origin"] = pluginsdk.Str(sess.Data["origin"])
	} else if err != nil {
		info["error"] = "session_get_failed: " + err.Error()
	}

	var hist struct {
		Messages struct {
			Turns []map[string]any `json:"turns"`
		} `json:"messages"`
	}
	if err := p.CallInto(ctx, methodSessionHistory, map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"session_id": t.SessionID, "dialog_id": t.DialogID},
	}, &hist); err != nil {
		info["error"] = "session_history_failed: " + err.Error()
		return info
	}
	turns := hist.Messages.Turns
	info["turn_count"] = len(turns)
	if len(turns) > 0 {
		last := turns[len(turns)-1]
		info["last_turn_at"] = pluginsdk.Str(last["created_at"])
		info["last_turn_status"] = pluginsdk.Str(last["status"])
	}
	return info
}
