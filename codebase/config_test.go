package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── 测试桩：prj 配置面 ──────────────────────────────────────────

// fakeStore 是 prjStore 的内存实现（可注入读/写/删失败，用于重试语义断言）。
type fakeStore struct {
	mu   sync.Mutex
	data map[string]map[string]string // instance → key → value
	sets int
	dels int

	failGet    map[string]int // key → 剩余失败次数
	failSet    map[string]int
	failDelete map[string]int
}

func newFakeStore(values map[string]string) *fakeStore {
	data := map[string]map[string]string{}
	inst := map[string]string{}
	for k, v := range values {
		inst[k] = v
	}
	data["inst-1"] = inst
	return &fakeStore{data: data, failGet: map[string]int{}, failSet: map[string]int{}, failDelete: map[string]int{}}
}

func (s *fakeStore) Get(_ context.Context, instanceID, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.failGet[key]; n > 0 {
		s.failGet[key] = n - 1
		return "", false, errors.New("boom-get")
	}
	v, ok := s.data[instanceID][key]
	return v, ok && v != "", nil
}

func (s *fakeStore) Set(_ context.Context, instanceID string, entries map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range entries {
		if n := s.failSet[k]; n > 0 {
			s.failSet[k] = n - 1
			return errors.New("boom-set")
		}
	}
	if s.data[instanceID] == nil {
		s.data[instanceID] = map[string]string{}
	}
	for k, v := range entries {
		s.data[instanceID][k] = v
	}
	s.sets++
	return nil
}

func (s *fakeStore) Delete(_ context.Context, instanceID, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.failDelete[key]; n > 0 {
		s.failDelete[key] = n - 1
		return errors.New("boom-delete")
	}
	delete(s.data[instanceID], key)
	s.dels++
	return nil
}

func (s *fakeStore) value(instanceID, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[instanceID][key]
	return v, ok
}

// newTestManager 构造只依赖桩的配置管理器（不触达宿主 / 引擎）。
func newTestManager(store prjStore, workdirs map[string]string, calls *[]string, args *[]map[string]any, t *testing.T) *configManager {
	if workdirs == nil {
		workdirs = map[string]string{"inst-1": `E:\proj`}
	}
	return &configManager{
		store: store,
		engine: func(_ context.Context, name string, a map[string]any) (any, error) {
			if calls != nil {
				*calls = append(*calls, name)
			}
			if args != nil {
				*args = append(*args, a)
			}
			return map[string]any{"ok": true}, nil
		},
		logf:     t.Logf,
		workdirs: workdirs,
		applied:  map[string]string{},
	}
}

// ─── 迁移 ────────────────────────────────────────────────────────

// TestMigrateLegacyKeys：旧键 → 新键（写新键 + 删旧键）。
func TestMigrateLegacyKeys(t *testing.T) {
	store := newFakeStore(map[string]string{
		"enable-codegraph":          "true",
		"codegraph.exts":            ".go,.ts",
		"codegraph.skip-dirs":       "node_modules",
		"codegraph.stack-gitignore": "true",
		"enable-vfts":               "true",
		"vfts.exts":                 ".md",
		"vfts.skip-dirs":            "dist",
		"vfts.stack-gitignore":      "false",
		"vfts.docs":                 "true",
		"vfts.doc-max-mb":           "80",
	})
	// 运行时状态键不迁移
	store.data["inst-1"]["codegraph.status"] = `{"state":"ready"}`
	store.data["inst-1"]["vfts.status"] = `{"state":"ready"}`

	m := newTestManager(store, nil, nil, nil, t)
	if n := m.migrateInstance(context.Background(), "inst-1"); n != len(legacyKeyMap) {
		t.Fatalf("迁移键数 = %d，期望 %d", n, len(legacyKeyMap))
	}
	want := map[string]string{
		KeyCodegraphEnabled:        "true",
		KeyCodegraphExts:           ".go,.ts",
		KeyCodegraphSkipDirs:       "node_modules",
		KeyCodegraphStackGitignore: "true",
		KeyVftsEnabled:             "true",
		KeyVftsExts:                ".md",
		KeyVftsSkipDirs:            "dist",
		KeyVftsStackGitignore:      "false",
		KeyVftsDocs:                "true",
		KeyVftsDocMaxMB:            "80",
	}
	for k, v := range want {
		if got, ok := store.value("inst-1", k); !ok || got != v {
			t.Errorf("新键 %s = %q(存在=%v)，期望 %q", k, got, ok, v)
		}
	}
	for _, lm := range legacyKeyMap {
		if _, ok := store.value("inst-1", lm.Old); ok {
			t.Errorf("旧键 %s 应已删除", lm.Old)
		}
	}
	// 状态键保持原名
	if _, ok := store.value("inst-1", "codegraph.status"); !ok {
		t.Error("运行时状态键 codegraph.status 不应被迁移/删除")
	}
	if _, ok := store.value("inst-1", "vfts.status"); !ok {
		t.Error("运行时状态键 vfts.status 不应被迁移/删除")
	}
}

