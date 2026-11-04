package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/configkit"
	"github.com/lsong98sh/chonkpilot-util/strlist"
)

// 配置键（唯一源；落 data 层 usr 配置）。
const (
	KeyDefaultTimeoutSec = "plugins.crontab.default-timeout-sec"
	KeyMissedGraceSec    = "plugins.crontab.missed-grace-sec"
	KeyMaxTasks          = "plugins.crontab.max-tasks"
	KeyAllowedWorkDirs   = "plugins.crontab.allowed-work-dirs"
	KeyCreateToast       = "plugins.crontab.create-toast"
	KeyNotifyFallback    = "plugins.crontab.notify-fallback"
	KeySandbox           = "plugins.crontab.sandbox"
	// KeyShowInSessionList = 定时任务顶层对话是否进入用户会话列表（G-99 / 方案 §5.5.4）。
	// 消费方 = data 层 `sessionstore.ListVisibleDialogs`（data-session-list）；缺省 false。
	KeyShowInSessionList = "plugins.crontab.show-in-session-list"
)

// 校验区间。
const (
	timeoutMin     = 1
	timeoutMax     = 86400
	graceMin       = 0
	graceMax       = 86400
	maxTasksMin    = 1
	maxTasksMax    = 2000
	maxRulesLen    = 4096
	fallbackLog    = "log"
	fallbackWindow = "window"
)

var allKeys = []string{
	KeyDefaultTimeoutSec, KeyMissedGraceSec, KeyMaxTasks,
	KeyAllowedWorkDirs, KeyCreateToast, KeyNotifyFallback, KeySandbox,
	KeyShowInSessionList,
}

// settings 是插件配置的内存视图。
type settings struct {
	DefaultTimeoutSec int
	MissedGraceSec    int
	MaxTasks          int
	AllowedWorkDirs   []string
	CreateToast       bool
	NotifyFallback    string
	// Sandbox 是**任务级沙箱的缺省值**（SANDBOX-WRITE-1 P2）：任务未显式设置 sandbox 时用它。
	// false = 默认不隔离（与 P1 前行为一致）；true = 默认以 Low IL + Job 执行。
	Sandbox bool
	// ShowInSessionList = 定时任务顶层对话是否进入用户会话列表（G-99）。缺省 false。
	ShowInSessionList bool
}

// defaultSettings 缺键兜底。
func defaultSettings() settings {
	return settings{
		DefaultTimeoutSec: 600,
		MissedGraceSec:    300,
		MaxTasks:          200,
		CreateToast:       true,
		NotifyFallback:    fallbackLog,
	}
}

