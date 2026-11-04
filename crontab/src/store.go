package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// tasksFileVersion 是 tasks.json 的格式版本（只增，不做隐式迁移）。
const tasksFileVersion = 1

// runsFileMaxBytes 是 runs.jsonl 单文件上限（超出滚动为 .1 / .2 / .3）。
const runsFileMaxBytes = 1 << 20

// CreatedBy 记录任务创建来源（仅作 run 的默认 cwd / 审计，不做项目隔离）。
type CreatedBy struct {
	InstanceID string `json:"instance_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	WorkDir    string `json:"work_dir,omitempty"`
}

// Action 是任务的二选一动作（type 只有 run / notify）；Param 保留规范化后的 JSON。
type Action struct {
	Type  string          `json:"type"`
	Param json.RawMessage `json:"param"`
}

// Task 是一条定时任务（全局，不区分项目）。
type Task struct {
	ID           string     `json:"id"`
	Cron         string     `json:"cron"`
	Action       Action     `json:"action"`
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
	// DisabledReason 是「执行前检查导致停用」的原因（当前唯一取值 = work_dir_missing）；
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
)

// RunRecord 是一条运行明细（runs.jsonl / Task.LastRun）。
type RunRecord struct {
	TaskID     string `json:"task_id"`
	At         int64  `json:"at"`
	Kind       string `json:"kind"` // run | notify | skipped
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

// tasksFile 是 tasks.json 的落盘形态。
type tasksFile struct {
	Version int     `json:"version"`
	Tasks   []*Task `json:"tasks"`
}

// Store 是 tasks.json + runs.jsonl 的持久化（并发安全；写盘原子替换）。
type Store struct {
	dir  string
	path string
	runs string

	mu   sync.Mutex
	data tasksFile
}

// newStore 构造存储（不读盘；调用方随后 Load）。
func newStore(dir string) *Store {
	return &Store{
		dir:  dir,
		path: filepath.Join(dir, "tasks.json"),
		runs: filepath.Join(dir, "runs.jsonl"),
		data: tasksFile{Version: tasksFileVersion},
	}
}

// Load 读取 tasks.json：缺失 → 空；损坏 → 备份为 tasks.json.corrupt-<ts> 后按空启动（不静默覆盖）。
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("创建插件数据目录失败: %w", err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.data = tasksFile{Version: tasksFileVersion}
			return nil
		}
		return fmt.Errorf("读取 tasks.json 失败: %w", err)
	}
	var tf tasksFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		backup := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().Unix())
		_ = os.WriteFile(backup, raw, 0o600)
		s.data = tasksFile{Version: tasksFileVersion}
		return fmt.Errorf("tasks.json 解析失败（已备份到 %s）: %w", backup, err)
	}
	if tf.Version <= 0 {
		tf.Version = tasksFileVersion
	}
	if tf.Tasks == nil {
		tf.Tasks = []*Task{}
	}
	s.data = tf
	return nil
}

// List 返回任务快照（按创建时间升序）。
func (s *Store) List() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.data.Tasks))
	for _, t := range s.data.Tasks {
		out = append(out, *t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get 按 id 取任务快照。
func (s *Store) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findLocked(id)
	if t == nil {
		return Task{}, false
	}
	return *t, true
}

// Add 新增任务并落盘（id 重复 → 错误）。
func (s *Store) Add(t Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findLocked(t.ID) != nil {
		return fmt.Errorf("任务 id 重复: %s", t.ID)
	}
	cp := t
	s.data.Tasks = append(s.data.Tasks, &cp)
	return s.persistLocked()
}

// ErrTaskNotFound 表示任务不存在。
var ErrTaskNotFound = errors.New("任务不存在")

// Update 在锁内修改任务；mutate 返回错误 → 不落盘（修改仍保留在内存，用于跳过标记场景）。
func (s *Store) Update(id string, mutate func(*Task) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.findLocked(id)
	if t == nil {
		return ErrTaskNotFound
	}
	if err := mutate(t); err != nil {
		return err
	}
	return s.persistLocked()
}

// Delete 删除任务并落盘。
func (s *Store) Delete(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.Tasks {
		if t.ID != id {
			continue
		}
		cp := *t
		s.data.Tasks = append(s.data.Tasks[:i], s.data.Tasks[i+1:]...)
		if err := s.persistLocked(); err != nil {
			return Task{}, err
		}
		return cp, nil
	}
	return Task{}, ErrTaskNotFound
}

// findLocked 按 id 查任务指针（调用方持锁）。
func (s *Store) findLocked(id string) *Task {
	for _, t := range s.data.Tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// persistLocked 原子写 tasks.json（同目录 tmp + fsync + rename，0600）。
func (s *Store) persistLocked() error {
	s.data.Version = tasksFileVersion
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// AppendRun 追加一条运行明细（1MB 滚动 × 3；失败只返回错误，不阻断调度）。
func (s *Store) AppendRun(rec RunRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.rotateRunsLocked(); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.runs, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// rotateRunsLocked 超过上限 → runs.jsonl.1/.2/.3 滚动（调用方持锁）。
func (s *Store) rotateRunsLocked() error {
	fi, err := os.Stat(s.runs)
	if err != nil || fi.Size() < runsFileMaxBytes {
		return nil
	}
	for i := 3; i >= 1; i-- {
		from := s.runs
		if i > 1 {
			from = fmt.Sprintf("%s.%d", s.runs, i-1)
		}
		to := fmt.Sprintf("%s.%d", s.runs, i)
		_ = os.Remove(to)
		if _, err := os.Stat(from); err == nil {
			_ = os.Rename(from, to)
		}
	}
	return nil
}

// RecentRuns 返回最近 limit 条运行明细（新 → 旧；损坏行跳过）。
func (s *Store) RecentRuns(limit int) []RunRecord {
	return s.RecentRunsFor("", limit)
}

// RecentRunsFor 返回最近 limit 条运行明细（新 → 旧）；taskID 非空时只取该任务的记录。
func (s *Store) RecentRunsFor(taskID string, limit int) []RunRecord {
	if limit <= 0 {
		limit = 50
	}
	raw, err := os.ReadFile(s.runs)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	out := make([]RunRecord, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var rec RunRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if taskID != "" && rec.TaskID != taskID {
			continue
		}
		out = append(out, rec)
	}
	return out
}
