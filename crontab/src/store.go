package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 CGO）
)

// 存储文件（legacy JSON 仅导入一次，之后不再读写）。
const (
	dbFileName      = "crontab.db"
	legacyTasksName = "tasks.json"
	legacyRunsName  = "runs.jsonl"
)

// schemaVersion 是 crontab.db 的 schema 版本（写入 meta 表）。
const schemaVersion = 1

// runsMaxRows 是 runs 表保留的最大行数（超出裁剪最旧记录）。
const runsMaxRows = 20000

// CreatedBy 记录任务创建来源（仅作 run 的默认 cwd / 审计，不做项目隔离）。
type CreatedBy struct {
	InstanceID string `json:"instance_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	// TopSession 是创建工具调用所属的**顶层主会话**（gateway 注入）。完成后通知固定回主会话：
	// 普通顶层对话 = SessionID；子代理/子会话内创建时二者不同，此时用 TopSession。
	TopSession string `json:"top_session,omitempty"`
	WorkDir    string `json:"work_dir,omitempty"`
}

// 任务的会话模式（Task.SessionMode）。
const (
	// SessionModeMain（缺省）：run/agent 在项目「定时任务」隐藏对话下的任务子会话执行；
	// 完成后通知 **主会话**（CreatedBy.TopSession → 回退 SessionID）。
	SessionModeMain = "main"
	// SessionModeOwn：任务自己就是顶层会话（独立 dialog，进会话列表），**不发完成通知**。
	SessionModeOwn = "own"
)

// Action 是任务的动作（type = run / notify / agent）；Param 保留规范化后的 JSON。
type Action struct {
	Type  string          `json:"type"`
	Param json.RawMessage `json:"param"`
}

// Task 是一条定时任务（全局，不区分项目）。
//
// 调度形态二选一：
//   - 周期：Cron 非空（5 段 cron / @宏），Once=false；
//   - 一次性：Once=true，RunAt=绝对执行时间（unix 秒），Cron 为空。
//     执行一次后自动停用（enabled=false, disabled_reason=completed_once），保留历史供查看。
type Task struct {
	ID     string `json:"id"`
	Cron   string `json:"cron"`
	Action Action `json:"action"`
	// Once / RunAt 是一次性调度（"50 分钟后做 X"/固定时间）：
	//   once=true + run_at=unix 秒；cron 为空。
	Once  bool  `json:"once,omitempty"`
	RunAt int64 `json:"run_at,omitempty"`
	// SessionMode 决定结果会话与完成通知（空 = main，兼容旧任务）：
	//   main → 隐藏「定时任务」对话下的任务子会话，完成后通知主会话；
	//   own  → 任务自己是顶层会话（进会话列表），不通知。
	SessionMode  string     `json:"session_mode,omitempty"`
	Description  string     `json:"description"`
	Enabled      bool       `json:"enabled"`
	CreatedAt    int64      `json:"created_at"`
	CreatedBy    CreatedBy  `json:"created_by"`
	NextRunAt    int64      `json:"next_run_at"`
	RunCount     int        `json:"run_count"`
	LastRun      *RunRecord `json:"last_run,omitempty"`
	RunningSince int64      `json:"running_since,omitempty"`
	// Sandbox 是**任务级**沙箱开关（SANDBOX-WRITE-1 P2）：nil = 继承插件配置
	// `plugins.crontab.sandbox`；true/false = 仅本任务生效。
	Sandbox *bool `json:"sandbox,omitempty"`
	// DisabledReason 是自动停用的原因：
	//   - work_dir_missing：项目目录不存在（可恢复）；
	//   - completed_once：一次性任务已执行完成（终态，需改期/新建）；
	//   - once_missed：一次性任务在补偿窗口外错过（终态）。
	// 用户重新启用或 work_dir 恢复后清空。
	DisabledReason string `json:"disabled_reason,omitempty"`
	// SessionID / DialogID 是该任务**子会话**的归属（SANDBOX-WRITE-1 P4 / C11）：
	// 首次 run 时经宿主能力面建立（`plugin/v1/session.ensure`），此后复用以展示会话内容。
	// 未执行过 → 空（任务详情页显示「尚无会话」）。
	SessionID string `json:"session_id,omitempty"`
	DialogID  string `json:"dialog_id,omitempty"`
}

// 任务停用原因（DisabledReason 取值）。
const (
	ReasonWorkDirMissing = "work_dir_missing"
	ReasonOnceCompleted  = "completed_once"
	ReasonOnceMissed     = "once_missed"
)

// RunRecord 是一条运行明细（runs 表 / Task.LastRun）。
type RunRecord struct {
	TaskID string `json:"task_id"`
	At     int64  `json:"at"`
	Kind   string `json:"kind"` // run | notify | agent | skipped | interrupted
	// Trigger 区分计划触发（空 = 定时）与人工补跑（manual）；补跑不改变下次执行时间。
	Trigger    string `json:"trigger,omitempty"`
	OK         bool   `json:"ok"`
	ExitCode   int    `json:"exit_code,omitempty"`
	MS         int64  `json:"ms,omitempty"`
	Error      string `json:"error,omitempty"`
	Detail     string `json:"detail,omitempty"`
	OutputTail string `json:"output_tail,omitempty"`
	// 本轮挂靠的会话 / 轮次（SANDBOX-WRITE-1 P4 / C11；**只增**，未归属时不写键）。
	SessionID string `json:"session_id,omitempty"`
	DialogID  string `json:"dialog_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
}