// settingsOf 由已落库 KV 构造配置快照（缺失 / 越界按默认）。
func settingsOf(values map[string]string) settings {
	s := defaultSettings()
	if n, err := strconv.Atoi(strings.TrimSpace(values[KeyDefaultTimeoutSec])); err == nil && n >= timeoutMin && n <= timeoutMax {
		s.DefaultTimeoutSec = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(values[KeyMissedGraceSec])); err == nil && n >= graceMin && n <= graceMax {
		s.MissedGraceSec = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(values[KeyMaxTasks])); err == nil && n >= maxTasksMin && n <= maxTasksMax {
		s.MaxTasks = n
	}
	s.AllowedWorkDirs = parseDirs(values[KeyAllowedWorkDirs])
	if v, ok := values[KeyCreateToast]; ok {
		s.CreateToast = strings.EqualFold(strings.TrimSpace(v), "true")
	}
	if v := strings.TrimSpace(values[KeyNotifyFallback]); v == fallbackWindow {
		s.NotifyFallback = fallbackWindow
	}
	if v, ok := values[KeySandbox]; ok {
		s.Sandbox = strings.EqualFold(strings.TrimSpace(v), "true")
	}
	if v, ok := values[KeyShowInSessionList]; ok {
		s.ShowInSessionList = strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return s
}

// values 展开为 `键 → 字符串`（load 回显）。
func (s settings) values() map[string]string {
	return map[string]string{
		KeyDefaultTimeoutSec: strconv.Itoa(s.DefaultTimeoutSec),
		KeyMissedGraceSec:    strconv.Itoa(s.MissedGraceSec),
		KeyMaxTasks:          strconv.Itoa(s.MaxTasks),
		KeyAllowedWorkDirs:   strings.Join(s.AllowedWorkDirs, ","),
		KeyCreateToast:       boolString(s.CreateToast),
		KeyNotifyFallback:    s.NotifyFallback,
		KeySandbox:           boolString(s.Sandbox),
		KeyShowInSessionList: boolString(s.ShowInSessionList),
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// parseDirs 解析目录白名单（逗号/分号/换行分隔；只保留绝对路径，Clean 归一）。
func parseDirs(raw string) []string {
	out := make([]string, 0, 4)
	for _, p := range strlist.Split(raw) {
		if filepath.IsAbs(p) {
			out = append(out, filepath.Clean(p))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fieldSpec 是配置页字段声明。
type fieldSpec struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Label string `json:"label"`
	Min   *int   `json:"min,omitempty"`
	Max   *int   `json:"max,omitempty"`
}

var configFields = []fieldSpec{
	{Key: KeyDefaultTimeoutSec, Type: "integer", Label: "run 任务默认超时（秒）", Min: configkit.IntPtr(timeoutMin), Max: configkit.IntPtr(timeoutMax)},
	{Key: KeyMissedGraceSec, Type: "integer", Label: "错过补偿窗口（秒；0 = 不补跑）", Min: configkit.IntPtr(graceMin), Max: configkit.IntPtr(graceMax)},
	{Key: KeyMaxTasks, Type: "integer", Label: "任务总数上限", Min: configkit.IntPtr(maxTasksMin), Max: configkit.IntPtr(maxTasksMax)},
	{Key: KeyAllowedWorkDirs, Type: "string", Label: "允许创建任务的目录白名单（逗号分隔的绝对路径；空 = 不限）"},
	{Key: KeyCreateToast, Type: "boolean", Label: "创建任务后弹系统通知"},
	{Key: KeyNotifyFallback, Type: "string", Label: "系统通知失败兜底（log | window）"},
	{Key: KeySandbox, Type: "boolean", Label: "任务缺省启用进程沙箱（仅限写、任务级可覆盖）"},
	{Key: KeyShowInSessionList, Type: "boolean", Label: "定时任务对话显示在用户会话列表（需重新打开会话列表生效）"},
}

// configStore 是配置存储的最小依赖（可替换：测试 = 内存桩）。
type configStore interface {
	Load(ctx context.Context) (map[string]string, error)
	Save(ctx context.Context, values map[string]string) error
	Delete(ctx context.Context) error
}

// hostConfigStore 经宿主能力面读写 data 层 usr 配置。
type hostConfigStore struct{ p *pluginsdk.Plugin }

func (s *hostConfigStore) Load(ctx context.Context) (map[string]string, error) {
	var res struct {
		Data map[string]string `json:"data"`
	}
	if err := s.p.CallInto(ctx, "plugin/v1/config.user.load", map[string]any{}, &res); err != nil {
		return nil, err
	}
	if res.Data == nil {
		return map[string]string{}, nil
	}
	return res.Data, nil
}

func (s *hostConfigStore) Save(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	entries := make(map[string]any, len(values))
	for k, v := range values {
		entries[k] = v
	}
	_, err := s.p.Call(ctx, "plugin/v1/config.user.set", map[string]any{"entries": entries})
	return err
}

func (s *hostConfigStore) Delete(ctx context.Context) error {
	for _, key := range allKeys {
		if _, err := s.p.Call(ctx, "plugin/v1/config.user.delete", map[string]any{"id": key}); err != nil {
			return err
		}
	}
	return nil
}

// configManager 承载配置动作与生效快照。
type configManager struct {
	p      *pluginsdk.Plugin
	store  configStore
	logf   func(format string, args ...any)
	mu     sync.RWMutex
	values map[string]string
	loaded bool
}

// newConfigManager 构造配置管理器（生产实现）。
func newConfigManager(p *pluginsdk.Plugin) *configManager {
	return &configManager{
		p:     p,
		store: &hostConfigStore{p: p},
		logf: func(format string, args ...any) {
			p.Logger().Info(fmt.Sprintf(format, args...))
		},
	}
}

// Init 启动时读取配置（失败仅记录，运行时按默认兜底）。
func (m *configManager) Init(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	values, err := m.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	}
	m.values = values
	m.loaded = true
	return nil
}

// Settings 返回当前生效配置快照。
func (m *configManager) Settings() settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return settingsOf(m.values)
}

// Actions 返回 UI 动作（宿主 `POST /plugins/crontab/api/<action>`）。
func (m *configManager) Actions() map[string]pluginsdk.RequestFunc {
	return map[string]pluginsdk.RequestFunc{
		"load":  m.handleLoad,
		"save":  m.handleSave,
		"reset": m.handleReset,
	}
}

func (m *configManager) handleLoad(_ context.Context, _ map[string]any) (any, error) {
	m.mu.RLock()
	values := settingsOf(m.values).values()
	explicit := configkit.ExplicitKeysInOrder(m.values, allKeys)
	m.mu.RUnlock()
	return map[string]any{"ok": true, "fields": configFields, "values": values, "explicit": explicit}, nil
}

func (m *configManager) handleSave(ctx context.Context, payload map[string]any) (any, error) {
	entries, err := configkit.SaveEntries(payload)
	if err != nil {
		return nil, err
	}
	norm := make(map[string]string, len(entries))
	keys := make([]string, 0, len(entries))
	for k, v := range entries {
		if !isKnownKey(k) {
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s（只接受 plugins.crontab.*）", k)
		}
		val, err := normalizeValue(k, v)
		if err != nil {
			return nil, err
		}
		norm[k] = val
		keys = append(keys, k)
	}
	m.mu.Lock()
	merged := make(map[string]string, len(m.values)+len(norm))
	for k, v := range m.values {
		merged[k] = v
	}
	for k, v := range norm {
		merged[k] = v
	}
	if err := m.store.Save(ctx, merged); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("配置写入失败：%w", err)
	}
	m.values = merged
	values := settingsOf(m.values).values()
	m.mu.Unlock()
	m.logf("crontab: 配置已保存（keys=%s）", strings.Join(keys, ","))
	return map[string]any{"ok": true, "keys": keys, "values": values}, nil
}

func (m *configManager) handleReset(ctx context.Context, _ map[string]any) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Delete(ctx); err != nil {
		return nil, fmt.Errorf("恢复默认失败：%w", err)
	}
	m.values = map[string]string{}
	return map[string]any{"ok": true, "values": settingsOf(nil).values()}, nil
}

func isKnownKey(key string) bool {
	for _, k := range allKeys {
		if k == key {
			return true
		}
	}
	return false
}

// normalizeValue 校验并归一单个配置值（非法 → 明确错误）。
func normalizeValue(key string, raw any) (string, error) {
	switch key {
	case KeyDefaultTimeoutSec:
		return configkit.NormalizeInt(key, raw, timeoutMin, timeoutMax)
	case KeyMissedGraceSec:
		return configkit.NormalizeInt(key, raw, graceMin, graceMax)
	case KeyMaxTasks:
		return configkit.NormalizeInt(key, raw, maxTasksMin, maxTasksMax)
	case KeyCreateToast:
		return configkit.NormalizeBool(key, raw)
	case KeySandbox:
		return configkit.NormalizeBool(key, raw)
	case KeyShowInSessionList:
		return configkit.NormalizeBool(key, raw)
	case KeyNotifyFallback:
		s := strings.ToLower(strings.TrimSpace(pluginsdk.Str(raw)))
		if s == "" {
			return fallbackLog, nil
		}
		if s != fallbackLog && s != fallbackWindow {
			return "", fmt.Errorf("config_invalid: %s 只接受 log / window（收到 %q）", key, s)
		}
		return s, nil
	case KeyAllowedWorkDirs:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if len(s) > maxRulesLen {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 %d 字符）", key, maxRulesLen)
		}
		var dirs []string
		for _, p := range strlist.Split(s) {
			if !filepath.IsAbs(p) {
				return "", fmt.Errorf("config_invalid: %s 只接受绝对路径（收到 %q）", key, p)
			}
			dirs = append(dirs, filepath.Clean(p))
		}
		return strings.Join(dirs, ","), nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s", key)
}

// dataDir 返回插件数据目录（宿主注入）。
func dataDir() string {
	if v := strings.TrimSpace(os.Getenv(pluginsdk.EnvDataDir)); v != "" {
		return v
	}
	return pluginsdk.DefaultDataDir()
}
