package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// ─── 测试桩：配置存储 ────────────────────────────────────────────

type memStore struct {
	mu      sync.Mutex
	data    map[string]string
	saves   int
	deletes int

	failSave error
}

func newMemStore(values map[string]string) *memStore {
	data := map[string]string{}
	for k, v := range values {
		data[k] = v
	}
	return &memStore{data: data}
}

func (s *memStore) Load(_ context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.data {
		out[k] = v
	}
	return out, nil
}

func (s *memStore) Save(_ context.Context, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	if s.failSave != nil {
		return s.failSave
	}
	out := map[string]string{}
	for k, v := range values {
		out[k] = v
	}
	s.data = out
	return nil
}

func (s *memStore) Delete(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes++
	s.data = map[string]string{}
	return nil
}

func (s *memStore) get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

func (s *memStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}

func (s *memStore) saveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

func newTestManager(t *testing.T, store configStore, legacy map[string]any) *configManager {
	t.Helper()
	m := &configManager{store: store, logf: t.Logf}
	if legacy != nil {
		m.legacy = func(key string) (any, bool) {
			v, ok := legacy[key]
			return v, ok
		}
	}
	if err := m.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return m
}

// ─── 读取 / 写入 ─────────────────────────────────────────────────

// TestKeyNamespace：全部用户配置键统一 `plugins.websearch.` 前缀且不重复。
func TestKeyNamespace(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "plugins.websearch.") {
			t.Errorf("键 %q 缺少 plugins.websearch. 前缀", k)
		}
		if seen[k] {
			t.Errorf("键重复：%s", k)
		}
		seen[k] = true
	}
}

func TestHandleLoadDefaults(t *testing.T) {
	m := newTestManager(t, newMemStore(nil), nil)
	res, err := m.handleLoad(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleLoad: %v", err)
	}
	values := res.(map[string]any)["values"].(map[string]string)
	want := map[string]string{
		KeyAPIKey:            "",
		KeyBaseURL:           "https://api.deepseek.com",
		KeySearchPath:        "/v1/tools/search",
		KeyCount:             "10",
		KeyNeedContent:       "false",
		KeyTimeoutSec:        "30",
		KeyFetchMaxMB:        "4",
		KeyFetchTimeoutSec:   "60",
		KeyDownloadDir:       "",
		KeyDownloadMaxMB:     "100",
		KeyDownloadExts:      "",
		KeyDownloadOverwrite: "false",
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("默认 %s = %q，期望 %q", k, values[k], v)
		}
	}
}

func TestHandleSaveWritesNamespaceAndReadsBack(t *testing.T) {
	store := newMemStore(nil)
	m := newTestManager(t, store, nil)
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{
			KeyAPIKey:            "sk-test-key",
			KeyBaseURL:           "https://api.example.com",
			KeySearchPath:        "/v1/search",
			KeyCount:             "5",
			KeyNeedContent:       "true",
			KeyTimeoutSec:        "15",
			KeyFetchMaxMB:        "8",
			KeyFetchTimeoutSec:   "90",
			KeyDownloadDir:       `E:\dl`,
			KeyDownloadMaxMB:     "50",
			KeyDownloadExts:      ".zip,.pdf",
			KeyDownloadOverwrite: "true",
		},
	}); err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	if v, ok := store.get(KeyAPIKey); !ok || v != "sk-test-key" {
		t.Errorf("密钥应落私有存储：%q(存在=%v)", v, ok)
	}
	cfg := m.Settings()
	if cfg.APIKey != "sk-test-key" || cfg.BaseURL != "https://api.example.com" || cfg.Count != 5 ||
		!cfg.NeedContent || cfg.TimeoutSec != 15 || cfg.FetchMaxMB != 8 || cfg.FetchTimeoutSec != 90 ||
		cfg.DownloadDir != `E:\dl` || cfg.DownloadMaxMB != 50 || cfg.DownloadExts != ".zip,.pdf" || !cfg.DownloadOverwrite {
		t.Errorf("Settings = %+v", cfg)
	}
	res, err := m.handleLoad(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleLoad: %v", err)
	}
	values := res.(map[string]any)["values"].(map[string]string)
	if values[KeyAPIKey] != "sk-t****-key" {
		t.Errorf("load 应回显掩码，实际 %q", values[KeyAPIKey])
	}
	if values[KeyCount] != "5" {
		t.Errorf("读回 count = %q", values[KeyCount])
	}
}