// tasksFile 是旧版 tasks.json 的落盘形态（仅用于一次性导入）。
type tasksFile struct {
	Tasks []*Task `json:"tasks"`
}

// Store 是 crontab.db 的持久化（SQLite + WAL；多进程可同时打开）。
//
// 任务定义在 tasks 表、运行明细在 runs 表；旧版 tasks.json / runs.jsonl 在
// Load 时一次性导入并改名，之后不再读写。
type Store struct {
	dir  string
	path string // 数据库文件路径（测试 / 诊断）

	// mu 只保护打开 / 关闭与 db 指针；数据库本身并发安全。
	mu sync.Mutex
	db *sql.DB
}

// errStoreClosed 表示 Store 尚未 Load 或已 Close。
var errStoreClosed = errors.New("crontab 数据库未打开或已关闭")

// ErrTaskNotFound 表示任务不存在。
var ErrTaskNotFound = errors.New("任务不存在")

// newStore 构造存储（不打开 DB；调用方随后 Load）。
func newStore(dir string) *Store {
	return &Store{
		dir:  dir,
		path: filepath.Join(dir, dbFileName),
	}
}

// conn 返回已打开的 *sql.DB（未打开 / 已关闭 → errStoreClosed）。
func (s *Store) conn() (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, errStoreClosed
	}
	return s.db, nil
}

// Load 打开数据库、幂等建表，并一次性导入旧版 JSON（导入后改名保留）。
//
// 损坏的旧 tasks.json 按旧行为备份为 tasks.json.corrupt-<ts> 并返回错误
// （插件 main 告警后按空任务启动）；DB 已建表且保持为空。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建插件数据目录失败: %w", err)
	}
	// _txlock=immediate：每个写事务在 BEGIN 时直接拿写锁（RESERVED），
	// 多进程认领同一任务时只有一个事务能先进入；第二个在 busy_timeout 内等待，
	// 拿到锁后读到的是第一个已提交的值（→ overlap / no-op）。
	dsn := "file:" + filepath.ToSlash(s.path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("打开 crontab.db 失败: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("打开 crontab.db 失败: %w", err)
	}
	s.db = db
	if err := s.migrateLocked(); err != nil {
		_ = db.Close()
		s.db = nil
		return fmt.Errorf("初始化 crontab.db 失败: %w", err)
	}
	if err := s.importLegacyTasksLocked(); err != nil {
		return err
	}
	if err := s.importLegacyRunsLocked(); err != nil {
		return err
	}
	return nil
}

