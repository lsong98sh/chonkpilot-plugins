package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-util/scriptrun"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

// outputTailMax 是运行记录里保留的输出尾巴（字节）。
const outputTailMax = 4 << 10

// notifierBodyMax 是系统通知正文上限（rune）。
const notifierBodyMax = 200

// agentRunReq 是「以完整智能体执行 prompt」的宿主能力入参（plugin/v1/agent.run）。
type agentRunReq struct {
	WorkDir     string
	InstanceID  string
	SessionID   string
	DialogID    string
	SessionKind string
	Prompt      string
	AgentName   string
	Title       string
	DisplayName string
	TimeoutSec  int
}

// agentRunResult 是宿主智能体执行结果。
type agentRunResult struct {
	OK        bool
	Status    string // complete / incomplete / error / interrupted / cancelled
	Finish    string
	Text      string
	Error     string
	MS        int64
	SessionID string
	DialogID  string
	TurnID    string
}

// agentHost 是宿主「无父轮次启动完整智能体」的能力面。nil = 宿主未提供（旧宿主 / 单测）
// → agent 动作记为失败（不静默降级为无工具 LLM）。
type agentHost interface {
	RunAgent(ctx context.Context, req agentRunReq) (agentRunResult, error)
}

// runner 执行一次任务的 action（run / notify / agent）。
type runner struct {
	cfg      *configManager
	notifier Notifier
	logf     func(format string, args ...any)
	// ensureWorkdir 经宿主 `plugin/v1/workdir.ensure` 确保项目已打开（P3 / C13·C17）：
	// 未打开 → 宿主隐藏打开并返回主实例 id。nil = 宿主未提供（测试 / 旧宿主）→ 跳过该步。
	ensureWorkdir func(ctx context.Context, workDir string) (string, error)
	// resolveSandbox 经宿主 `plugin/v1/sandbox.resolve` 取该 work_dir 的可写白名单（P2）。
	// nil = 宿主未提供（测试 / 旧宿主）→ 视为拿不到策略，按 fail-open 执行并留痕。
	resolveSandbox func(ctx context.Context, workDir string) ([]string, error)
	// sessions 是会话归属能力面（P4 / C11）。nil = 宿主未提供 / 单测 → 跳过归属（不阻断执行）。
	sessions sessionHost
	// agents 是宿主智能体执行能力面（action.type=agent）。nil = 宿主未提供 → 该动作失败并留痕。
	agents agentHost
}

// execute 执行任务动作并返回运行记录（不落盘；调用方负责写 store / runs.jsonl）。
func (r *runner) execute(ctx context.Context, t Task, spec *ActionSpec) RunRecord {
	now := time.Now().Unix()
	switch spec.Type {
	case ActionRun:
		return r.runCommand(ctx, t, spec, now)
	case ActionNotify:
		return r.notify(t, spec, now)
	case ActionAgent:
		return r.runAgent(ctx, t, spec, now)
	}
	return RunRecord{TaskID: t.ID, At: now, Kind: spec.Type, OK: false, Error: "未知动作类型: " + spec.Type}
}

