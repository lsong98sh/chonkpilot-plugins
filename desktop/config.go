// config.go —— desktop 插件配置面（PLUGIN-UI-1 第三段）。
//
// 唯一链路（28-plugins §3A.5「单源不双写」）：
//
//	插件页 webui/config.html → POST /plugins/desktop/api/{load,save,reset}
//	  → 宿主动作面（plugin.ui.rpc）→ 本文件（校验 + 默认兜底）
//	  → 宿主能力面 plugin/v1/config.user.{load,set,delete} → 数据层 usr 配置（`plugins.desktop.*`）
//
// 存储说明（2026-10-24「插件配置收回 data 层」）：配置落 **data 层 usr 配置库**的
// `plugins.desktop.*` 键（写哪一级由本插件配置页语义决定 —— 本插件是用户级，故落 usr）。
// 上一版写在插件私有文件 `<CHONKPILOT_PLUGIN_DATA_DIR>/webui-config.json`：启动时**只读一次**
// 做迁移（migratePrivateFileLocked），成功后删除该文件（单源）。
// 旧宿主配置键只允许在 migrateLocked 中一次性读取。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// ─── 键命名空间（唯一源）────────────────────────────────────────────
const (
	KeyScreenshotDir = "plugins.desktop.screenshot-dir"
)

// allKeys 是本插件的用户配置键全集（稳定序；load 恒回显这些键的有效值）。
var allKeys = []string{KeyScreenshotDir}

// legacyKeyMap 旧宿主键 → 新命名空间键（**迁移专用**；除 migrateLocked 外不得读旧键）。
var legacyKeyMap = []struct{ Old, New string }{
	{"screenshotDir", KeyScreenshotDir},
}

// configFields 是配置页字段声明（页面据 type/min/max/secret 动态渲染）。
var configFields = []fieldSpec{
	{Key: KeyScreenshotDir, Type: "string", Label: "截图默认目录（空 = 引擎默认）"},
}

