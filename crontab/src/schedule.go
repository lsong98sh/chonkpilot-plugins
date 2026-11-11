package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-util/strx"
	"github.com/robfig/cron/v3"
)

// cronParser 只接受 5 段（分 时 日 月 周）与 @宏（@daily / @hourly / @weekly / @monthly / @every）。
// 6 段（含秒）一律拒绝——本插件不提供秒级 cron。
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// parseCron 解析 cron 表达式（5 段或 @宏）。
func parseCron(expr string) (cron.Schedule, error) {
	e := strings.TrimSpace(expr)
	if e == "" {
		return nil, errors.New("cron 不能为空")
	}
	if strings.HasPrefix(e, "@every") {
		arg := strings.TrimSpace(strings.TrimPrefix(e, "@every"))
		if arg == "" {
			return nil, errors.New("@every 需要时长参数（如 @every 5m）")
		}
		d, derr := time.ParseDuration(arg)
		if derr != nil {
			return nil, fmt.Errorf("cron 表达式非法: @every %w", derr)
		}
		if d < time.Second {
			return nil, fmt.Errorf("@every 时长必须 ≥ 1s（收到 %s）", d)
		}
	}
	if !strings.HasPrefix(e, "@") {
		if n := len(strings.Fields(e)); n != 5 {
			return nil, fmt.Errorf("cron 必须是 5 段（分 时 日 月 周），收到 %d 段", n)
		}
	}
	s, err := cronParser.Parse(e)
	if err != nil {
		return nil, fmt.Errorf("cron 表达式非法: %w", err)
	}
	return s, nil
}

// nextRun 返回 after 之后的下一次执行时间（unix 秒）。
func nextRun(expr string, after time.Time) (int64, error) {
	s, err := parseCron(expr)
	if err != nil {
		return 0, err
	}
	next := s.Next(after)
	if next.IsZero() {
		return 0, errors.New("cron 无下一次执行时间")
	}
	return next.Unix(), nil
}

// scheduleInput 是 crontab_add / crontab_update 的调度参数解析结果。
// 二选一：Cron 非空（周期）或 Once=true + RunAt（一次性）。
type scheduleInput struct {
	Cron  string
	Once  bool
	RunAt int64
}

// parseScheduleInput 解析「cron / at / delay」三选一调度参数。
//
//   - cron：5 段 cron 或 @宏（周期任务）；
//   - at：一次性绝对时间（RFC3339 / "2006-01-02 15:04[:05]" / "2006-01-02" / unix 秒）；
//   - delay：一次性相对当前时间的时长（如 "50m" / "2h"；必须 > 0）。
//
// cron 与 at/delay 互斥；at 必须晚于 now（延迟由 delay 表达）。
func parseScheduleInput(cronArg, atArg, delayArg any, now time.Time) (scheduleInput, error) {
	cronExpr := strings.TrimSpace(argString(cronArg))
	atText := strings.TrimSpace(argString(atArg))
	delayText := strings.TrimSpace(argString(delayArg))

	hasCron := cronExpr != ""
	hasAt := atText != ""
	hasDelay := delayText != ""

	switch {
	case hasCron && (hasAt || hasDelay):
		return scheduleInput{}, errors.New("cron 与 at/delay 互斥：周期任务用 cron，一次性任务用 at 或 delay")
	case hasAt && hasDelay:
		return scheduleInput{}, errors.New("at 与 delay 互斥：固定时间用 at，相对时间用 delay")
	case !hasCron && !hasAt && !hasDelay:
		return scheduleInput{}, errors.New("必须提供 cron（周期）或 at/delay（一次性）之一")
	}

	if hasCron {
		if _, err := parseCron(cronExpr); err != nil {
			return scheduleInput{}, err
		}
		return scheduleInput{Cron: cronExpr}, nil
	}

	var runAt time.Time
	if hasAt {
		t, err := parseAtTime(atText)
		if err != nil {
			return scheduleInput{}, err
		}
		runAt = t
	} else {
		d, err := time.ParseDuration(delayText)
		if err != nil {
			return scheduleInput{}, fmt.Errorf("delay 非法（示例：50m / 2h）: %w", err)
		}
		if d <= 0 {
			return scheduleInput{}, fmt.Errorf("delay 必须大于 0（收到 %s）", d)
		}
		runAt = now.Add(d)
	}
	if !runAt.After(now) {
		return scheduleInput{}, fmt.Errorf("一次性执行时间必须晚于当前时间（收到 %s）", runAt.Format("2006-01-02 15:04:05"))
	}
	return scheduleInput{Once: true, RunAt: runAt.Unix()}, nil
}

