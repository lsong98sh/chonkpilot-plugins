package main

import (
	"context"
	"errors"
	"fmt"
	"os"
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
				recs = append(recs, RunRecord{TaskID: cur.ID, At: nowU, Kind: "skipped",
					OK: false, Detail: "missed", Error: "错过执行窗口（超出补偿阈值）"})
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
		overlap := false
		err = s.store.Update(id, func(cur *Task) error {
			if !cur.Enabled || cur.NextRunAt == 0 || cur.NextRunAt > nowU {
				return nil // 已被其它路径推进 / 停用
			}
			if cur.RunningSince > 0 {
				overlap = true
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
		if overlap {
			_ = s.store.AppendRun(RunRecord{TaskID: id, At: nowU, Kind: "skipped", OK: false, Detail: "overlap"})
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

// execute 执行一条已认领的任务（完成 / 失败后清除运行标记并记运行明细）。
//
// 执行前检查（P2 / C14）：work_dir 不存在 → 任务停用（`work_dir_missing`），
// 记 `skipped:workdir-missing`，只通知一次，不执行（run / notify 一律适用）。
func (s *scheduler) execute(t Task) {
	now := time.Now().Unix()
	if !workDirExists(t.CreatedBy.WorkDir) {
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