// TestHandleSaveKeepsAPIKeyWhenEmptyOrMasked：空 / 掩码 = 不修改。
func TestHandleSaveKeepsAPIKeyWhenEmptyOrMasked(t *testing.T) {
	store := newMemStore(map[string]string{KeyAPIKey: "sk-real-key-0001"})
	m := newTestManager(t, store, nil)
	for _, v := range []string{"", "****", "sk-****0001"} {
		if _, err := m.handleSave(context.Background(), map[string]any{
			"entries": map[string]any{KeyAPIKey: v, KeyCount: "6"},
		}); err != nil {
			t.Fatalf("save(%q): %v", v, err)
		}
	}
	if got, _ := store.get(KeyAPIKey); got != "sk-real-key-0001" {
		t.Errorf("空/掩码不应覆盖密钥：%q", got)
	}
	if m.Settings().Count != 6 {
		t.Errorf("同批其它键应生效：%+v", m.Settings())
	}
}

func TestHandleSaveMergesPartial(t *testing.T) {
	m := newTestManager(t, newMemStore(nil), nil)
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{KeyCount: "5"},
	}); err != nil {
		t.Fatalf("save #1: %v", err)
	}
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{KeyNeedContent: "true"},
	}); err != nil {
		t.Fatalf("save #2: %v", err)
	}
	cfg := m.Settings()
	if cfg.Count != 5 || !cfg.NeedContent {
		t.Errorf("部分保存丢失既有键：%+v", cfg)
	}
}

// ─── 校验 ────────────────────────────────────────────────────────

func TestHandleSaveValidationRejects(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value any
		want  string
	}{
		{"base-url 非绝对", KeyBaseURL, "api.example.com", "http/https 绝对地址"},
		{"base-url 协议非法", KeyBaseURL, "ftp://example.com", "http/https 绝对地址"},
		{"search-path 无前缀", KeySearchPath, "v1/search", "以 / 开头"},
		{"count 下界", KeyCount, "0", "1–50"},
		{"count 上界", KeyCount, "51", "1–50"},
		{"布尔非法", KeyNeedContent, "yes", `必须是 "true"/"false"`},
		{"检索超时越界", KeyTimeoutSec, "121", "5–120"},
		{"fetch 上限越界", KeyFetchMaxMB, "33", "1–32"},
		{"fetch 超时越界", KeyFetchTimeoutSec, "601", "5–600"},
		{"下载上限越界", KeyDownloadMaxMB, "2049", "1–2048"},
		{"下载目录相对", KeyDownloadDir, "downloads", "绝对路径"},
		{"扩展名非法字符", KeyDownloadExts, ".zip,exe/zip", "非法字符"},
		{"未知键（旧键名）", "apiKey", "sk-x", "未知配置键"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore(nil)
			m := newTestManager(t, store, nil)
			_, err := m.handleSave(context.Background(), map[string]any{
				"entries": map[string]any{tc.key: tc.value},
			})
			if err == nil || !strings.Contains(err.Error(), "config_invalid") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望 config_invalid 且含 %q，实际 %v", tc.want, err)
			}
			if store.len() != 0 {
				t.Errorf("校验失败不应写任何键：%v", store.data)
			}
		})
	}
}

func TestHandleSaveStoreErrorExplicit(t *testing.T) {
	store := newMemStore(nil)
	store.failSave = errors.New("disk down")
	m := newTestManager(t, store, nil)
	_, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{KeyCount: "5"},
	})
	if err == nil || !strings.Contains(err.Error(), "配置写入失败") {
		t.Fatalf("期望「配置写入失败」，实际 %v", err)
	}
}

