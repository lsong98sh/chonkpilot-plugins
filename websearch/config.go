// config.go —— websearch 插件配置面（PLUGIN-UI-1 第三段）。
//
// 唯一链路（28-plugins §3A.5「单源不双写」）：
//
//	插件页 webui/config.html → POST /plugins/websearch/api/{load,save,reset}
//	  → 宿主动作面（plugin.ui.rpc）→ 本文件（校验 + 默认兜底）
//	  → 宿主能力面 plugin/v1/config.user.{load,set,delete} → 数据层 usr 配置（`plugins.websearch.*`）
//
// 存储说明（2026-10-24「插件配置收回 data 层」）：配置落 **data 层 usr 配置库**的
// `plugins.websearch.*` 键（写哪一级由本插件配置页语义决定 —— 本插件是用户级，故落 usr）。
// **API Key 写库原文**（密钥只在 usr 层，绝不进项目共享库）；`/api/load` 回前端恒掩码。
// 上一版写在插件私有文件 `<CHONKPILOT_PLUGIN_DATA_DIR>/webui-config.json`：启动时**只读一次**
// 做迁移（migratePrivateFileLocked），成功后删除该文件（单源）。
// 旧宿主配置键（含旧 secrets.json 的 apiKey）只允许在 migrateLocked 中一次性读取；
// load 恒回显掩码，保存时空 / 掩码 = 不修改。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// ─── 键命名空间（唯一源）────────────────────────────────────────────
const (
	KeyAPIKey            = "plugins.websearch.api-key"
	KeyBaseURL           = "plugins.websearch.base-url"
	KeySearchPath        = "plugins.websearch.search-path"
	KeyCount             = "plugins.websearch.count"
	KeyNeedContent       = "plugins.websearch.need-content"
	KeyTimeoutSec        = "plugins.websearch.timeout-sec"
	KeyFetchMaxMB        = "plugins.websearch.fetch-max-mb"
	KeyFetchTimeoutSec   = "plugins.websearch.fetch-timeout-sec"
	KeyDownloadDir       = "plugins.websearch.download-dir"
	KeyDownloadMaxMB     = "plugins.websearch.download-max-mb"
	KeyDownloadExts      = "plugins.websearch.download-exts"
	KeyDownloadOverwrite = "plugins.websearch.download-overwrite"
)

// 校验区间（与旧 manifest 声明一致）。
const (
	countMin         = 1
	countMax         = 50
	timeoutSecMin    = 5
	timeoutSecMax    = 120
	fetchMaxMBMin    = 1
	fetchMaxMBMax    = 32
	fetchTimeoutMin  = 5
	fetchTimeoutMax  = 600
	downloadMaxMBMin = 1
	downloadMaxMBMax = 2048
)

// allKeys 是本插件的用户配置键全集（稳定序；load 恒回显这些键的有效值）。
var allKeys = []string{
	KeyAPIKey, KeyBaseURL, KeySearchPath, KeyCount, KeyNeedContent, KeyTimeoutSec,
	KeyFetchMaxMB, KeyFetchTimeoutSec,
	KeyDownloadDir, KeyDownloadMaxMB, KeyDownloadExts, KeyDownloadOverwrite,
}

// legacyKeyMap 旧宿主键 → 新命名空间键（**迁移专用**；除 migrateLocked 外不得读旧键）。
var legacyKeyMap = []struct{ Old, New string }{
	{"apiKey", KeyAPIKey},
	{"baseUrl", KeyBaseURL},
	{"searchPath", KeySearchPath},
	{"count", KeyCount},
	{"needContent", KeyNeedContent},
	{"timeoutSec", KeyTimeoutSec},
	{"fetchMaxMB", KeyFetchMaxMB},
	{"fetchTimeoutSec", KeyFetchTimeoutSec},
	{"downloadDir", KeyDownloadDir},
	{"downloadMaxMB", KeyDownloadMaxMB},
	{"downloadExts", KeyDownloadExts},
	{"downloadOverwrite", KeyDownloadOverwrite},
}