// TestMigrateIdempotent：重复执行不重复写（新键已存在 → 忽略）。
func TestMigrateIdempotent(t *testing.T) {
	store := newFakeStore(map[string]string{"enable-vfts": "true"})
	m := newTestManager(store, nil, nil, nil, t)
	if n := m.migrateInstance(context.Background(), "inst-1"); n != 1 {
		t.Fatalf("首次迁移 = %d，期望 1", n)
	}
	sets := store.sets
	if n := m.migrateInstance(context.Background(), "inst-1"); n != 0 {
		t.Fatalf("二次迁移 = %d，期望 0", n)
	}
	if store.sets != sets {
		t.Errorf("二次迁移不应再写：sets %d → %d", sets, store.sets)
	}
}

// TestMigrateKeepsOldWhenNewExists：新键已存在 → 不动旧键（单源以新键为准）。
func TestMigrateKeepsOldWhenNewExists(t *testing.T) {
	store := newFakeStore(map[string]string{
		"vfts.exts":       ".old",
		KeyVftsExts:       ".new",
		"vfts.doc-max-mb": "10",
		KeyVftsDocMaxMB:   "20",
	})
	m := newTestManager(store, nil, nil, nil, t)
	if n := m.migrateInstance(context.Background(), "inst-1"); n != 0 {
		t.Fatalf("迁移键数 = %d，期望 0（新键已存在）", n)
	}
	if v, _ := store.value("inst-1", KeyVftsExts); v != ".new" {
		t.Errorf("新键值被改写：%q", v)
	}
	if v, ok := store.value("inst-1", "vfts.exts"); !ok || v != ".old" {
		t.Errorf("新键已存在时不应改动旧键：got %q(存在=%v)", v, ok)
	}
}

// TestMigrateRetriesAfterPartialFailure：删旧键失败 → 回滚新键 → 下次重试成功（不丢配置）。
func TestMigrateRetriesAfterPartialFailure(t *testing.T) {
	store := newFakeStore(map[string]string{"vfts.exts": ".md"})
	store.failDelete["vfts.exts"] = 1
	m := newTestManager(store, nil, nil, nil, t)

	if n := m.migrateInstance(context.Background(), "inst-1"); n != 0 {
		t.Fatalf("删除失败时应迁移 0 键，实际 %d", n)
	}
	if _, ok := store.value("inst-1", "vfts.exts"); !ok {
		t.Fatal("删除失败后旧键应保留")
	}
	if _, ok := store.value("inst-1", KeyVftsExts); ok {
		t.Fatal("删除失败后应回滚新键（否则下次会被判为「新键已存在」而永不再迁）")
	}

	if n := m.migrateInstance(context.Background(), "inst-1"); n != 1 {
		t.Fatalf("重试应迁移 1 键，实际 %d", n)
	}
	if _, ok := store.value("inst-1", "vfts.exts"); ok {
		t.Error("重试成功后旧键应删除")
	}
	if v, ok := store.value("inst-1", KeyVftsExts); !ok || v != ".md" {
		t.Errorf("重试后新键 = %q(存在=%v)，期望 .md", v, ok)
	}
}

// TestMigrateReadFailureKeepsOld：读新键失败 → 保留旧键（下次重试）。
func TestMigrateReadFailureKeepsOld(t *testing.T) {
	store := newFakeStore(map[string]string{"vfts.docs": "true"})
	store.failGet[KeyVftsDocs] = 1
	m := newTestManager(store, nil, nil, nil, t)
	if n := m.migrateInstance(context.Background(), "inst-1"); n != 0 {
		t.Fatalf("读失败时应迁移 0 键，实际 %d", n)
	}
	if _, ok := store.value("inst-1", "vfts.docs"); !ok {
		t.Error("读失败后旧键应保留")
	}
	if n := m.migrateInstance(context.Background(), "inst-1"); n != 1 {
		t.Fatalf("重试应迁移 1 键，实际 %d", n)
	}
}