func TestHandleReset(t *testing.T) {
	store := newMemStore(map[string]string{KeyCount: "5", KeyAPIKey: "sk-x-0001"})
	m := newTestManager(t, store, nil)
	res, err := m.handleReset(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleReset: %v", err)
	}
	if store.len() != 0 || store.deletes != 1 {
		t.Errorf("reset 应删存储：len=%d deletes=%d", store.len(), store.deletes)
	}
	values := res.(map[string]any)["values"].(map[string]string)
	if values[KeyCount] != "10" || values[KeyAPIKey] != "" {
		t.Errorf("reset 后应回显默认：%v", values)
	}
}

// ─── 一次性迁移 ─────────────────────────────────────────────────

func TestMigrateLegacyOnce(t *testing.T) {
	store := newMemStore(nil)
	legacy := map[string]any{
		"apiKey":            "sk-legacy-key-0001",
		"baseUrl":           "https://legacy.example.com",
		"searchPath":        "/v2/search",
		"count":             float64(7),
		"needContent":       true,
		"timeoutSec":        "20",
		"fetchMaxMB":        6,
		"fetchTimeoutSec":   "120",
		"downloadDir":       `D:\dl`,
		"downloadMaxMB":     300,
		"downloadExts":      ".zip",
		"downloadOverwrite": true,
	}
	_ = newTestManager(t, store, legacy)
	want := map[string]string{
		KeyAPIKey:            "sk-legacy-key-0001",
		KeyBaseURL:           "https://legacy.example.com",
		KeySearchPath:        "/v2/search",
		KeyCount:             "7",
		KeyNeedContent:       "true",
		KeyTimeoutSec:        "20",
		KeyFetchMaxMB:        "6",
		KeyFetchTimeoutSec:   "120",
		KeyDownloadDir:       `D:\dl`,
		KeyDownloadMaxMB:     "300",
		KeyDownloadExts:      ".zip",
		KeyDownloadOverwrite: "true",
	}
	for k, v := range want {
		if got, ok := store.get(k); !ok || got != v {
			t.Errorf("迁移后 %s = %q(存在=%v)，期望 %q", k, got, ok, v)
		}
	}
	saves := store.saveCount()
	_ = newTestManager(t, store, legacy)
	if store.saveCount() != saves {
		t.Errorf("二次迁移不应再写：saves %d → %d", saves, store.saveCount())
	}
}

func TestMigrateSkipsDefaults(t *testing.T) {
	store := newMemStore(nil)
	_ = newTestManager(t, store, map[string]any{
		"baseUrl":           "https://api.deepseek.com",
		"searchPath":        "/v1/tools/search",
		"count":             10,
		"needContent":       false,
		"timeoutSec":        30,
		"fetchMaxMB":        4,
		"fetchTimeoutSec":   60,
		"downloadMaxMB":     100,
		"downloadOverwrite": false,
	})
	if store.len() != 0 {
		t.Errorf("默认旧值不应迁移落键：%v", store.data)
	}
}

// ─── 业务生效：status / download-dir 读新配置 ────────────────────

func TestStatusAndDownloadDirUseSettings(t *testing.T) {
	store := newMemStore(map[string]string{
		KeyAPIKey:            "sk-test-key",
		KeyBaseURL:           "https://api.example.com",
		KeyDownloadDir:       `E:\dl`,
		KeyDownloadMaxMB:     "50",
		KeyDownloadExts:      ".zip",
		KeyDownloadOverwrite: "true",
		KeyFetchMaxMB:        "8",
	})
	m := newTestManager(t, store, nil)
	s := &searcher{cfg: m}

	st, err := s.status(context.Background(), nil)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	out := st.(map[string]any)
	if out["configured"] != true || out["base_url"] != "https://api.example.com" {
		t.Errorf("status = %v", out)
	}

	dir, err := s.downloadDir(context.Background(), nil)
	if err != nil {
		t.Fatalf("download-dir: %v", err)
	}
	dd := dir.(map[string]any)
	if dd["dir"] != `E:\dl` || dd["maxMB"] != 50 || dd["exts"] != ".zip" || dd["overwrite"] != true || dd["fetchMaxMB"] != 8 {
		t.Errorf("download-dir = %v", dd)
	}
}

// ─── 单源守卫 ────────────────────────────────────────────────────