// Close 关闭底层 DB（幂等）。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// migrateLocked 幂等建表（调用方持锁且 db 已打开）。
func (s *Store) migrateLocked() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tasks (
			id                     TEXT PRIMARY KEY,
			cron                   TEXT NOT NULL DEFAULT '',
			action_type            TEXT NOT NULL DEFAULT '',
			action_param           TEXT NOT NULL DEFAULT '',
			once                   INTEGER NOT NULL DEFAULT 0,
			run_at                 INTEGER NOT NULL DEFAULT 0,
			session_mode           TEXT NOT NULL DEFAULT '',
			description            TEXT NOT NULL DEFAULT '',
			enabled                INTEGER NOT NULL DEFAULT 0,
			created_at             INTEGER NOT NULL DEFAULT 0,
			created_by_instance    TEXT NOT NULL DEFAULT '',
			created_by_session     TEXT NOT NULL DEFAULT '',
			created_by_top_session TEXT NOT NULL DEFAULT '',
			created_by_work_dir    TEXT NOT NULL DEFAULT '',
			next_run_at            INTEGER NOT NULL DEFAULT 0,
			run_count              INTEGER NOT NULL DEFAULT 0,
			last_run               TEXT NOT NULL DEFAULT '',
			running_since          INTEGER NOT NULL DEFAULT 0,
			sandbox                INTEGER,
			disabled_reason        TEXT NOT NULL DEFAULT '',
			session_id             TEXT NOT NULL DEFAULT '',
			dialog_id              TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(enabled, next_run_at)`,
		`CREATE TABLE IF NOT EXISTS runs (
			seq         INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id     TEXT NOT NULL,
			at          INTEGER NOT NULL,
			kind        TEXT NOT NULL DEFAULT '',
			trigger     TEXT NOT NULL DEFAULT '',
			ok          INTEGER NOT NULL DEFAULT 0,
			exit_code   INTEGER NOT NULL DEFAULT 0,
			ms          INTEGER NOT NULL DEFAULT 0,
			error       TEXT NOT NULL DEFAULT '',
			detail      TEXT NOT NULL DEFAULT '',
			output_tail TEXT NOT NULL DEFAULT '',
			session_id  TEXT NOT NULL DEFAULT '',
			dialog_id   TEXT NOT NULL DEFAULT '',
			turn_id     TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runs_task ON runs(task_id, at DESC, seq DESC)`,
		`INSERT OR IGNORE INTO meta(key, value) VALUES('schema_version', '1')`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(`UPDATE meta SET value = ? WHERE key = 'schema_version'`, fmt.Sprint(schemaVersion))
	return err
}

// ── 旧 JSON 一次性导入 ─────────────────────────

// importLegacyTasksLocked：tasks 表为空且 tasks.json 存在时导入；成功后改名为
// tasks.json.migrated-<unix>（不删除原文件内容）。损坏 → 备份 .corrupt-<ts> 并返回错误。
func (s *Store) importLegacyTasksLocked() error {
	path := filepath.Join(s.dir, legacyTasksName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取 tasks.json 失败: %w", err)
	}
	n, err := countRows(s.db, "tasks")
	if err != nil {
		return err
	}
	if n > 0 { // 已迁移过 / 新库已有数据 → 忽略旧文件
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 并发首次启动时另一进程已导入并改名
		}
		return fmt.Errorf("读取 tasks.json 失败: %w", err)
	}
	var tf tasksFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", path, time.Now().Unix())
		// 原文件保留（不静默删除）；已有备份则不再重复写一份。
		if old, _ := filepath.Glob(path + ".corrupt-*"); len(old) == 0 {
			_ = os.WriteFile(backup, raw, 0o600)
		}
		return fmt.Errorf("tasks.json 解析失败（已备份，原文件保留）: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("导入 tasks.json 失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// 写事务内再次确认：并发首次启动时，另一个进程可能已先导入并改名。
	var n2 int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n2); err != nil {
		return fmt.Errorf("导入 tasks.json 失败: %w", err)
	}
	if n2 > 0 {
		return nil
	}
	for _, t := range tf.Tasks {
		if t == nil {
			continue
		}
		if err := insertTask(tx, *t); err != nil {
			return fmt.Errorf("导入 tasks.json 失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("导入 tasks.json 失败: %w", err)
	}
	_ = os.Rename(path, fmt.Sprintf("%s.migrated-%d", path, time.Now().Unix()))
	return nil
}

// importLegacyRunsLocked：runs 表为空且 runs.jsonl 存在时逐行导入；损坏行跳过
// （旧行为）。成功后改名为 runs.jsonl.migrated-<unix>。
func (s *Store) importLegacyRunsLocked() error {
	path := filepath.Join(s.dir, legacyRunsName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取 runs.jsonl 失败: %w", err)
	}
	n, err := countRows(s.db, "runs")
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 并发首次启动时另一进程已导入并改名
		}
		return fmt.Errorf("读取 runs.jsonl 失败: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	recs := make([]RunRecord, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec RunRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // 损坏行跳过（旧行为）
		}
		recs = append(recs, rec)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("导入 runs.jsonl 失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// 写事务内再次确认（并发首次启动时另一个进程可能已先导入）。
	var n2 int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n2); err != nil {
		return fmt.Errorf("导入 runs.jsonl 失败: %w", err)
	}
	if n2 > 0 {
		return nil
	}
	for _, rec := range recs {
		if err := insertRun(tx, rec); err != nil {
			return fmt.Errorf("导入 runs.jsonl 失败: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM runs WHERE seq NOT IN (SELECT seq FROM runs ORDER BY seq DESC LIMIT ?)`, runsMaxRows); err != nil {
		return fmt.Errorf("导入 runs.jsonl 失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("导入 runs.jsonl 失败: %w", err)
	}
	_ = os.Rename(path, fmt.Sprintf("%s.migrated-%d", path, time.Now().Unix()))
	return nil
}

// countRows 统计表行数（table 仅由本文件内的常量传入）。
func countRows(db *sql.DB, table string) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ── 任务读写 ──────────────────────────────────

// taskColumns 是 tasks 表的完整列序（与 scanTask 一一对应）。
const taskColumns = `id, cron, action_type, action_param, once, run_at, session_mode, description,
	enabled, created_at, created_by_instance, created_by_session, created_by_top_session,
	created_by_work_dir, next_run_at, run_count, last_run, running_since, sandbox,
	disabled_reason, session_id, dialog_id`

// rowScanner 抽象 *sql.Row / *sql.Rows 的 Scan。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTask 按 taskColumns 顺序组装 Task（JSON 列损坏时降级为 nil，不阻断列表）。
func scanTask(sc rowScanner) (Task, error) {
	var (
		t           Task
		actionParam string
		lastRun     string
		once        int
		enabled     int
		sandbox     sql.NullBool
	)
	err := sc.Scan(
		&t.ID, &t.Cron, &t.Action.Type, &actionParam, &once, &t.RunAt, &t.SessionMode,
		&t.Description, &enabled, &t.CreatedAt,
		&t.CreatedBy.InstanceID, &t.CreatedBy.SessionID, &t.CreatedBy.TopSession, &t.CreatedBy.WorkDir,
		&t.NextRunAt, &t.RunCount, &lastRun, &t.RunningSince, &sandbox,
		&t.DisabledReason, &t.SessionID, &t.DialogID,
	)
	if err != nil {
		return Task{}, err
	}
	t.Once = once != 0
	t.Enabled = enabled != 0
	if actionParam != "" {
		t.Action.Param = json.RawMessage(actionParam)
	}
	if lastRun != "" {
		var rec RunRecord
		if json.Unmarshal([]byte(lastRun), &rec) == nil {
			t.LastRun = &rec
		}
	}
	if sandbox.Valid {
		v := sandbox.Bool
		t.Sandbox = &v
	}
	return t, nil
}

// taskJSONFields 把 Task 的可变 JSON 列转为 SQL 参数。
func taskJSONFields(t Task) (actionParam, lastRun string, sandbox any, err error) {
	actionParam = string(t.Action.Param)
	sandbox = nil
	if t.Sandbox != nil {
		if *t.Sandbox {
			sandbox = 1
		} else {
			sandbox = 0
		}
	}
	if t.LastRun != nil {
		b, merr := json.Marshal(t.LastRun)
		if merr != nil {
			return "", "", nil, merr
		}
		lastRun = string(b)
	}
	return actionParam, lastRun, sandbox, nil
}

// insertTask 在事务中插入一条任务（重复 id → 错误）。
func insertTask(tx *sql.Tx, t Task) error {
	actionParam, lastRun, sandbox, err := taskJSONFields(t)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO tasks (
		id, cron, action_type, action_param, once, run_at, session_mode, description,
		enabled, created_at, created_by_instance, created_by_session, created_by_top_session,
		created_by_work_dir, next_run_at, run_count, last_run, running_since, sandbox,
		disabled_reason, session_id, dialog_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Cron, t.Action.Type, actionParam, boolInt(t.Once), t.RunAt, t.SessionMode,
		t.Description, boolInt(t.Enabled), t.CreatedAt,
		t.CreatedBy.InstanceID, t.CreatedBy.SessionID, t.CreatedBy.TopSession, t.CreatedBy.WorkDir,
		t.NextRunAt, t.RunCount, lastRun, t.RunningSince, sandbox,
		t.DisabledReason, t.SessionID, t.DialogID,
	)
	return err
}

// updateTask 在事务中覆盖一条任务的全部列（id 不变）。
func updateTask(tx *sql.Tx, t Task) error {
	actionParam, lastRun, sandbox, err := taskJSONFields(t)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE tasks SET
		cron = ?, action_type = ?, action_param = ?, once = ?, run_at = ?, session_mode = ?,
		description = ?, enabled = ?, created_at = ?, created_by_instance = ?,
		created_by_session = ?, created_by_top_session = ?, created_by_work_dir = ?,
		next_run_at = ?, run_count = ?, last_run = ?, running_since = ?, sandbox = ?,
		disabled_reason = ?, session_id = ?, dialog_id = ?
	WHERE id = ?`,
		t.Cron, t.Action.Type, actionParam, boolInt(t.Once), t.RunAt, t.SessionMode,
		t.Description, boolInt(t.Enabled), t.CreatedAt,
		t.CreatedBy.InstanceID, t.CreatedBy.SessionID, t.CreatedBy.TopSession, t.CreatedBy.WorkDir,
		t.NextRunAt, t.RunCount, lastRun, t.RunningSince, sandbox,
		t.DisabledReason, t.SessionID, t.DialogID, t.ID,
	)
	return err
}

// boolInt 把 bool 转 SQLite INTEGER。
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// getTaskTx 在事务中按 id 取任务（无行 → sql.ErrNoRows）。
func getTaskTx(tx *sql.Tx, id string) (Task, error) {
	return scanTask(tx.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
}

// List 返回任务快照（按创建时间、id 升序）。
func (s *Store) List() []Task {
	db, err := s.conn()
	if err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT ` + taskColumns + ` FROM tasks ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// Get 按 id 取任务快照。
func (s *Store) Get(id string) (Task, bool) {
	db, err := s.conn()
	if err != nil {
		return Task{}, false
	}
	t, err := scanTask(db.QueryRow(`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return Task{}, false
	}
	return t, true
}