// TestMigrateOnlyInMigrationFunc：源码中旧键字面量只允许出现在 config.go 的 legacyKeyMap。
func TestMigrateOnlyInMigrationFunc(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		src := string(raw)
		for _, lm := range legacyKeyMap {
			count := strings.Count(src, `"`+lm.Old+`"`)
			if name == "config.go" {
				if count != 1 {
					t.Errorf("config.go 中旧键 %q 出现 %d 次（应仅 1 次 = legacyKeyMap）", lm.Old, count)
				}
				continue
			}
			if count != 0 {
				t.Errorf("%s 中仍引用旧键 %q（%d 次）——除迁移映射外不得有旧键读取路径", name, lm.Old, count)
			}
		}
	}
}

// ─── 保存链路：动作 → prj 新键 → 引擎 ────────────────────────────

// TestHandleSaveWritesNamespaceAndPushesEngine：save → 写 plugins.codebase.* → 引擎收到 configure。
func TestHandleSaveWritesNamespaceAndPushesEngine(t *testing.T) {
	store := newFakeStore(nil)
	var calls []string
	var args []map[string]any
	m := newTestManager(store, nil, &calls, &args, t)

	res, err := m.handleSave(context.Background(), map[string]any{
		"instance_id": "inst-1",
		"entries": map[string]any{
			KeyCodegraphEnabled:   "true",
			KeyCodegraphExts:      ".go, .ts",
			KeyVftsEnabled:        "true",
			KeyVftsDocs:           "true",
			KeyVftsDocMaxMB:       "120",
			KeyVftsStackGitignore: true,
		},
	})
	if err != nil {
		t.Fatalf("handleSave: %v", err)
	}
	out := res.(map[string]any)
	if out["instance_id"] != "inst-1" {
		t.Errorf("instance_id = %v", out["instance_id"])
	}

	// 1) prj 新键落库（含 bool 归一为 "true"/"false"）
	if v, ok := store.value("inst-1", KeyVftsStackGitignore); !ok || v != "true" {
		t.Errorf("vfts.stack-gitignore = %q(存在=%v)，期望 true", v, ok)
	}
	if v, ok := store.value("inst-1", KeyVftsDocMaxMB); !ok || v != "120" {
		t.Errorf("doc-max-mb = %q，期望 120", v)
	}

	// 2) 引擎收到 configure（codegraph 先、vfts 后）
	if len(calls) != 2 || calls[0] != "codegraph_configure" || calls[1] != "vfts_configure" {
		t.Fatalf("引擎调用 = %v，期望 [codegraph_configure vfts_configure]", calls)
	}
	if got := args[0]["workdir"]; got != `E:\proj` {
		t.Errorf("codegraph workdir = %v", got)
	}
	exts, _ := args[0]["exts"].([]string)
	if len(exts) != 2 || exts[0] != ".go" || exts[1] != ".ts" {
		t.Errorf("codegraph exts = %v", args[0]["exts"])
	}
	if got := args[1]["doc_max_bytes"]; got != int64(120)<<20 {
		t.Errorf("vfts doc_max_bytes = %v，期望 %d", got, int64(120)<<20)
	}
	applied, _ := out["applied"].([]string)
	if len(applied) != 2 {
		t.Errorf("applied = %v", out["applied"])
	}
}

// TestHandleSaveIdempotent：同值重复保存不重复下发引擎。
func TestHandleSaveIdempotent(t *testing.T) {
	store := newFakeStore(nil)
	var calls []string
	m := newTestManager(store, nil, &calls, nil, t)
	payload := map[string]any{
		"instance_id": "inst-1",
		"entries":     map[string]any{KeyVftsEnabled: "true"},
	}
	if _, err := m.handleSave(context.Background(), payload); err != nil {
		t.Fatalf("首次 save: %v", err)
	}
	if _, err := m.handleSave(context.Background(), payload); err != nil {
		t.Fatalf("二次 save: %v", err)
	}
	if len(calls) != 1 || calls[0] != "vfts_configure" {
		t.Fatalf("引擎调用 = %v，期望仅一次 vfts_configure", calls)
	}
}