// configFields 是配置页字段声明（页面据 type/min/max/secret 动态渲染）。
var configFields = []fieldSpec{
	{Key: KeyAPIKey, Type: "string", Label: "DeepSeek API Key（仅检索需要；留空/掩码 = 不修改）", Secret: true},
	{Key: KeyBaseURL, Type: "string", Label: "检索 API Base URL"},
	{Key: KeySearchPath, Type: "string", Label: "检索路径"},
	{Key: KeyCount, Type: "integer", Label: "默认返回条数", Min: intPtr(countMin), Max: intPtr(countMax)},
	{Key: KeyNeedContent, Type: "boolean", Label: "检索默认返回正文"},
	{Key: KeyTimeoutSec, Type: "integer", Label: "检索超时（秒）", Min: intPtr(timeoutSecMin), Max: intPtr(timeoutSecMax)},
	{Key: KeyFetchMaxMB, Type: "integer", Label: "web_fetch 单次上限（MB）", Min: intPtr(fetchMaxMBMin), Max: intPtr(fetchMaxMBMax)},
	{Key: KeyFetchTimeoutSec, Type: "integer", Label: "web_fetch / web_download 超时（秒）", Min: intPtr(fetchTimeoutMin), Max: intPtr(fetchTimeoutMax)},
	{Key: KeyDownloadDir, Type: "string", Label: "web_download 默认目录（空 = ~/Downloads/chonkpilot）"},
	{Key: KeyDownloadMaxMB, Type: "integer", Label: "web_download 单文件上限（MB）", Min: intPtr(downloadMaxMBMin), Max: intPtr(downloadMaxMBMax)},
	{Key: KeyDownloadExts, Type: "string", Label: "允许的扩展名（逗号分隔，空 = 全部）"},
	{Key: KeyDownloadOverwrite, Type: "boolean", Label: "同名文件覆盖（关闭则自动加 -1）"},
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

func intPtr(n int) *int { return &n }

// ─── 配置模型 ────────────────────────────────────────────────────

// settings 是本插件配置的内存视图（落盘形态 = allKeys 的字符串 KV）。
type settings struct {
	APIKey            string
	BaseURL           string
	SearchPath        string
	Count             int
	NeedContent       bool
	TimeoutSec        int
	FetchMaxMB        int
	FetchTimeoutSec   int
	DownloadDir       string
	DownloadMaxMB     int
	DownloadExts      string
	DownloadOverwrite bool
}

// defaultSettings 缺键兜底（与实际生效值一致）。
func defaultSettings() settings {
	return settings{
		BaseURL:         "https://api.deepseek.com",
		SearchPath:      "/v1/tools/search",
		Count:           10,
		TimeoutSec:      30,
		FetchMaxMB:      4,
		FetchTimeoutSec: 60,
		DownloadMaxMB:   100,
	}
}

// settingsOf 由「已存在的 KV 键」构造配置快照（缺失 / 越界按默认）。
func settingsOf(values map[string]string) settings {
	s := defaultSettings()
	s.APIKey = strings.TrimSpace(values[KeyAPIKey])
	if v := strings.TrimSpace(values[KeyBaseURL]); v != "" {
		s.BaseURL = v
	}
	if v := strings.TrimSpace(values[KeySearchPath]); v != "" {
		s.SearchPath = v
	}
	if n, ok := parseIntIn(values[KeyCount], countMin, countMax); ok {
		s.Count = n
	}
	if raw, ok := values[KeyNeedContent]; ok {
		s.NeedContent = parseBool(raw, s.NeedContent)
	}
	if n, ok := parseIntIn(values[KeyTimeoutSec], timeoutSecMin, timeoutSecMax); ok {
		s.TimeoutSec = n
	}
	if n, ok := parseIntIn(values[KeyFetchMaxMB], fetchMaxMBMin, fetchMaxMBMax); ok {
		s.FetchMaxMB = n
	}
	if n, ok := parseIntIn(values[KeyFetchTimeoutSec], fetchTimeoutMin, fetchTimeoutMax); ok {
		s.FetchTimeoutSec = n
	}
	s.DownloadDir = strings.TrimSpace(values[KeyDownloadDir])
	if n, ok := parseIntIn(values[KeyDownloadMaxMB], downloadMaxMBMin, downloadMaxMBMax); ok {
		s.DownloadMaxMB = n
	}
	s.DownloadExts = strings.TrimSpace(values[KeyDownloadExts])
	if raw, ok := values[KeyDownloadOverwrite]; ok {
		s.DownloadOverwrite = parseBool(raw, s.DownloadOverwrite)
	}
	return s
}

// values 把快照展开成 `键 → 字符串` 的有效值（load 回显用；apiKey 恒掩码）。
func (s settings) values() map[string]string {
	return map[string]string{
		KeyAPIKey:            maskSecret(s.APIKey),
		KeyBaseURL:           s.BaseURL,
		KeySearchPath:        s.SearchPath,
		KeyCount:             strconv.Itoa(s.Count),
		KeyNeedContent:       boolString(s.NeedContent),
		KeyTimeoutSec:        strconv.Itoa(s.TimeoutSec),
		KeyFetchMaxMB:        strconv.Itoa(s.FetchMaxMB),
		KeyFetchTimeoutSec:   strconv.Itoa(s.FetchTimeoutSec),
		KeyDownloadDir:       s.DownloadDir,
		KeyDownloadMaxMB:     strconv.Itoa(s.DownloadMaxMB),
		KeyDownloadExts:      s.DownloadExts,
		KeyDownloadOverwrite: boolString(s.DownloadOverwrite),
	}
}

func boolString(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func parseBool(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true
	case "false":
		return false
	}
	return def
}

func parseIntIn(raw string, min, max int) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < min || n > max {
		return 0, false
	}
	return n, true
}