func TestConfigPageUsesAPIActions(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("webui", "config.html"))
	if err != nil {
		t.Fatalf("读 config.html: %v", err)
	}
	src := string(raw)
	for _, banned := range []string{"ckPlugin.call('config.", `ckPlugin.call("config.`, "plugin-config-"} {
		if strings.Contains(src, banned) {
			t.Errorf("config.html 不应出现 %q（配置只能经 /api/{load,save,reset}）", banned)
		}
	}
	for _, want := range []string{"/plugins/websearch/api/", "X-CK-Plugin-Id", "'load'", "'save'", "'reset'"} {
		if !strings.Contains(src, want) {
			t.Errorf("config.html 缺少 %q", want)
		}
	}
}

func TestLegacyHostConfigReadOnlyInMigration(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "config.go" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		if strings.Contains(string(raw), ".Config()") {
			t.Errorf("%s 仍在读旧宿主配置（单源已迁至 config.go 的存储/迁移）", name)
		}
	}
}

// ─── 私有文件迁移（上一版 webui-config.json → data 层）与单源守卫 ──────────

// TestMigratePrivateFileIntoDataLayer：上一版私有文件 → data 层（幂等：data 层已有值优先）。
func TestMigratePrivateFileIntoDataLayer(t *testing.T) {
	key := allKeys[0]
	writeFile := func(content string) string {
		path := filepath.Join(t.TempDir(), legacyConfigFileName)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("写旧配置文件: %v", err)
		}
		return path
	}
	initWith := func(store *memStore, path string) {
		t.Helper()
		m := &configManager{store: store, logf: t.Logf, legacyFile: path}
		if err := m.Init(context.Background()); err != nil {
			t.Fatalf("Init: %v", err)
		}
	}

	t.Run("文件存在→入库并删除", func(t *testing.T) {
		path := writeFile(`{"` + key + `":"sk-test-legacy"}`)
		store := newMemStore(nil)
		initWith(store, path)
		if v, ok := store.get(key); !ok || v != "sk-test-legacy" {
			t.Fatalf("迁移后 %s = %q(存在=%v)", key, v, ok)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("迁移成功后应删除旧文件：%v", err)
		}
	})
	t.Run("文件不存在→不报错", func(t *testing.T) {
		store := newMemStore(nil)
		initWith(store, filepath.Join(t.TempDir(), legacyConfigFileName))
		if store.saveCount() != 0 {
			t.Fatalf("无文件不应写存储：%d", store.saveCount())
		}
	})
	t.Run("data层已有值→不覆盖且删文件", func(t *testing.T) {
		path := writeFile(`{"` + key + `":"sk-test-legacy"}`)
		store := newMemStore(map[string]string{key: "sk-test-data"})
		initWith(store, path)
		if v, _ := store.get(key); v != "sk-test-data" {
			t.Fatalf("data 层已有值不应被覆盖：%q", v)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("迁移完成后应删除旧文件：%v", err)
		}
	})
	t.Run("写入失败→保留文件", func(t *testing.T) {
		path := writeFile(`{"` + key + `":"sk-test-legacy"}`)
		store := newMemStore(nil)
		store.failSave = errors.New("db down")
		initWith(store, path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("写入失败应保留旧文件（下次启动重试）：%v", err)
		}
	})
	t.Run("解析失败→保留文件", func(t *testing.T) {
		path := writeFile(`{not json`)
		store := newMemStore(nil)
		initWith(store, path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("解析失败应保留旧文件：%v", err)
		}
	})
}

// TestConfigStoreWritesDataLayerOnly：单源守卫——configStore 不再写插件私有配置文件。
func TestConfigStoreWritesDataLayerOnly(t *testing.T) {
	raw, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读 config.go: %v", err)
	}
	src := string(raw)
	for _, banned := range []string{"os.WriteFile", "os.Rename", "os.MkdirAll"} {
		if strings.Contains(src, banned) {
			t.Errorf("config.go 不应再写文件（%s）：插件配置单源 = data 层 usr 配置", banned)
		}
	}
	for _, want := range []string{"plugin/v1/config.user.load", "plugin/v1/config.user.set", "plugin/v1/config.user.delete"} {
		if !strings.Contains(src, want) {
			t.Errorf("config.go 缺少 %q（写侧应走宿主能力面）", want)
		}
	}
}