// Add 新增任务（id 重复 → 错误）。
func (s *Store) Add(t Task) error { return s.AddLimited(t, 0) }

// AddLimited 在同一个写事务内检查任务总数上限并新增（max <= 0 = 不限）。
// 上限判定与插入同事务，避免并发 add 越过 max-tasks；失败回滚。
func (s *Store) AddLimited(t Task, max int) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	err = tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, t.ID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists > 0 {
		return fmt.Errorf("任务 id 重复: %s", t.ID)
	}
	if max > 0 {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return fmt.Errorf("任务数已达上限（%d）", max)
		}
	}
	if err := insertTask(tx, t); err != nil {
		return err
	}
	return tx.Commit()
}

// Update 在单个写事务内修改任务（SELECT → mutate → UPDATE）。
//
// mutate 返回错误 → 整个事务回滚，任务保持磁盘上的原值（不再有「失败但内存已改」的语义）。
// 事务以 BEGIN IMMEDIATE 开始：多进程并发认领同一任务时，只有一个事务能先写入，
// 后到者拿到写锁后读到已提交的新值（典型用法：closure 只在 RunningSince==0 时 claim）。
func (s *Store) Update(id string, mutate func(*Task) error) error {
	_, err := s.updateTx(id, mutate)
	return err
}