// runCommand 执行 run 动作（命令 + 可选完成通知）。
//
// 执行前（P3 / 方案 §5.5.2 ②）：work_dir 未打开 → 请宿主**隐藏打开**（C13/C17）。
// 失败 → 本轮跳过 + 记错（`workdir-open-failed`），**不 disable**（与 P2 的 `work_dir_missing`
// 区分：目录存在但打不开属可重试的瞬态问题，下一轮仍会尝试）。
func (r *runner) runCommand(ctx context.Context, t Task, spec *ActionSpec, now int64) RunRecord {
	if rec, ok := r.ensureProjectOpen(ctx, t, now); !ok {
		return rec
	}
	// 会话归属（P4 / C11）：执行前把本轮挂到该任务的子会话下（失败不阻断，见 beginCronRound）。
	ref, sessDetail := r.beginCronRound(ctx, t)
	cfg := r.cfg.Settings()
	timeout := time.Duration(cfg.DefaultTimeoutSec) * time.Second
	sb := r.sandboxFor(ctx, t)
	res, err := scriptrun.Run(ctx, scriptrun.Options{
		Command:    spec.Run.Command,
		Cwd:        t.CreatedBy.WorkDir,
		InstanceID: t.CreatedBy.InstanceID,
		Timeout:    timeout,
		MaxOutput:  256 << 10,
		Sandbox:    sb.scriptSandbox(),
	})
	rec := RunRecord{TaskID: t.ID, At: now, Kind: ActionRun, OK: res.OK, ExitCode: res.ExitCode, MS: res.MS}
	if err != nil {
		rec.OK = false
		rec.Error = err.Error()
	} else if !res.OK {
		rec.Error = res.Error
	}
	rec.OutputTail = strx.TruncBytes(strings.TrimSpace(res.Output), outputTailMax)
	// fail-open / 部分目录未打标 / 会话归属未完成 → 留痕（不阻断任务）。
	rec.Detail = appendDetail(rec.Detail, sb.Reason)
	rec.Detail = appendDetail(rec.Detail, sessDetail)
	rec.SessionID, rec.DialogID, rec.TurnID = ref.SessionID, ref.DialogID, ref.TurnID
	r.finishCronRound(ctx, strings.TrimSpace(t.CreatedBy.WorkDir), ref, rec)

	if enabled, text := spec.NotifySetting(); enabled {
		body := buildCompletionBody(t.Description, text, rec)
		if nerr := r.notifier.Notify("ChonkPilot 定时任务", body, ""); nerr != nil {
			rec.Detail = appendDetail(rec.Detail, "notify_error: "+nerr.Error())
			if r.logf != nil {
				r.logf("crontab: 完成通知失败（task=%s）: %v", t.ID, nerr)
			}
		}
	}
	return rec
}

// runAgent 执行 agent 动作：经宿主 `plugin/v1/agent.run` 启动一个**无父轮次**的完整智能体轮次
// （全工具面，可读项目文件 → 支持「定时代码审查」这类任务），等待收尾并记录结果。
//
// 会话归属：先确保「项目根会话 → 任务子会话」存在，再让宿主在该任务子会话里建轮次；
// 结果正文写入 RunRecord.OutputTail（供任务详情 / 通知查看）。
func (r *runner) runAgent(ctx context.Context, t Task, spec *ActionSpec, now int64) RunRecord {
	rec := RunRecord{TaskID: t.ID, At: now, Kind: ActionAgent}
	if spec == nil || spec.Agent == nil {
		rec.Error = "agent 动作参数缺失"
		return rec
	}
	if skip, ok := r.ensureProjectOpen(ctx, t, now); !ok {
		return skip
	}
	ref, detail := r.beginCronSession(ctx, t)
	rec.Detail = appendDetail(rec.Detail, detail)
	rec.SessionID, rec.DialogID = ref.SessionID, ref.DialogID

	if r.agents == nil {
		rec.Error = "宿主未提供 plugin/v1/agent.run（无法执行智能体动作）"
		return rec
	}
	timeoutSec := spec.Agent.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = r.cfg.Settings().DefaultTimeoutSec
	}
	start := time.Now()
	title := spec.Agent.Title
	if title == "" {
		title = t.Description
	}
	displayName := spec.Agent.DisplayName
	if displayName == "" {
		displayName = "定时任务"
	}
	res, err := r.agents.RunAgent(ctx, agentRunReq{
		WorkDir:     strings.TrimSpace(t.CreatedBy.WorkDir),
		InstanceID:  t.CreatedBy.InstanceID,
		SessionID:   ref.SessionID,
		DialogID:    ref.DialogID,
		SessionKind: sessionKindCrontabTask,
		Prompt:      spec.Agent.Prompt,
		AgentName:   spec.Agent.Agent,
		Title:       title,
		DisplayName: displayName,
		TimeoutSec:  timeoutSec,
	})
	rec.MS = time.Since(start).Milliseconds()
	if err != nil {
		rec.OK = false
		rec.Error = err.Error()
	} else {
		rec.OK = res.OK
		rec.OutputTail = strx.TruncBytes(strings.TrimSpace(res.Text), outputTailMax)
		if res.Status != "" && !res.OK {
			rec.Error = "智能体未完成（" + res.Status + "）"
			if strings.TrimSpace(res.Error) != "" {
				rec.Error += "：" + strings.TrimSpace(res.Error)
			}
		}
		if res.SessionID != "" {
			rec.SessionID = res.SessionID
		}
		if res.DialogID != "" {
			rec.DialogID = res.DialogID
		}
		rec.TurnID = res.TurnID
	}

	if enabled, text := spec.AgentNotifySetting(); enabled {
		body := buildAgentBody(t.Description, text, rec, res.Text)
		if nerr := r.notifier.Notify("ChonkPilot 定时任务", body, ""); nerr != nil {
			rec.Detail = appendDetail(rec.Detail, "notify_error: "+nerr.Error())
			if r.logf != nil {
				r.logf("crontab: 完成通知失败（task=%s）: %v", t.ID, nerr)
			}
		}
	}
	return rec
}

