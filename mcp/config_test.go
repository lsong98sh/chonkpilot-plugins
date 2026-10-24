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

// TestKeyNamespace：全部用户配置键统一 `plugins.mcp.` 前缀且不重复。
func TestKeyNamespace(t *testing.T) {
	seen := map[string]bool{}
	for _, k := range allKeys {
		if !strings.HasPrefix(k, "plugins.mcp.") {
			t.Errorf("键 %q 缺少 plugins.mcp. 前缀", k)
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
	if values[KeyDefaultLimit] != "20" || values[KeyIncludeNonHot] != "true" || values[KeyInvokeTimeoutSec] != "120" {
		t.Errorf("默认值错误：%v", values)
	}
}

func TestHandleSaveWritesNamespaceAndReadsBack(t *testing.T) {
	store := newMemStore(nil)
	m := newTestManager(t, store, nil)
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{
			KeyDefaultLimit:     "5",
			KeyIncludeNonHot:    false,
			KeyInvokeTimeoutSec: "60",
		},
	}); err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	want := map[string]string{
		KeyDefaultLimit:     "5",
		KeyIncludeNonHot:    "false",
		KeyInvokeTimeoutSec: "60",
	}
	for k, v := range want {
		if got, ok := store.get(k); !ok || got != v {
			t.Errorf("落库 %s = %q(存在=%v)，期望 %q", k, got, ok, v)
		}
	}
	cfg := m.Settings()
	if cfg.DefaultLimit != 5 || cfg.IncludeNonHot || cfg.InvokeTimeoutSec != 60 {
		t.Errorf("Settings = %+v", cfg)
	}
	res, err := m.handleLoad(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleLoad: %v", err)
	}
	if got := res.(map[string]any)["values"].(map[string]string)[KeyIncludeNonHot]; got != "false" {
		t.Errorf("读回 include-non-hot = %q", got)
	}
}

func TestHandleSaveMergesPartial(t *testing.T) {
	m := newTestManager(t, newMemStore(nil), nil)
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{KeyDefaultLimit: "8"},
	}); err != nil {
		t.Fatalf("save #1: %v", err)
	}
	if _, err := m.handleSave(context.Background(), map[string]any{
		"entries": map[string]any{KeyIncludeNonHot: "false"},
	}); err != nil {
		t.Fatalf("save #2: %v", err)
	}
	cfg := m.Settings()
	if cfg.DefaultLimit != 8 || cfg.IncludeNonHot {
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
		{"布尔非法", KeyIncludeNonHot, "yes", `必须是 "true"/"false"`},
		{"条数下界", KeyDefaultLimit, "0", "1–200"},
		{"条数上界", KeyDefaultLimit, "201", "1–200"},
		{"超时下界", KeyInvokeTimeoutSec, "4", "5–1800"},
		{"超时上界", KeyInvokeTimeoutSec, "1801", "5–1800"},
		{"未知键（旧键名）", "defaultLimit", "10", "未知配置键"},
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
		"entries": map[string]any{KeyDefaultLimit: "7"},
	})
	if err == nil || !strings.Contains(err.Error(), "配置写入失败") {
		t.Fatalf("期望「配置写入失败」，实际 %v", err)
	}
}

func TestHandleReset(t *testing.T) {
	store := newMemStore(map[string]string{KeyDefaultLimit: "7"})
	m := newTestManager(t, store, nil)
	res, err := m.handleReset(context.Background(), nil)
	if err != nil {
		t.Fatalf("handleReset: %v", err)
	}
	if store.len() != 0 || store.deletes != 1 {
		t.Errorf("reset 应删存储：len=%d deletes=%d", store.len(), store.deletes)
	}
	if res.(map[string]any)["values"].(map[string]string)[KeyDefaultLimit] != "20" {
		t.Errorf("reset 后应回显默认：%v", res)
	}
}

// ─── 一次性迁移 ─────────────────────────────────────────────────

func TestMigrateLegacyOnce(t *testing.T) {
	store := newMemStore(nil)
	legacy := map[string]any{
		"defaultLimit":     float64(15),
		"includeNonHot":    false,
		"invokeTimeoutSec": "90",
	}
	_ = newTestManager(t, store, legacy)
	want := map[string]string{
		KeyDefaultLimit:     "15",
		KeyIncludeNonHot:    "false",
		KeyInvokeTimeoutSec: "90",
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
		"defaultLimit":     20,
		"includeNonHot":    true,
		"invokeTimeoutSec": 120,
	})
	if store.len() != 0 {
		t.Errorf("默认旧值不应迁移落键：%v", store.data)
	}
}

// ─── 业务生效：info 读新配置 ────────────────────────────────────

func TestInfoUsesSettings(t *testing.T) {
	m := newTestManager(t, newMemStore(map[string]string{
		KeyDefaultLimit:     "3",
		KeyIncludeNonHot:    "false",
		KeyInvokeTimeoutSec: "30",
	}), nil)
	p := &mcpPlugin{cfg: m}
	res, err := p.info(context.Background(), nil)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	out := res.(map[string]any)
	if out["defaultLimit"] != 3 || out["includeNonHot"] != false || out["invokeTimeoutSec"] != 30 {
		t.Errorf("info = %v", out)
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
	for _, want := range []string{"/plugins/mcp/api/", "X-CK-Plugin-Id", "'load'", "'save'", "'reset'"} {
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
		path := writeFile(`{"` + key + `":"from-file"}`)
		store := newMemStore(nil)
		initWith(store, path)
		if v, ok := store.get(key); !ok || v != "from-file" {
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
		path := writeFile(`{"` + key + `":"from-file"}`)
		store := newMemStore(map[string]string{key: "from-data"})
		initWith(store, path)
		if v, _ := store.get(key); v != "from-data" {
			t.Fatalf("data 层已有值不应被覆盖：%q", v)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("迁移完成后应删除旧文件：%v", err)
		}
	})
	t.Run("写入失败→保留文件", func(t *testing.T) {
		path := writeFile(`{"` + key + `":"from-file"}`)
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