// UpdateTx 与 Update 相同，但返回更新后的任务快照。
func (s *Store) UpdateTx(id string, mutate func(*Task) error) (Task, error) {
	return s.updateTx(id, mutate)
}

// updateTx 在写事务内应用 mutate 并提交；任何失败都回滚。
func (s *Store) updateTx(id string, mutate func(*Task) error) (Task, error) {
	db, err := s.conn()
	if err != nil {
		return Task{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := getTaskTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	if err := mutate(&t); err != nil {
		return Task{}, err
	}
	if err := updateTask(tx, t); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Delete 在事务内删除任务，返回被删快照。
func (s *Store) Delete(id string) (Task, error) {
	db, err := s.conn()
	if err != nil {
		return Task{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return Task{}, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := getTaskTx(tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE id = ?`, id); err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return t, nil
}

// ── 运行明细 ──────────────────────────────────

// runColumns 是 runs 表的列序（scanRun 使用）。
const runColumns = `task_id, at, kind, trigger, ok, exit_code, ms, error, detail,
	output_tail, session_id, dialog_id, turn_id`

// insertRun 在事务中插入一条运行明细。
func insertRun(tx *sql.Tx, rec RunRecord) error {
	_, err := tx.Exec(`INSERT INTO runs (
		task_id, at, kind, trigger, ok, exit_code, ms, error, detail,
		output_tail, session_id, dialog_id, turn_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.TaskID, rec.At, rec.Kind, rec.Trigger, boolInt(rec.OK), rec.ExitCode, rec.MS,
		rec.Error, rec.Detail, rec.OutputTail, rec.SessionID, rec.DialogID, rec.TurnID,
	)
	return err
}

// scanRun 按 runColumns 顺序组装 RunRecord。
func scanRun(sc rowScanner) (RunRecord, error) {
	var (
		rec RunRecord
		ok  int
	)
	err := sc.Scan(&rec.TaskID, &rec.At, &rec.Kind, &rec.Trigger, &ok, &rec.ExitCode,
		&rec.MS, &rec.Error, &rec.Detail, &rec.OutputTail, &rec.SessionID,
		&rec.DialogID, &rec.TurnID)
	if err != nil {
		return RunRecord{}, err
	}
	rec.OK = ok != 0
	return rec, nil
}

// AppendRun 追加一条运行明细（失败只返回错误，不阻断调度）；rows 超过上限时裁剪最旧记录。
func (s *Store) AppendRun(rec RunRecord) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertRun(tx, rec); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM runs WHERE seq NOT IN (SELECT seq FROM runs ORDER BY seq DESC LIMIT ?)`, runsMaxRows); err != nil {
		return err
	}
	return tx.Commit()
}

// RecentRuns 返回最近 limit 条运行明细（新 → 旧）。
func (s *Store) RecentRuns(limit int) []RunRecord {
	return s.RecentRunsFor("", limit)
}

// RecentRunsFor 返回最近 limit 条运行明细（新 → 旧；taskID 空 = 全部）。
func (s *Store) RecentRunsFor(taskID string, limit int) []RunRecord {
	if limit <= 0 {
		limit = 50
	}
	db, err := s.conn()
	if err != nil {
		return nil
	}
	var (
		rows *sql.Rows
	)
	if taskID == "" {
		rows, err = db.Query(`SELECT `+runColumns+` FROM runs ORDER BY at DESC, seq DESC LIMIT ?`, limit)
	} else {
		rows, err = db.Query(`SELECT `+runColumns+` FROM runs WHERE task_id = ? ORDER BY at DESC, seq DESC LIMIT ?`, taskID, limit)
	}
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]RunRecord, 0, limit)
	for rows.Next() {
		rec, err := scanRun(rows)
		if err != nil {
			return nil
		}
		out = append(out, rec)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}