// notify 执行 notify 动作（系统通知）。
func (r *runner) notify(t Task, spec *ActionSpec, now int64) RunRecord {
	rec := RunRecord{TaskID: t.ID, At: now, Kind: ActionNotify, OK: true}
	body := strx.TruncRunes(HTMLToText(spec.Notify.HTML), notifierBodyMax)
	if err := r.notifier.Notify("ChonkPilot 定时任务", body, spec.Notify.ClickURL); err != nil {
		rec.OK = false
		rec.Error = err.Error()
	}
	return rec
}

// ensureProjectOpen 请求宿主确保项目已打开（P3）。返回 (rec, false) = 本轮跳过。
// `ensureWorkdir` 未注入（旧宿主 / 单测）→ 不阻断（保持 P2 行为）。
func (r *runner) ensureProjectOpen(ctx context.Context, t Task, now int64) (RunRecord, bool) {
	if r.ensureWorkdir == nil {
		return RunRecord{}, true
	}
	wd := strings.TrimSpace(t.CreatedBy.WorkDir)
	if wd == "" {
		return RunRecord{}, true
	}
	if _, err := r.ensureWorkdir(ctx, wd); err != nil {
		if r.logf != nil {
			r.logf("crontab: 任务 %s 的项目打开失败（本轮跳过，下轮重试）: %v", t.ID, err)
		}
		return RunRecord{
			TaskID: t.ID, At: now, Kind: "skipped", OK: false,
			Detail: "workdir-open-failed",
			Error:  "项目打开失败（本轮跳过，下轮重试）：" + err.Error(),
		}, false
	}
	return RunRecord{}, true
}

// buildAgentBody 构造「智能体执行结束」的通知正文（附结果摘要，详情见任务子会话）。
func buildAgentBody(desc, custom string, rec RunRecord, text string) string {
	status := "执行成功"
	if !rec.OK {
		status = "执行失败"
	}
	prefix := "「" + desc + "」"
	if strings.TrimSpace(custom) != "" {
		prefix = strings.TrimSpace(custom)
	}
	body := fmt.Sprintf("%s %s（耗时 %dms）", prefix, status, rec.MS)
	if snippet := strings.TrimSpace(text); snippet != "" {
		body += "\n" + strx.TruncRunes(snippet, 120)
	}
	if rec.Error != "" {
		body += "\n" + rec.Error
	}
	return strx.TruncRunes(body, notifierBodyMax)
}

// buildCompletionBody 构造「命令执行结束」的通知正文。
func buildCompletionBody(desc, custom string, rec RunRecord) string {
	status := "执行成功"
	if !rec.OK {
		status = "执行失败"
		if rec.ExitCode != 0 {
			status = fmt.Sprintf("执行失败（exit %d）", rec.ExitCode)
		}
	}
	prefix := "「" + desc + "」"
	if strings.TrimSpace(custom) != "" {
		prefix = strings.TrimSpace(custom)
	}
	body := fmt.Sprintf("%s %s（耗时 %dms）", prefix, status, rec.MS)
	if rec.Error != "" {
		body += "\n" + rec.Error
	}
	return strx.TruncRunes(body, notifierBodyMax)
}