// TestHandleSaveOnlyEnabledEngine：两引擎均关闭 → 不下发引擎（仅落配置）。
func TestHandleSaveOnlyEnabledEngine(t *testing.T) {
	store := newFakeStore(nil)
	var calls []string
	m := newTestManager(store, nil, &calls, nil, t)
	if _, err := m.handleSave(context.Background(), map[string]any{
		"instance_id": "inst-1",
		"entries":     map[string]any{KeyCodegraphEnabled: "false", KeyVftsEnabled: "false"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("两引擎均关闭时不应下发引擎，实际 %v", calls)
	}
}

// TestHandleSaveEngineErrorExplicit：引擎下发失败 → 配置已落库 + 明确错误。
func TestHandleSaveEngineErrorExplicit(t *testing.T) {
	store := newFakeStore(nil)
	m := newTestManager(store, nil, nil, nil, t)
	m.engine = func(context.Context, string, map[string]any) (any, error) {
		return nil, errors.New("engine down")
	}
	_, err := m.handleSave(context.Background(), map[string]any{
		"instance_id": "inst-1",
		"entries":     map[string]any{KeyVftsEnabled: "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "引擎下发失败") {
		t.Fatalf("应返回「引擎下发失败」明确错误，实际 %v", err)
	}
	if v, ok := store.value("inst-1", KeyVftsEnabled); !ok || v != "true" {
		t.Error("引擎失败不应回滚已落库的配置")
	}
}

// TestHandleSaveWithoutWorkdir：无法确定工作区 → 明确错误（不写配置）。
func TestHandleSaveWithoutWorkdir(t *testing.T) {
	store := newFakeStore(nil)
	m := newTestManager(store, map[string]string{}, nil, nil, t)
	_, err := m.handleSave(context.Background(), map[string]any{
		"instance_id": "inst-1",
		"entries":     map[string]any{KeyVftsEnabled: "true"},
	})
	if err == nil || !strings.Contains(err.Error(), "工作区未确定") {
		t.Fatalf("期望「工作区未确定」错误，实际 %v", err)
	}
	if len(store.data["inst-1"]) != 0 {
		t.Error("校验失败不应写任何键")
	}
}

// ─── 校验 ────────────────────────────────────────────────────────

// TestHandleSaveValidation：非法值被拒绝且错误明确（不静默截断 / 不改写）。
func TestHandleSaveValidation(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value any
		want  string
	}{
		{"布尔非法", KeyVftsEnabled, "yes", "必须是 \"true\"/\"false\""},
		{"扩展名缺前缀", KeyCodegraphExts, "go,ts", "非法"},
		{"扩展名非法字符", KeyVftsExts, ".go/x", "非法"},
		{"文档上限越界", KeyVftsDocMaxMB, "0", "1–4096"},
		{"文档上限非数字", KeyVftsDocMaxMB, "abc", "1–4096"},
		{"未知键", "vfts.exts", ".md", "未知配置键"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(nil)
			m := newTestManager(store, nil, nil, nil, t)
			_, err := m.handleSave(context.Background(), map[string]any{
				"instance_id": "inst-1",
				"entries":     map[string]any{tc.key: tc.value},
			})
			if err == nil || !strings.Contains(err.Error(), "config_invalid") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("期望 config_invalid 且含 %q，实际 %v", tc.want, err)
			}
			if len(store.data["inst-1"]) != 0 {
				t.Error("校验失败不应写任何键")
			}
		})
	}
}

// TestNormalizeValueAccepts：合法值原样保留（扩展名/规则格式不改写）。
func TestNormalizeValueAccepts(t *testing.T) {
	got, err := normalizeValue(KeyVftsSkipDirs, " node_modules , !dist/ \n .git/")
	if err != nil {
		t.Fatalf("normalizeValue: %v", err)
	}
	if got != "node_modules , !dist/ \n .git/" {
		t.Errorf("skip-dirs 原样保留（仅去首尾空白），got %q", got)
	}
	if v, err := normalizeValue(KeyVftsDocMaxMB, 64); err != nil || v != "64" {
		t.Errorf("doc-max-mb 数字入参 = %q, %v", v, err)
	}
	if v, err := normalizeValue(KeyCodegraphStackGitignore, true); err != nil || v != "true" {
		t.Errorf("bool 入参 = %q, %v", v, err)
	}
}

// ─── load ────────────────────────────────────────────────────────

// TestHandleLoadDefaults：缺键 → 内置默认；已存在 → 回显原值 + explicit 清单。
func TestHandleLoadDefaults(t *testing.T) {
	store := newFakeStore(map[string]string{
		KeyVftsEnabled: "true",
		KeyVftsExts:    ".md",
	})
	m := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)
	res, err := m.handleLoad(context.Background(), map[string]any{"instance_id": "inst-1"})
	if err != nil {
		t.Fatalf("handleLoad: %v", err)
	}
	out := res.(map[string]any)
	values, _ := out["values"].(map[string]string)
	if values[KeyVftsEnabled] != "true" || values[KeyVftsExts] != ".md" {
		t.Errorf("已配置值回显错误：%v", values)
	}
	if values[KeyVftsDocMaxMB] != "50" {
		t.Errorf("doc-max-mb 默认应为 50，got %q", values[KeyVftsDocMaxMB])
	}
	if values[KeyCodegraphEnabled] != "false" {
		t.Errorf("codegraph 默认应关闭，got %q", values[KeyCodegraphEnabled])
	}
	if out["workdir"] != `E:\proj` {
		t.Errorf("workdir = %v", out["workdir"])
	}
	explicit, _ := out["explicit"].([]string)
	if len(explicit) != 2 {
		t.Errorf("explicit = %v，期望 2 个键", explicit)
	}
}