// maskSecret 回显掩码（空 → ""；<=8 字符 → "****"）。
func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
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

// Save 增量写（逐键落库；空 map = 无操作）。密钥以**原文**写库（掩码只作用于回显）。
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

// Actions 返回本插件承担 UI 动作名（宿主 `POST /plugins/websearch/api/<action>`）。
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
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s（只接受 plugins.websearch.* 命名空间）", k)
		}
		if k == KeyAPIKey {
			// 空 / 掩码 = 不修改（密钥永不回显明文；掩码含 **** → 视为未改）。
			if s := strings.TrimSpace(pluginsdk.Str(v)); s == "" || strings.Contains(s, "****") {
				continue
			}
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
	m.logf("websearch: 配置已保存（keys=%s）", strings.Join(keys, ","))
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
	case KeyAPIKey:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if s == "" {
			return "", fmt.Errorf("config_invalid: %s 不能为空（留空 = 不修改）", key)
		}
		if len(s) > 512 {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 512 字符，收到 %d）", key, len(s))
		}
		for _, r := range s {
			if r < 0x21 || r > 0x7e {
				return "", fmt.Errorf("config_invalid: %s 只允许可见 ASCII 字符（不含空白）", key)
			}
		}
		return s, nil
	case KeyBaseURL:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "", fmt.Errorf("config_invalid: %s 必须是 http/https 绝对地址（收到 %q）", key, s)
		}
		return s, nil
	case KeySearchPath:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if !strings.HasPrefix(s, "/") || len(s) > 256 || strings.ContainsAny(s, " \t\r\n") {
			return "", fmt.Errorf("config_invalid: %s 必须以 / 开头的路径（收到 %q）", key, s)
		}
		return s, nil
	case KeyCount:
		return normalizeInt(key, raw, countMin, countMax)
	case KeyNeedContent, KeyDownloadOverwrite:
		s := strings.ToLower(strings.TrimSpace(pluginsdk.Str(raw)))
		if s == "true" || s == "false" {
			return s, nil
		}
		return "", fmt.Errorf("config_invalid: %s 必须是 \"true\"/\"false\"（收到 %q）", key, pluginsdk.Str(raw))
	case KeyTimeoutSec:
		return normalizeInt(key, raw, timeoutSecMin, timeoutSecMax)
	case KeyFetchMaxMB:
		return normalizeInt(key, raw, fetchMaxMBMin, fetchMaxMBMax)
	case KeyFetchTimeoutSec:
		return normalizeInt(key, raw, fetchTimeoutMin, fetchTimeoutMax)
	case KeyDownloadMaxMB:
		return normalizeInt(key, raw, downloadMaxMBMin, downloadMaxMBMax)
	case KeyDownloadDir:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if s == "" {
			return "", nil
		}
		if len(s) > 1024 {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 1024 字符，收到 %d）", key, len(s))
		}
		if strings.ContainsRune(s, 0) {
			return "", fmt.Errorf("config_invalid: %s 含非法字符 NUL", key)
		}
		if strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
			return s, nil
		}
		if !filepath.IsAbs(s) {
			return "", fmt.Errorf("config_invalid: %s 必须是绝对路径或以 ~/ 开头（收到 %q）", key, s)
		}
		return filepath.Clean(s), nil
	case KeyDownloadExts:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if len(s) > 1024 {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 1024 字符，收到 %d）", key, len(s))
		}
		for _, item := range strings.Split(s, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			ext := strings.TrimPrefix(item, ".")
			if ext == "" || len(ext) > 32 {
				return "", fmt.Errorf("config_invalid: %s 的扩展名 %q 非法（须 1–32 字符）", key, item)
			}
			for _, r := range ext {
				ok := r == '-' || r == '_' || (r >= '0' && r <= '9') ||
					(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
				if !ok {
					return "", fmt.Errorf("config_invalid: %s 的扩展名 %q 含非法字符 %q", key, item, string(r))
				}
			}
		}
		return s, nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s", key)
}

func normalizeInt(key string, raw any, min, max int) (string, error) {
	s := strings.TrimSpace(pluginsdk.Str(raw))
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return "", fmt.Errorf("config_invalid: %s 必须是 %d–%d 的整数（收到 %q）", key, min, max, s)
	}
	return strconv.Itoa(n), nil
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

// ─── 一次性迁移（旧宿主键 → plugins.websearch.*）────────────────────

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
		if lm.New == KeyAPIKey {
			// 密钥无默认：旧值非空即迁移。
			m.values[lm.New] = val
			changed = true
			m.logf("配置迁移 %s → %s（密钥值不回显）", lm.Old, lm.New)
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