// parseAtTime 解析一次性绝对时间（本地时区）。
func parseAtTime(s string) (time.Time, error) {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, errors.New("at 的 unix 秒必须大于 0")
		}
		return time.Unix(n, 0), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(s), time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("at 时间格式非法（示例：2026-11-08 18:30 / RFC3339）: %q", s)
}

// argString 把工具参数转字符串（nil → 空）。
func argString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	default:
		return ""
	}
}

// scheduler 是 1s ticker 的调度内核（单 goroutine 扫描；任务级不重入）。
type scheduler struct {
	store  *Store
	cfg    *configManager
	runner *runner
	logf   func(format string, args ...any)
	now    func() time.Time

	mu       sync.Mutex
	lastTick time.Time
	lastNow  time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newScheduler 构造调度器。
func newScheduler(store *Store, cfg *configManager, r *runner, logf func(string, ...any)) *scheduler {
	return &scheduler{store: store, cfg: cfg, runner: r, logf: logf, now: time.Now}
}

// Start 先做启动恢复，再起 1s ticker。
func (s *scheduler) Start() {
	if s.ctx != nil {
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.Recover(s.now())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-t.C:
				s.tick(s.now())
			}
		}
	}()
}

// Stop 停止调度（等待正在收尾的 tick）。
func (s *scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

// Recover 启动恢复：清理中断标记、重算 next_run_at、按 grace 补跑 / 记 skipped:missed。
func (s *scheduler) Recover(now time.Time) {
	grace := int64(s.cfg.Settings().MissedGraceSec)
	nowU := now.Unix()
	for _, t := range s.store.List() {
		task := t
		var recs []RunRecord
		err := s.store.Update(task.ID, func(cur *Task) error {
			if cur.RunningSince > 0 {
				recs = append(recs, RunRecord{
					TaskID: cur.ID, At: nowU, Kind: "interrupted",
					OK: false, Error: "上一轮执行被中断（进程重启）",
				})
				rec := RunRecord{TaskID: cur.ID, At: nowU, Kind: "interrupted", OK: false,
					Error: "上一轮执行被中断（进程重启）"}
				cur.LastRun = &rec
				cur.RunningSince = 0
			}
			if !cur.Enabled {
				cur.NextRunAt = 0
				return nil
			}
			// 一次性任务：不重算下一次；错过补偿窗口 → 终态停用（不执行）。
			if cur.Once {
				if cur.RunAt <= 0 {
					cur.Enabled = false
					cur.NextRunAt = 0
					cur.DisabledReason = ReasonOnceMissed
					rec := RunRecord{TaskID: cur.ID, At: nowU, Kind: "skipped",
						OK: false, Detail: "missed", Error: "一次性任务的 run_at 非法"}
					recs = append(recs, rec)
					cp := rec
					cur.LastRun = &cp
					return nil
				}
				if cur.RunAt <= nowU {
					if cur.RunAt >= nowU-grace {
						cur.NextRunAt = nowU // grace 窗口内 → 立即补跑
						return nil
					}
					rec := RunRecord{TaskID: cur.ID, At: nowU, Kind: "skipped",
						OK: false, Detail: "missed", Error: "错过一次性执行窗口（超出补偿阈值）"}
					recs = append(recs, rec)
					cp := rec
					cur.LastRun = &cp
					cur.Enabled = false
					cur.NextRunAt = 0
					cur.DisabledReason = ReasonOnceMissed
					return nil
				}
				cur.NextRunAt = cur.RunAt
				return nil
			}
			sched, perr := parseCron(cur.Cron)
			if perr != nil {
				cur.NextRunAt = 0
				if s.logf != nil {
					s.logf("crontab: 任务 %s 的 cron 非法（已停用调度）: %v", cur.ID, perr)
				}
				return nil
			}
			next := sched.Next(now)
			if cur.NextRunAt > 0 && cur.NextRunAt <= nowU {
				if cur.NextRunAt >= nowU-grace {
					cur.NextRunAt = nowU // grace 窗口内 → 立即补跑
					return nil
				}
				rec := RunRecord{TaskID: cur.ID, At: nowU, Kind: "skipped",
					OK: false, Detail: "missed", Error: "错过执行窗口（超出补偿阈值）"}
				recs = append(recs, rec)
				cp := rec
				cur.LastRun = &cp // 诊断可见：list 的 last_run 不再停留在更早的成功记录
			}
			cur.NextRunAt = next.Unix()
			return nil
		})
		if err != nil {
			if !errors.Is(err, ErrTaskNotFound) && s.logf != nil {
				s.logf("crontab: 启动恢复任务 %s 失败: %v", task.ID, err)
			}
			continue
		}
		for _, rec := range recs {
			_ = s.store.AppendRun(rec)
		}
	}
}

// tick 扫描到期任务（1s 一次）；同一任务上一轮未结束 → 推进下次时间并记 skipped:overlap。
func (s *scheduler) tick(now time.Time) {
	s.mu.Lock()
	last := s.lastTick
	s.lastTick = now
	s.mu.Unlock()

	// 系统时间回拨 > 5min → 重算全部 next_run_at（不补跑）。
	if !last.IsZero() && now.Before(last.Add(-5*time.Minute)) {
		if s.logf != nil {
			s.logf("crontab: 检测到系统时间回拨，重算全部下次执行时间（不补跑）")
		}
		s.rescheduleAll(now)
		return
	}

	nowU := now.Unix()
	for _, t := range s.store.List() {
		if !t.Enabled || t.NextRunAt == 0 || t.NextRunAt > nowU {
			continue
		}
		// 一次性任务：只认领一次，NextRunAt 立即清零（执行收口后自动停用）。
		if t.Once {
			s.scheduleOnce(&t, nowU)
			continue
		}
		sched, err := parseCron(t.Cron)
		if err != nil {
			if s.logf != nil {
				s.logf("crontab: 任务 %s 的 cron 非法（跳过本次）: %v", t.ID, err)
			}
			continue
		}
		next := sched.Next(now).Unix()
		if next <= nowU {
			next = sched.Next(now.Add(time.Second)).Unix()
		}

		id := t.ID
		var overlapRec *RunRecord
		err = s.store.Update(id, func(cur *Task) error {
			if !cur.Enabled || cur.NextRunAt == 0 || cur.NextRunAt > nowU {
				return nil // 已被其它路径推进 / 停用
			}
			if cur.RunningSince > 0 {
				rec := RunRecord{TaskID: id, At: nowU, Kind: "skipped", OK: false, Detail: "overlap"}
				cp := rec
				cur.LastRun = &cp
				overlapRec = &rec
				cur.NextRunAt = next
				return nil
			}
			cur.NextRunAt = next
			cur.RunningSince = nowU
			return nil
		})
		if err != nil {
			if !errors.Is(err, ErrTaskNotFound) && s.logf != nil {
				s.logf("crontab: 认领任务 %s 失败: %v", id, err)
			}
			continue
		}
		if overlapRec != nil {
			_ = s.store.AppendRun(*overlapRec)
			continue
		}
		claimed, ok := s.store.Get(id)
		if !ok {
			continue
		}
		s.wg.Add(1)
		go func(task Task) {
			defer s.wg.Done()
			s.execute(task)
		}(claimed)
	}
}

// scheduleOnce 认领一条到期的一次性任务：NextRunAt 清零（不再重复），上一轮仍在跑 →
// 记 skipped:overlap 并终态停用（一次性任务没有下一轮可推进）。
func (s *scheduler) scheduleOnce(t *Task, nowU int64) {
	id := t.ID
	var overlapRec *RunRecord
	err := s.store.Update(id, func(cur *Task) error {
		if !cur.Enabled || !cur.Once || cur.NextRunAt == 0 || cur.NextRunAt > nowU {
			return nil // 已被其它路径推进 / 停用
		}
		if cur.RunningSince > 0 {
			rec := RunRecord{TaskID: id, At: nowU, Kind: "skipped", OK: false, Detail: "overlap"}
			cp := rec
			cur.LastRun = &cp
			overlapRec = &rec
			cur.NextRunAt = 0
			cur.Enabled = false
			cur.DisabledReason = ReasonOnceMissed
			return nil
		}
		cur.NextRunAt = 0
		cur.RunningSince = nowU
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrTaskNotFound) && s.logf != nil {
			s.logf("crontab: 认领一次性任务 %s 失败: %v", id, err)
		}
		return
	}
	if overlapRec != nil {
		_ = s.store.AppendRun(*overlapRec)
		return
	}
	claimed, ok := s.store.Get(id)
	if !ok {
		return
	}
	s.wg.Add(1)
	go func(task Task) {
		defer s.wg.Done()
		s.execute(task)
	}(claimed)
}

// execute 执行一条已认领的任务（完成 / 失败后清除运行标记并记运行明细）。
//
// 执行前检查（P2 / C14）：run / agent 依赖项目目录，work_dir 不存在 → 任务停用
// （`work_dir_missing`），记 `skipped:workdir-missing`，只通知一次，不执行。
// notify 不访问工作目录，允许在无项目的全局会话中运行，不参与该检查。
func (s *scheduler) execute(t Task) {
	now := time.Now().Unix()
	if t.Action.Type != ActionNotify && !workDirExists(t.CreatedBy.WorkDir) {
		s.disableForMissingWorkDir(t, now)
		return
	}
	// work_dir 已恢复 → 清掉旧的停用原因（任务仍保持停用，由用户显式启用）。
	if t.DisabledReason != "" {
		_ = s.store.Update(t.ID, func(cur *Task) error {
			cur.DisabledReason = ""
			return nil
		})
	}
	spec, err := parseStoredAction(t.Action)
	var rec RunRecord
	if err != nil {
		rec = RunRecord{TaskID: t.ID, At: now, Kind: t.Action.Type, OK: false, Error: err.Error()}
	} else {
		rec = s.runner.execute(s.ctx, t, spec)
	}
	_ = s.store.Update(t.ID, func(cur *Task) error {
		cur.RunningSince = 0
		cur.RunCount++
		cp := rec
		cur.LastRun = &cp
		// 一次性任务：执行一次即终态（成功 / 失败都不再重跑）。
		// 仅当当前定义仍是「同一次调度」时才自动停用；若运行期间被 update 改期/改成周期，
		// 不能把新定义清掉（旧执行只影响旧定义）。
		if t.Once && cur.Once && cur.RunAt == t.RunAt {
			cur.Enabled = false
			cur.NextRunAt = 0
			if cur.DisabledReason == "" {
				cur.DisabledReason = ReasonOnceCompleted
			}
		}
		// 会话归属（P4 / C11）：记录首次建立的子会话 / 对话，供任务详情页只读展示。
		if rec.SessionID != "" {
			cur.SessionID = rec.SessionID
		}
		if rec.DialogID != "" {
			cur.DialogID = rec.DialogID
		}
		return nil
	})
	_ = s.store.AppendRun(rec)
	if !rec.OK && s.logf != nil {
		s.logf("crontab: 任务 %s 执行失败（kind=%s）: %s", t.ID, rec.Kind, rec.Error)
	}
}

// disableForMissingWorkDir 把任务置为停用（work_dir_missing），记运行明细，并在**首次**停用时通知。
func (s *scheduler) disableForMissingWorkDir(t Task, now int64) {
	wd := strings.TrimSpace(t.CreatedBy.WorkDir)
	rec := RunRecord{
		TaskID: t.ID, At: now, Kind: "skipped", OK: false, Detail: "workdir-missing",
		Error: "work_dir 不存在或不可访问：" + wd,
	}
	first := t.DisabledReason != ReasonWorkDirMissing
	_ = s.store.Update(t.ID, func(cur *Task) error {
		cur.RunningSince = 0
		cur.Enabled = false
		cur.NextRunAt = 0
		cur.DisabledReason = ReasonWorkDirMissing
		cp := rec
		cur.LastRun = &cp
		return nil
	})
	_ = s.store.AppendRun(rec)
	if s.logf != nil {
		s.logf("crontab: 任务 %s 的 work_dir 不存在，已停用（%s）", t.ID, wd)
	}
	if !first || s.runner == nil || s.runner.notifier == nil {
		return
	}
	body := strx.TruncRunes("「"+t.Description+"」的项目目录已不存在，任务已自动停用（work_dir_missing）。"+
		"请重新打开项目或修正任务后再启用。", notifierBodyMax)
	if err := s.runner.notifier.Notify("ChonkPilot 定时任务", body, ""); err != nil && s.logf != nil {
		s.logf("crontab: 停用通知失败（task=%s）: %v", t.ID, err)
	}
}

// workDirExists 判定目录是否存在且为目录（空 = false）。
func workDirExists(dir string) bool {
	d := strings.TrimSpace(dir)
	if d == "" {
		return false
	}
	st, err := os.Stat(d)
	return err == nil && st.IsDir()
}

// rescheduleAll 重算全部启用任务的下次执行时间（时间回拨 / 配置变更兜底）。
func (s *scheduler) rescheduleAll(now time.Time) {
	for _, t := range s.store.List() {
		task := t
		_ = s.store.Update(task.ID, func(cur *Task) error {
			if !cur.Enabled {
				cur.NextRunAt = 0
				return nil
			}
			if cur.Once {
				cur.NextRunAt = cur.RunAt
				return nil
			}
			n, err := nextRun(cur.Cron, now)
			if err != nil {
				cur.NextRunAt = 0
				return nil
			}
			cur.NextRunAt = n
			return nil
		})
	}
}