// ─── 配置刷新（外部写入）热生效 ──────────────────────────────────

// TestOnPrjConfigRefreshApplies：刷新广播命中本命名空间 → 重读并下发引擎。
func TestOnPrjConfigRefreshApplies(t *testing.T) {
	store := newFakeStore(map[string]string{KeyVftsEnabled: "true"})
	var calls []string
	m := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, &calls, nil, t)

	wait := make(chan struct{})
	m.engine = func(_ context.Context, name string, _ map[string]any) (any, error) {
		calls = append(calls, name)
		close(wait)
		return map[string]any{"ok": true}, nil
	}
	m.onPrjConfigRefresh([]byte(`{"instance_id":"inst-1","id":"plugins.codebase.vfts.enabled","op":"save"}`))
	<-wait
	if len(calls) != 1 || calls[0] != "vfts_configure" {
		t.Fatalf("引擎调用 = %v", calls)
	}
}

// TestOnPrjConfigRefreshIgnoresOtherKeys：无关键的刷新不触发引擎下发。
func TestOnPrjConfigRefreshIgnoresOtherKeys(t *testing.T) {
	store := newFakeStore(map[string]string{KeyVftsEnabled: "true"})
	called := false
	m := newTestManager(store, map[string]string{"inst-1": `E:\proj`}, nil, nil, t)
	m.engine = func(context.Context, string, map[string]any) (any, error) {
		called = true
		return nil, nil
	}
	m.onPrjConfigRefresh([]byte(`{"instance_id":"inst-1","id":"memory.enabled","op":"save"}`))
	if called {
		t.Fatal("无关键不应触发引擎下发")
	}
}

// ─── 打开项目（instance-register）即迁移 + 下发 ────────────────────

// TestOnInstanceRegisterMigratesAndApplies：实例登记 → 记录工作区 + 迁移旧键 + 按配置下发引擎。
func TestOnInstanceRegisterMigratesAndApplies(t *testing.T) {
	store := newFakeStore(map[string]string{"enable-vfts": "true"})
	done := make(chan map[string]any, 1)
	m := newTestManager(store, map[string]string{}, nil, nil, t)
	m.engine = func(_ context.Context, name string, a map[string]any) (any, error) {
		if name == "vfts_configure" {
			done <- a
		}
		return nil, nil
	}
	m.onInstanceRegister([]byte(`{"instance_id":"inst-1","work_dir":"E:\\proj"}`))
	select {
	case args := <-done:
		if args["workdir"] != `E:\proj` {
			t.Errorf("workdir = %v", args["workdir"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("instance-register 未触发引擎下发")
	}
	if v, ok := store.value("inst-1", KeyVftsEnabled); !ok || v != "true" {
		t.Errorf("实例登记后应完成旧键迁移：got %q(存在=%v)", v, ok)
	}
}

// ─── 配置页守卫：保存只走动作面，不直调引擎 configure ──────────────

func TestConfigPageUsesAPISave(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("webui", "config.html"))
	if err != nil {
		t.Fatalf("读 config.html: %v", err)
	}
	src := string(raw)
	for _, banned := range []string{"codegraph_configure", "vfts_configure"} {
		if strings.Contains(src, "'"+banned+"'") || strings.Contains(src, `"`+banned+`"`) {
			t.Errorf("config.html 不应直调引擎管理工具 %s（配置须经 /api/save → prj → 插件）", banned)
		}
	}
	for _, want := range []string{"/plugins/codebase/api/", "X-CK-Plugin-Id", "plugins.codebase.codegraph.enabled", "plugins.codebase.vfts.enabled"} {
		if !strings.Contains(src, want) {
			t.Errorf("config.html 缺少 %q", want)
		}
	}
}
