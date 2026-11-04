package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/configkit"
)

// 配置键（唯一源；落插件数据目录 config.json，键名沿用 `plugins.remote.` 前缀）。
const (
	KeyServerURL = "plugins.remote.serverUrl"
	KeyUsername  = "plugins.remote.username"
	KeyPassword  = "plugins.remote.password"
)

// settings 是配置的内存视图。
type settings struct {
	ServerURL string
	Username  string
	Password  string
}

// Configured 判定是否具备出站连接所需的最小配置。
func (s settings) Configured() bool {
	return s.ServerURL != "" && s.Username != "" && s.Password != ""
}

func settingsOf(values map[string]string) settings {
	return settings{
		ServerURL: strings.TrimSpace(values[KeyServerURL]),
		Username:  strings.TrimSpace(values[KeyUsername]),
		Password:  values[KeyPassword],
	}
}

// publicValues 是回显给配置页的字段（密码永不回显）。
func (s settings) publicValues() map[string]string {
	return map[string]string{
		"serverUrl": s.ServerURL,
		"username":  s.Username,
	}
}

// persistedValues 是落库形态（含密码，仅写不读回显）。
func (s settings) persistedValues() map[string]string {
	return map[string]string{
		KeyServerURL: s.ServerURL,
		KeyUsername:  s.Username,
		KeyPassword:  s.Password,
	}
}

// fieldSpec 是配置页字段声明（key = 页面字段名，非落库全键）。
type fieldSpec struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder,omitempty"`
}

var configFields = []fieldSpec{
	{Key: "serverUrl", Type: "string", Label: "服务器地址", Placeholder: "wss://host/tunnel（也接受 ws://）"},
	{Key: "username", Type: "string", Label: "用户名"},
	{Key: "password", Type: "password", Label: "密码", Placeholder: "留空 = 保持原密码"},
}

// configStore 是配置存储的最小依赖（可替换：测试 = 内存桩）。
type configStore interface {
	Load(ctx context.Context) (map[string]string, error)
	Save(ctx context.Context, values map[string]string) error
	Delete(ctx context.Context) error
}

// fileConfigStore 把插件配置落在插件自己的数据目录（config.json），
// 不依赖宿主 data 层的启动时序；密码仅写不读回显。
type fileConfigStore struct{ path string }

func newFileConfigStore(dir string) *fileConfigStore {
	return &fileConfigStore{path: filepath.Join(dir, "config.json")}
}

func (s *fileConfigStore) Load(context.Context) (map[string]string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch k {
		case "serverUrl":
			out[KeyServerURL] = v
		case "username":
			out[KeyUsername] = v
		case "password":
			out[KeyPassword] = v
		default:
			if strings.HasPrefix(k, "plugins.remote.") {
				out[k] = v
			}
		}
	}
	return out, nil
}

func (s *fileConfigStore) Save(_ context.Context, values map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *fileConfigStore) Delete(context.Context) error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// configManager 承载配置动作与生效快照。
type configManager struct {
	store    configStore
	log      *slog.Logger
	onChange func()

	mu     sync.RWMutex
	values map[string]string
}

func newConfigManager(log *slog.Logger) *configManager {
	return newConfigManagerWithStore(newFileConfigStore(pluginsdk.DefaultDataDir()), log)
}

func newConfigManagerWithStore(store configStore, log *slog.Logger) *configManager {
	return &configManager{store: store, log: log, values: map[string]string{}}
}

// Init 启动时读取配置（失败仅记录，运行时按未配置兜底）。
func (m *configManager) Init(ctx context.Context) error {
	return m.Reload(ctx)
}

// Reload 重新从宿主配置存储读取（启动竞态 / 外部改配置时重试）。
func (m *configManager) Reload(ctx context.Context) error {
	values, err := m.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	}
	if values == nil {
		values = map[string]string{}
	}
	m.mu.Lock()
	m.values = values
	m.mu.Unlock()
	return nil
}

// Settings 返回当前生效配置快照。
func (m *configManager) Settings() settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return settingsOf(m.values)
}

// SetValues 直接替换内存配置（测试用；生产走 save 动作）。
func (m *configManager) SetValues(values map[string]string) {
	m.mu.Lock()
	m.values = values
	m.mu.Unlock()
}

// Save 持久化并热生效（配置页 save 动作的公共实现）。
func (m *configManager) Save(ctx context.Context, values map[string]string) error {
	if err := m.store.Save(ctx, values); err != nil {
		return fmt.Errorf("配置写入失败：%w", err)
	}
	m.mu.Lock()
	m.values = values
	m.mu.Unlock()
	m.notify()
	return nil
}

func (m *configManager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

// Actions 返回 UI 动作（宿主 `POST /plugins/remote/api/<action>`）。
func (m *configManager) Actions() map[string]pluginsdk.RequestFunc {
	return map[string]pluginsdk.RequestFunc{
		"load":  m.handleLoad,
		"save":  m.handleSave,
		"reset": m.handleReset,
	}
}

func (m *configManager) handleLoad(_ context.Context, _ map[string]any) (any, error) {
	s := m.Settings()
	return map[string]any{
		"ok":          true,
		"fields":      configFields,
		"values":      s.publicValues(),
		"hasPassword": s.Password != "",
		"configured":  s.Configured(),
	}, nil
}

func (m *configManager) handleSave(ctx context.Context, payload map[string]any) (any, error) {
	entries, err := configkit.SaveEntries(payload)
	if err != nil {
		return nil, err
	}
	cur := m.Settings()
	next := cur
	keys := make([]string, 0, len(entries))
	for name, raw := range entries {
		val := pluginsdk.Str(raw)
		switch name {
		case "serverUrl":
			norm, err := normalizeServerURL(val)
			if err != nil {
				return nil, err
			}
			next.ServerURL = norm
		case "username":
			next.Username = strings.TrimSpace(val)
		case "password":
			// 留空 = 保持原密码（密码不回显 → 用户不清空即不修改）。
			if val != "" {
				next.Password = val
			}
		default:
			return nil, fmt.Errorf("config_invalid: 未知配置项 %s", name)
		}
		keys = append(keys, name)
	}
	if err := m.Save(ctx, next.persistedValues()); err != nil {
		return nil, err
	}
	if m.log != nil {
		m.log.Info("remote: 配置已保存", "keys", strings.Join(keys, ","))
	}
	return map[string]any{"ok": true, "keys": keys, "values": next.publicValues()}, nil
}

func (m *configManager) handleReset(ctx context.Context, _ map[string]any) (any, error) {
	if err := m.store.Delete(ctx); err != nil {
		return nil, fmt.Errorf("恢复默认失败：%w", err)
	}
	m.mu.Lock()
	m.values = map[string]string{}
	m.mu.Unlock()
	m.notify()
	return map[string]any{"ok": true, "values": defaultSettings().publicValues()}, nil
}

func defaultSettings() settings { return settings{} }

// normalizeServerURL 校验并归一服务器地址（空 = 未配置；只接受 ws:// / wss://）。
func normalizeServerURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("config_invalid: 服务器地址不合法（收到 %q）", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "ws" && scheme != "wss" {
		return "", errors.New("config_invalid: 服务器地址只接受 ws:// 或 wss://")
	}
	return s, nil
}