// fieldSpec 是配置页字段（JSON 形状与旧 manifest config.fields 兼容）。
type fieldSpec struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	Label  string `json:"label"`
	Min    *int   `json:"min,omitempty"`
	Max    *int   `json:"max,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

// ─── 配置模型 ────────────────────────────────────────────────────

// settings 是本插件配置的内存视图（落盘形态 = allKeys 的字符串 KV）。
type settings struct {
	ScreenshotDir string
}

// defaultSettings 缺键兜底（与实际生效值一致）。
func defaultSettings() settings { return settings{} }

// settingsOf 由「已存在的 KV 键」构造配置快照（缺失按默认）。
func settingsOf(values map[string]string) settings {
	return settings{ScreenshotDir: values[KeyScreenshotDir]}
}

// values 把快照展开成 `键 → 字符串` 的有效值（load 回显用）。
func (s settings) values() map[string]string {
	return map[string]string{KeyScreenshotDir: s.ScreenshotDir}
}

// ─── 存储（data 层 usr 配置；可替换：测试 = 内存桩）───────────────────

// legacyConfigFileName 是上一版插件私有配置文件名（**只用于迁移读取**，迁移成功后删除）。
const legacyConfigFileName = "webui-config.json"

// configStore 是本插件对配置存储的最小依赖。
type configStore interface {
	Load(ctx context.Context) (map[string]string, error)
	Save(ctx context.Context, values map[string]string) error
	Delete(ctx context.Context) error
}

// hostConfigStore 经宿主能力面读写 data 层 usr 配置（键前缀由宿主按本插件身份强制）。
type hostConfigStore struct{ p *pluginsdk.Plugin }

func newHostConfigStore(p *pluginsdk.Plugin) *hostConfigStore { return &hostConfigStore{p: p} }

// Load 读回本插件已落库的键（宿主只回 `plugins.<本插件>.` 前缀，不含别家）。
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

// Save 增量写（逐键落库；空 map = 无操作）。
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

// Delete 删本插件全部已知键（恢复默认；删不存在的键宿主视为成功）。
func (s *hostConfigStore) Delete(ctx context.Context) error {
	for _, key := range allKeys {
		if _, err := s.p.Call(ctx, "plugin/v1/config.user.delete", map[string]any{"id": key}); err != nil {
			return err
		}
	}
	return nil
}

// legacyConfigFilePath 返回上一版私有配置文件的路径（迁移专用）。
func legacyConfigFilePath() string {
	dir := strings.TrimSpace(os.Getenv(pluginsdk.EnvDataDir))
	if dir == "" {
		dir = pluginsdk.DefaultDataDir()
	}
	return filepath.Join(dir, legacyConfigFileName)
}

// ─── 配置管理器 ───────────────────────────────────────────────────

// configManager 承载配置动作、一次性迁移与生效快照。
type configManager struct {
	p      *pluginsdk.Plugin
	store  configStore
	logf   func(format string, args ...any)
	warnf  func(format string, args ...any) // nil = 回落 logf
	legacy func(key string) (any, bool)     // 测试注入；nil = 读旧宿主配置快照
	// legacyFile 是上一版私有配置文件路径（迁移专用；测试可注入，空 = 不迁移）。
	legacyFile string

	mu     sync.RWMutex
	values map[string]string // 已确认落盘的键值（缺键 = 默认）
	loaded bool
}

// newConfigManager 构造配置管理器（生产实现）。
func newConfigManager(p *pluginsdk.Plugin) *configManager {
	return &configManager{
		p:          p,
		store:      newHostConfigStore(p),
		legacyFile: legacyConfigFilePath(),
		logf: func(format string, args ...any) {
			p.Logger().Info(fmt.Sprintf(format, args...))
		},
		warnf: func(format string, args ...any) {
			p.Logger().Warn(fmt.Sprintf(format, args...))
		},
	}
}

// warn 记一条告警（未注入 warnf → 回落 logf）。
func (m *configManager) warn(format string, args ...any) {
	if m.warnf != nil {
		m.warnf(format, args...)
		return
	}
	m.logf(format, args...)
}

// Actions 返回本插件承担 UI 动作名（宿主 `POST /plugins/desktop/api/<action>`）。
func (m *configManager) Actions() map[string]pluginsdk.RequestFunc {
	return map[string]pluginsdk.RequestFunc{
		"load":  m.handleLoad,
		"save":  m.handleSave,
		"reset": m.handleReset,
	}
}

// Init 在启动时读取存储并执行一次性旧键迁移（失败仅记录，运行时按默认兜底）。
func (m *configManager) Init(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked(ctx)
}

func (m *configManager) loadLocked(ctx context.Context) error {
	values, err := m.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	}
	m.values = values
	m.migratePrivateFileLocked(ctx) // 上一版私有文件 → data 层（幂等；成功后删文件）
	if m.migrateLocked() {
		if err := m.store.Save(ctx, m.values); err != nil {
			m.warn("配置迁移写盘失败（下次启动重试）：%v", err)
		}
	}
	m.loaded = true
	return nil
}

// migratePrivateFileLocked 把上一版插件私有配置文件一次性迁入 data 层：
//   - 文件不存在 → 无操作；读取 / 解析失败 → 保留文件，下次启动重试（记 Warn）；
//   - data 层已有同名键 → **以 data 层为准，不覆盖**（幂等：重复执行不重复写）；
//   - 写入成功 → 删除文件（单源）；写入失败 → 保留文件，下次启动重试（记 Warn）。
func (m *configManager) migratePrivateFileLocked(ctx context.Context) {
	path := m.legacyFile
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			m.warn("旧配置文件读取失败（保留，下次启动重试）：%v", err)
		}
		return
	}
	var fileValues map[string]string
	if err := json.Unmarshal(raw, &fileValues); err != nil {
		m.warn("旧配置文件 %s 解析失败（保留，下次启动重试）：%v", path, err)
		return
	}
	changed := false
	for k, v := range fileValues {
		if !isKnownKey(k) {
			continue
		}
		if _, ok := m.values[k]; ok {
			continue // data 层已有值 → 以 data 层为准
		}
		m.values[k] = v
		changed = true
	}
	if changed {
		if err := m.store.Save(ctx, m.values); err != nil {
			m.warn("旧配置迁入 data 层失败（保留文件，下次启动重试）：%v", err)
			return
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.warn("旧配置文件删除失败（下次启动重试）：%v", err)
		return
	}
	m.logf("配置已迁入 data 层 usr（旧私有文件已删除）：%s", path)
}

func (m *configManager) ensureLoaded(ctx context.Context) error {
	m.mu.RLock()
	ok := m.loaded
	m.mu.RUnlock()
	if ok {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded {
		return nil
	}
	return m.loadLocked(ctx)
}

// Settings 返回当前生效配置快照（工具执行读此值，不再读旧宿主配置）。
func (m *configManager) Settings() settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return settingsOf(m.values)
}

// ─── 动作：load ──────────────────────────────────────────────────

func (m *configManager) handleLoad(ctx context.Context, _ map[string]any) (any, error) {
	if err := m.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	m.mu.RLock()
	values := settingsOf(m.values).values()
	explicit := explicitKeys(m.values)
	m.mu.RUnlock()
	return map[string]any{"ok": true, "fields": configFields, "values": values, "explicit": explicit}, nil
}

// ─── 动作：save ──────────────────────────────────────────────────

// handleSave 校验 → 写 data 层 usr 配置 → 回读（校验失败不写任何键）。
func (m *configManager) handleSave(ctx context.Context, payload map[string]any) (any, error) {
	entries, err := saveEntries(payload)
	if err != nil {
		return nil, err
	}
	if err := m.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	norm := make(map[string]string, len(entries))
	keys := make([]string, 0, len(entries))
	for k, v := range entries {
		if !isKnownKey(k) {
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s（只接受 plugins.desktop.* 命名空间）", k)
		}
		val, err := normalizeValue(k, v)
		if err != nil {
			return nil, err
		}
		norm[k] = val
		keys = append(keys, k)
	}
	sort.Strings(keys)

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
	m.logf("desktop: 配置已保存（keys=%s）", strings.Join(keys, ","))
	return map[string]any{"ok": true, "keys": keys, "values": values}, nil
}

// ─── 动作：reset ─────────────────────────────────────────────────

func (m *configManager) handleReset(ctx context.Context, _ map[string]any) (any, error) {
	if err := m.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Delete(ctx); err != nil {
		return nil, fmt.Errorf("恢复默认失败：%w", err)
	}
	m.values = map[string]string{}
	return map[string]any{"ok": true, "values": settingsOf(nil).values()}, nil
}

// ─── 校验 ────────────────────────────────────────────────────────

func isKnownKey(key string) bool {
	for _, k := range allKeys {
		if k == key {
			return true
		}
	}
	return false
}

// normalizeValue 校验并归一单个配置值（非法 → 明确错误，不静默截断）。
func normalizeValue(key string, raw any) (string, error) {
	switch key {
	case KeyScreenshotDir:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if len(s) > 1024 {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 1024 字符，收到 %d）", key, len(s))
		}
		if strings.ContainsRune(s, 0) {
			return "", fmt.Errorf("config_invalid: %s 含非法字符 NUL", key)
		}
		return s, nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s", key)
}

// saveEntries 取动作载荷里的 `entries`（对象形态；缺省 → 明确错误）。
func saveEntries(payload map[string]any) (map[string]any, error) {
	raw, ok := payload["entries"]
	if !ok || raw == nil {
		return nil, errors.New("config_invalid: 缺少 entries")
	}
	entries, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("config_invalid: entries 必须是对象（键 → 值）")
	}
	if len(entries) == 0 {
		return nil, errors.New("config_invalid: entries 不能为空")
	}
	return entries, nil
}

// explicitKeys 返回已显式落盘的键（稳定序）。
func explicitKeys(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for _, k := range allKeys {
		if _, ok := values[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// ─── 一次性迁移（旧宿主键 → plugins.desktop.*）────────────────────

// migrateLocked 把旧宿主键迁入命名空间：
//   - 新键已存在 → 忽略旧键（单源）；
//   - 旧键存在且新键缺失、且与内置默认不同 → 写新键；
//   - 旧键无法删除（无宿主写能力）→ 保留，但此后没有任何读取路径。
//
// 幂等：重复执行不会重复写（新键写入后即跳过）。
func (m *configManager) migrateLocked() bool {
	defaults := defaultSettings().values()
	changed := false
	for _, lm := range legacyKeyMap {
		if _, ok := m.values[lm.New]; ok {
			continue
		}
		raw, ok := m.legacyValue(lm.Old)
		if !ok || raw == nil {
			continue
		}
		val, err := normalizeValue(lm.New, raw)
		if err != nil {
			m.logf("配置迁移跳过非法旧值 %s：%v", lm.Old, err)
			continue
		}
		if val == defaults[lm.New] {
			continue
		}
		m.values[lm.New] = val
		changed = true
		m.logf("配置迁移 %s → %s", lm.Old, lm.New)
	}
	return changed
}

func (m *configManager) legacyValue(key string) (any, bool) {
	if m.legacy != nil {
		return m.legacy(key)
	}
	if m.p == nil || m.p.Config() == nil {
		return nil, false
	}
	v := m.p.Config().Value(key)
	return v, v != nil
}
