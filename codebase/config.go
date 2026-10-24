// config.go —— codebase 插件配置面（PLUGIN-UI-1 第三段）。
//
// 唯一链路（28-plugins §3A.5「单源不双写」）：
//
//	插件页 webui/config.html → POST /plugins/codebase/api/{load,save}
//	  → 宿主动作面（plugin.ui.rpc）→ 本文件 → plugin/v1/config.project.{get,set,delete}
//	  → prj 配置（plugins.codebase.*）→ 本插件比对配置变化 → 引擎 vfts_configure / codegraph_configure
//
// 约定：
//   - 用户配置键一律 `plugins.codebase.<引擎>.<项>`；**旧宿主键一次性迁入后不再读取**
//     （legacyKeyMap 是唯一允许出现旧键字面量的位置，迁移函数独占）。
//   - 校验归插件：非法取值 → 明确错误（`config_invalid: …`），不静默截断/改值。
//   - 兜底归插件：缺键 → 内置默认（拿不到刷新广播也按默认工作）。
//   - 运行时状态键（codegraph.status / vfts.status）不是用户配置，保持原名、不迁移。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// ─── 键命名空间（唯一源）────────────────────────────────────────────
const (
	KeyCodegraphEnabled        = "plugins.codebase.codegraph.enabled"
	KeyCodegraphExts           = "plugins.codebase.codegraph.exts"
	KeyCodegraphSkipDirs       = "plugins.codebase.codegraph.skip-dirs"
	KeyCodegraphStackGitignore = "plugins.codebase.codegraph.stack-gitignore"

	KeyVftsEnabled        = "plugins.codebase.vfts.enabled"
	KeyVftsExts           = "plugins.codebase.vfts.exts"
	KeyVftsSkipDirs       = "plugins.codebase.vfts.skip-dirs"
	KeyVftsStackGitignore = "plugins.codebase.vfts.stack-gitignore"
	KeyVftsDocs           = "plugins.codebase.vfts.docs"
	KeyVftsDocMaxMB       = "plugins.codebase.vfts.doc-max-mb"
)

// defaultVftsDocMaxMB / maxVftsDocMaxMB 与引擎默认（50MiB）及输入校验区间一致。
const (
	defaultVftsDocMaxMB = 50
	maxVftsDocMaxMB     = 4096
)

// allKeys 是本插件的用户配置键全集（稳定序；load 恒回显这些键的有效值）。
var allKeys = []string{
	KeyCodegraphEnabled, KeyCodegraphExts, KeyCodegraphSkipDirs, KeyCodegraphStackGitignore,
	KeyVftsEnabled, KeyVftsExts, KeyVftsSkipDirs, KeyVftsStackGitignore, KeyVftsDocs, KeyVftsDocMaxMB,
}

// legacyKeyMap 旧宿主键 → 新命名空间键（**迁移专用**；除 migrateOne 外任何代码不得读旧键）。
var legacyKeyMap = []struct{ Old, New string }{
	{"enable-codegraph", KeyCodegraphEnabled},
	{"codegraph.exts", KeyCodegraphExts},
	{"codegraph.skip-dirs", KeyCodegraphSkipDirs},
	{"codegraph.stack-gitignore", KeyCodegraphStackGitignore},
	{"enable-vfts", KeyVftsEnabled},
	{"vfts.exts", KeyVftsExts},
	{"vfts.skip-dirs", KeyVftsSkipDirs},
	{"vfts.stack-gitignore", KeyVftsStackGitignore},
	{"vfts.docs", KeyVftsDocs},
	{"vfts.doc-max-mb", KeyVftsDocMaxMB},
}

// pluginInstancePrefix 是宿主分配给插件的实例 id 前缀（见 pluginbridge.newPluginInstanceID）：
// 解析「当前项目实例」时须排除，否则会把插件自身当成项目。
const pluginInstancePrefix = "plugin:"

// ─── 配置模型 ────────────────────────────────────────────────────

// engineSettings 单引擎的可配置项。
type engineSettings struct {
	Enabled        bool
	Exts           string
	SkipDirs       string
	StackGitignore bool
}

// codebaseSettings 是两引擎 + 文档索引的完整配置快照（内存视图，落盘形态 = allKeys 的字符串 KV）。
type codebaseSettings struct {
	Codegraph    engineSettings
	Vfts         engineSettings
	VftsDocs     bool
	VftsDocMaxMB int
}

// defaultSettings 缺键兜底（与实际生效值保持一致：引擎开关默认关、扩展名/排除规则空 = 引擎默认集、
// 叠加 gitignore 默认关、文档索引默认关、单文件上限 50MB）。
func defaultSettings() codebaseSettings {
	return codebaseSettings{VftsDocMaxMB: defaultVftsDocMaxMB}
}

// settingsOf 由「已存在的 KV 键」构造配置快照（缺失键按默认）。
func settingsOf(values map[string]string) codebaseSettings {
	s := defaultSettings()
	s.Codegraph.Enabled = values[KeyCodegraphEnabled] == "true"
	s.Codegraph.Exts = values[KeyCodegraphExts]
	s.Codegraph.SkipDirs = values[KeyCodegraphSkipDirs]
	s.Codegraph.StackGitignore = values[KeyCodegraphStackGitignore] == "true"
	s.Vfts.Enabled = values[KeyVftsEnabled] == "true"
	s.Vfts.Exts = values[KeyVftsExts]
	s.Vfts.SkipDirs = values[KeyVftsSkipDirs]
	s.Vfts.StackGitignore = values[KeyVftsStackGitignore] == "true"
	s.VftsDocs = values[KeyVftsDocs] == "true"
	if raw := strings.TrimSpace(values[KeyVftsDocMaxMB]); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= maxVftsDocMaxMB {
			s.VftsDocMaxMB = n
		}
	}
	return s
}

// values 把快照展开成 `键 → 字符串` 的有效值（load 回显用）。
func (s codebaseSettings) values() map[string]string {
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return map[string]string{
		KeyCodegraphEnabled:        b(s.Codegraph.Enabled),
		KeyCodegraphExts:           s.Codegraph.Exts,
		KeyCodegraphSkipDirs:       s.Codegraph.SkipDirs,
		KeyCodegraphStackGitignore: b(s.Codegraph.StackGitignore),
		KeyVftsEnabled:             b(s.Vfts.Enabled),
		KeyVftsExts:                s.Vfts.Exts,
		KeyVftsSkipDirs:            s.Vfts.SkipDirs,
		KeyVftsStackGitignore:      b(s.Vfts.StackGitignore),
		KeyVftsDocs:                b(s.VftsDocs),
		KeyVftsDocMaxMB:            strconv.Itoa(s.VftsDocMaxMB),
	}
}

// canonical 规范化 JSON（配置变化比对用；同值重复保存不下发引擎）。
func (s codebaseSettings) canonical() string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ─── prj 配置存取（可替换：生产 = 宿主 UDS 能力面，测试 = 内存桩）─────────

// prjStore 是本插件对项目配置面的最小依赖。
type prjStore interface {
	// Get 读单键；键不存在/空值 → ("", false)。
	Get(ctx context.Context, instanceID, key string) (string, bool, error)
	// Set 批量写键（值恒为字符串）。
	Set(ctx context.Context, instanceID string, entries map[string]string) error
	// Delete 删单键（不存在视为成功）。
	Delete(ctx context.Context, instanceID, key string) error
}

// hostPrjStore 经 `plugin/v1/config.project.*` 读写 prj 配置（统一由 data 层管理，不落插件自有文件）。
type hostPrjStore struct{ p *pluginsdk.Plugin }

func (s hostPrjStore) Get(ctx context.Context, instanceID, key string) (string, bool, error) {
	raw, err := s.p.Call(ctx, "plugin/v1/config.project.get", map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"id": key},
	})
	if err != nil {
		return "", false, err
	}
	var out struct {
		Data *string `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false, fmt.Errorf("config.project.get 应答解析失败: %w", err)
	}
	if out.Data == nil || *out.Data == "" {
		return "", false, nil
	}
	return *out.Data, true, nil
}

func (s hostPrjStore) Set(ctx context.Context, instanceID string, entries map[string]string) error {
	rawEntries := make(map[string]any, len(entries))
	for k, v := range entries {
		rawEntries[k] = v
	}
	_, err := s.p.Call(ctx, "plugin/v1/config.project.set", map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"entries": rawEntries},
	})
	return err
}

func (s hostPrjStore) Delete(ctx context.Context, instanceID, key string) error {
	_, err := s.p.Call(ctx, "plugin/v1/config.project.delete", map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"id": key},
	})
	return err
}

// engineCall 是引擎管理工具调用（默认为进程内直调 codegraph / vfts 引擎；测试注入假引擎）。
type engineCall func(ctx context.Context, name string, args map[string]any) (any, error)

// ─── 配置管理器 ───────────────────────────────────────────────────

// configManager 承载配置动作、迁移、实例/工作区跟踪与引擎下发。
type configManager struct {
	p      *pluginsdk.Plugin
	store  prjStore
	engine engineCall
	logf   func(format string, args ...any)

	mu       sync.Mutex
	workdirs map[string]string // instance_id → work_dir（来源：instance-register 事件）
	applied  map[string]string // instance_id → 最近一次成功下发引擎的配置（canonical JSON）
}

// newConfigManager 构造配置管理器（store / engine 走生产实现）。
func newConfigManager(p *pluginsdk.Plugin) *configManager {
	return &configManager{
		p:     p,
		store: hostPrjStore{p: p},
		engine: func(ctx context.Context, name string, args map[string]any) (any, error) {
			return callTool(ctx, name, args)
		},
		logf: func(format string, args ...any) {
			slog.Info(fmt.Sprintf(format, args...))
		},
		workdirs: map[string]string{},
		applied:  map[string]string{},
	}
}

// Actions 返回本插件承担 UI 动作名（宿主 `POST /plugins/codebase/api/<action>`）。
func (m *configManager) Actions() map[string]pluginsdk.RequestFunc {
	return map[string]pluginsdk.RequestFunc{
		"load": m.handleLoad,
		"save": m.handleSave,
		// clear 是**维护动作**（清除索引产物，不改配置）：同样经插件转引擎，页面不直调引擎管理工具。
		"clear": m.handleClear,
	}
}

// handleClear 清除某工作区的 codegraph 索引产物（配置存档保留）。
func (m *configManager) handleClear(ctx context.Context, payload map[string]any) (any, error) {
	inst, err := m.resolveInstance(ctx, payload)
	if err != nil {
		return nil, err
	}
	workdir, err := m.resolveWorkdir(inst, payload)
	if err != nil {
		return nil, err
	}
	if _, err := m.engine(ctx, "codegraph_configure", map[string]any{
		"workdir": workdir,
		"mode":    "clear",
	}); err != nil {
		return nil, fmt.Errorf("清除索引失败：%w", err)
	}
	m.logf("codebase: 已清除 codegraph 索引（instance=%s，workdir=%s）", inst, workdir)
	return map[string]any{"ok": true, "instance_id": inst, "workdir": workdir}, nil
}

// ─── 动作：load ──────────────────────────────────────────────────

// handleLoad 回显有效配置（缺键按默认）；入参可带 instance_id / workdir 用于定位项目。
func (m *configManager) handleLoad(ctx context.Context, payload map[string]any) (any, error) {
	inst, err := m.resolveInstance(ctx, payload)
	if err != nil {
		return nil, err
	}
	stored, err := m.readValues(ctx, inst)
	if err != nil {
		return nil, err
	}
	explicit := make([]string, 0, len(stored))
	for _, k := range allKeys {
		if _, ok := stored[k]; ok {
			explicit = append(explicit, k)
		}
	}
	return map[string]any{
		"ok":          true,
		"instance_id": inst,
		"workdir":     m.workdirOf(inst),
		"values":      settingsOf(stored).values(),
		"explicit":    explicit,
	}, nil
}

// ─── 动作：save ──────────────────────────────────────────────────

// handleSave 校验 → 写 prj（plugins.codebase.*）→ 回读 → 下发引擎。
//
// 入参：`{instance_id?, workdir?, entries:{<plugins.codebase.* 键>: <值>}}`（可只提交部分键）。
// 校验失败 → 明确错误（不写任何键）。写入成功后引擎下发失败 → 配置已落库并返回明确错误
// （刷新广播会再试一次；页面提示「已保存，引擎应用失败」）。
func (m *configManager) handleSave(ctx context.Context, payload map[string]any) (any, error) {
	inst, err := m.resolveInstance(ctx, payload)
	if err != nil {
		return nil, err
	}
	rawEntries, err := saveEntries(payload)
	if err != nil {
		return nil, err
	}
	norm := make(map[string]string, len(rawEntries))
	keys := make([]string, 0, len(rawEntries))
	for k, v := range rawEntries {
		if !isKnownKey(k) {
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s（只接受 plugins.codebase.* 命名空间）", k)
		}
		val, err := normalizeValue(k, v)
		if err != nil {
			return nil, err
		}
		norm[k] = val
		keys = append(keys, k)
	}
	sort.Strings(keys)

	workdir, err := m.resolveWorkdir(inst, payload)
	if err != nil {
		return nil, err
	}
	if err := m.store.Set(ctx, inst, norm); err != nil {
		return nil, fmt.Errorf("配置写入失败（instance=%s）：%w", inst, err)
	}
	// 以存储为准回读（未提交的键沿用既有值），再比对/下发引擎。
	stored, err := m.readValues(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("配置已写入，但回读失败：%w", err)
	}
	applied, err := m.apply(ctx, inst, workdir, settingsOf(stored))
	if err != nil {
		return nil, fmt.Errorf("配置已写入，但引擎下发失败：%w", err)
	}
	m.logf("codebase: 配置已保存（instance=%s，keys=%s，引擎下发=%v）", inst, strings.Join(keys, ","), applied)
	return map[string]any{
		"ok":          true,
		"instance_id": inst,
		"keys":        keys,
		"applied":     applied,
	}, nil
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

// ─── 校验 ────────────────────────────────────────────────────────

// isKnownKey 判定是否本命名空间的用户配置键。
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
	case KeyCodegraphEnabled, KeyCodegraphStackGitignore, KeyVftsEnabled, KeyVftsStackGitignore, KeyVftsDocs:
		s := strings.ToLower(strings.TrimSpace(pluginsdk.Str(raw)))
		switch s {
		case "true", "false":
			return s, nil
		}
		return "", fmt.Errorf("config_invalid: %s 必须是 \"true\"/\"false\"（收到 %q）", key, pluginsdk.Str(raw))
	case KeyCodegraphExts, KeyVftsExts:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		for _, item := range splitList(s) {
			if err := validateExt(key, item); err != nil {
				return "", err
			}
		}
		return s, nil
	case KeyCodegraphSkipDirs, KeyVftsSkipDirs:
		s := strings.TrimSpace(pluginsdk.Str(raw))
		if len(s) > 8192 {
			return "", fmt.Errorf("config_invalid: %s 过长（上限 8192 字符，收到 %d）", key, len(s))
		}
		for _, rule := range splitRules(s) {
			if len(rule) > 512 {
				return "", fmt.Errorf("config_invalid: %s 单条规则过长（上限 512 字符）", key)
			}
			if strings.ContainsRune(rule, 0) {
				return "", fmt.Errorf("config_invalid: %s 含非法字符 NUL", key)
			}
		}
		return s, nil
	case KeyVftsDocMaxMB:
		n, err := strconv.Atoi(strings.TrimSpace(pluginsdk.Str(raw)))
		if err != nil || n < 1 || n > maxVftsDocMaxMB {
			return "", fmt.Errorf("config_invalid: %s 必须是 1–%d 的整数（收到 %q）", key, maxVftsDocMaxMB, pluginsdk.Str(raw))
		}
		return strconv.Itoa(n), nil
	}
	return "", fmt.Errorf("config_invalid: 未知配置键 %s", key)
}

// validateExt 校验单个扩展名（须以 '.' 开头、2–32 字符、仅字母数字与 . _ -）。
func validateExt(key, item string) error {
	if !strings.HasPrefix(item, ".") || len(item) < 2 || len(item) > 32 {
		return fmt.Errorf("config_invalid: %s 的扩展名 %q 非法（须以 . 开头、长度 2–32）", key, item)
	}
	for _, r := range item[1:] {
		ok := r == '.' || r == '_' || r == '-' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return fmt.Errorf("config_invalid: %s 的扩展名 %q 含非法字符 %q", key, item, string(r))
		}
	}
	return nil
}

// splitList 解析扩展名清单（逗号/分号/换行分隔；去空去重、保序）。
func splitList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

// splitRules 解析排除规则（逗号/分号/换行分隔；去空、**保序保重复**——gitignore 顺序有意义）。
func splitRules(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// ─── 实例 / 工作区解析 ────────────────────────────────────────────

// hostInstances 取当前宿主实例 id（排除插件自身实例）。
func (m *configManager) hostInstances(ctx context.Context) ([]string, error) {
	raw, err := m.p.Call(ctx, "plugin/v1/instance.list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("读取实例列表失败：%w", err)
	}
	var out struct {
		Instances []string `json:"instances"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("实例列表应答解析失败：%w", err)
	}
	ids := make([]string, 0, len(out.Instances))
	for _, id := range out.Instances {
		if id == "" || strings.HasPrefix(id, pluginInstancePrefix) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// resolveInstance 定位项目实例：显式 instance_id → workdir 匹配 → 唯一实例。
func (m *configManager) resolveInstance(ctx context.Context, payload map[string]any) (string, error) {
	if id := strings.TrimSpace(pluginsdk.Str(payload["instance_id"])); id != "" {
		return id, nil
	}
	if wd := normalizeWorkdir(pluginsdk.Str(payload["workdir"])); wd != "" {
		m.mu.Lock()
		hit := ""
		for id, w := range m.workdirs {
			if samePath(w, wd) && (hit == "" || id < hit) {
				hit = id
			}
		}
		m.mu.Unlock()
		if hit != "" {
			return hit, nil
		}
	}
	ids, err := m.hostInstances(ctx)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", errors.New("未找到项目实例：请显式提供 instance_id")
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("当前有 %d 个项目实例，请显式提供 instance_id 或工作区", len(ids))
}

// resolveWorkdir 定位项目根目录：入参 workdir → 实例登记时记录的 work_dir。
func (m *configManager) resolveWorkdir(inst string, payload map[string]any) (string, error) {
	wd := normalizeWorkdir(pluginsdk.Str(payload["workdir"]))
	if wd == "" {
		wd = m.workdirOf(inst)
	}
	if wd == "" {
		return "", errors.New("工作区未确定：请填写项目根目录（或重新打开项目窗口后重试）")
	}
	if !filepath.IsAbs(wd) {
		return "", fmt.Errorf("config_invalid: workdir 必须是绝对路径（收到 %q）", wd)
	}
	return wd, nil
}

// workdirOf 返回实例登记的工作区（未知 → ""）。
func (m *configManager) workdirOf(inst string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workdirs[inst]
}

// normalizeWorkdir 归一工作区路径（空 → ""；否则 Clean）。
func normalizeWorkdir(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	return filepath.Clean(s)
}

// samePath 比较两个已 Clean 的路径（Windows 大小写不敏感）。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ─── 读配置 / 下发引擎 ────────────────────────────────────────────

// readValues 读全部用户配置键（只返回**已存在**的键；缺失 = 调用方按默认）。
func (m *configManager) readValues(ctx context.Context, inst string) (map[string]string, error) {
	out := make(map[string]string, len(allKeys))
	for _, key := range allKeys {
		v, ok, err := m.store.Get(ctx, inst, key)
		if err != nil {
			return nil, fmt.Errorf("读取配置 %s 失败（instance=%s）：%w", key, inst, err)
		}
		if ok {
			out[key] = v
		}
	}
	return out, nil
}

// apply 把配置下发引擎（只下发**启用中**的引擎；同实例同值不重复下发）。
//
// 触发点 = ① 插件页保存；② prj 配置刷新广播（别的写入方，如场景向导）；③ 实例登记（打开项目）。
func (m *configManager) apply(ctx context.Context, inst, workdir string, s codebaseSettings) ([]string, error) {
	canon := s.canonical()
	m.mu.Lock()
	prev, seen := m.applied[inst]
	m.mu.Unlock()
	if seen && prev == canon {
		return nil, nil // 配置未变化 → 幂等（不重复下发）
	}
	applied := []string{}
	if s.Codegraph.Enabled {
		if _, err := m.engine(ctx, "codegraph_configure", map[string]any{
			"workdir":         workdir,
			"enabled":         true,
			"exts":            splitList(s.Codegraph.Exts),
			"skip_dirs":       splitRules(s.Codegraph.SkipDirs),
			"stack_gitignore": s.Codegraph.StackGitignore,
		}); err != nil {
			return applied, fmt.Errorf("codegraph_configure: %w", err)
		}
		applied = append(applied, "codegraph")
	}
	if s.Vfts.Enabled {
		if _, err := m.engine(ctx, "vfts_configure", map[string]any{
			"workdir":         workdir,
			"enabled":         true,
			"exts":            splitList(s.Vfts.Exts),
			"skip_dirs":       splitRules(s.Vfts.SkipDirs),
			"stack_gitignore": s.Vfts.StackGitignore,
			"docs":            s.VftsDocs,
			"doc_max_bytes":   int64(s.VftsDocMaxMB) << 20,
			"doc_cache_dir":   filepath.Join(workdir, ".chonkpilot", "vfts", "doc_text"),
		}); err != nil {
			return applied, fmt.Errorf("vfts_configure: %w", err)
		}
		applied = append(applied, "vfts")
	}
	m.mu.Lock()
	m.applied[inst] = canon
	m.mu.Unlock()
	return applied, nil
}

// ─── 一次性迁移（旧宿主键 → plugins.codebase.*）─────────────────────

// migrateInstance 把单个实例上的旧键迁入命名空间：
//   - 新键已存在 → 忽略旧键（单源；旧键不再被任何代码读取）；
//   - 旧键存在且新键缺失 → 写新键 + 删旧键；
//   - 任一步失败 → 保留旧键（删旧失败时回滚新键），下次启动重试。
//
// 幂等：重复执行不会反复写（新键写入后即跳过）。
func (m *configManager) migrateInstance(ctx context.Context, inst string) int {
	migrated := 0
	for _, lm := range legacyKeyMap {
		_, newOK, err := m.store.Get(ctx, inst, lm.New)
		if err != nil {
			m.logf("codebase: 配置迁移读新键 %s 失败（instance=%s）：%v（保留旧键，下次重试）", lm.New, inst, err)
			continue
		}
		if newOK {
			continue
		}
		oldVal, oldOK, err := m.store.Get(ctx, inst, lm.Old)
		if err != nil {
			m.logf("codebase: 配置迁移读旧键 %s 失败（instance=%s）：%v（下次重试）", lm.Old, inst, err)
			continue
		}
		if !oldOK {
			continue
		}
		if err := m.store.Set(ctx, inst, map[string]string{lm.New: oldVal}); err != nil {
			m.logf("codebase: 配置迁移写新键 %s 失败（instance=%s）：%v（保留旧键，下次重试）", lm.New, inst, err)
			continue
		}
		if err := m.store.Delete(ctx, inst, lm.Old); err != nil {
			// 回滚新键 → 维持「旧键仍在」的单源状态，下次启动整体重试
			// （否则新键已写、旧键永留的中间态会被后续迁移判为「新键已存在」而跳过）。
			if rbErr := m.store.Delete(ctx, inst, lm.New); rbErr != nil {
				m.logf("codebase: 配置迁移回滚新键 %s 失败（instance=%s）：%v（旧键残留，需人工清理）", lm.New, inst, rbErr)
			}
			m.logf("codebase: 配置迁移删除旧键 %s 失败（instance=%s）：%v（已回滚新键，下次重试）", lm.Old, inst, err)
			continue
		}
		migrated++
		m.logf("codebase: 配置迁移 %s → %s（instance=%s）", lm.Old, lm.New, inst)
	}
	if migrated > 0 {
		m.logf("codebase: 配置迁移完成（instance=%s，共 %d 键）", inst, migrated)
	}
	return migrated
}

// migrateAll 对宿主当前全部实例执行一次迁移（启动 / server-ready 时机）。
func (m *configManager) migrateAll(ctx context.Context) {
	ids, err := m.hostInstances(ctx)
	if err != nil {
		m.logf("codebase: 配置迁移跳过（%v）", err)
		return
	}
	for _, inst := range ids {
		m.migrateInstance(ctx, inst)
	}
}

// ─── 事件处理 ────────────────────────────────────────────────────

// onInstanceRegister 记录实例工作区 → 迁移 → 按配置下发引擎（打开项目即生效，不必先开配置页）。
func (m *configManager) onInstanceRegister(payload []byte) {
	var ev struct {
		InstanceID string `json:"instance_id"`
		WorkDir    string `json:"work_dir"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.InstanceID == "" || ev.WorkDir == "" {
		return
	}
	wd := normalizeWorkdir(ev.WorkDir)
	m.mu.Lock()
	m.workdirs[ev.InstanceID] = wd
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		m.migrateInstance(ctx, ev.InstanceID)
		stored, err := m.readValues(ctx, ev.InstanceID)
		if err != nil {
			m.logf("codebase: 读项目配置失败（instance=%s）：%v", ev.InstanceID, err)
			return
		}
		if _, err := m.apply(ctx, ev.InstanceID, wd, settingsOf(stored)); err != nil {
			m.logf("codebase: 项目 %s 配置下发引擎失败：%v", wd, err)
		}
	}()
}

// onInstanceExit 清理实例记录（工作区 / 已下发基线）。
func (m *configManager) onInstanceExit(payload []byte) {
	var ev struct {
		InstanceID string `json:"instance_id"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.InstanceID == "" {
		return
	}
	m.mu.Lock()
	delete(m.workdirs, ev.InstanceID)
	delete(m.applied, ev.InstanceID)
	m.mu.Unlock()
}

// onPrjConfigRefresh 处理 prj 配置变更广播：变更键命中本命名空间 → 重读并下发引擎
// （覆盖「别的写入方」（场景向导 / 手工改库）—— 与插件页保存同一条下发路径）。
func (m *configManager) onPrjConfigRefresh(payload []byte) {
	var ev struct {
		InstanceID string   `json:"instance_id"`
		ID         string   `json:"id"`
		IDs        []string `json:"ids"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	keys := ev.IDs
	if len(keys) == 0 {
		keys = []string{ev.ID}
	}
	hit := false
	for _, k := range keys {
		if isKnownKey(k) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		insts := []string{}
		m.mu.Lock()
		if ev.InstanceID != "" {
			insts = append(insts, ev.InstanceID)
		} else {
			for id := range m.workdirs {
				insts = append(insts, id)
			}
		}
		m.mu.Unlock()
		sort.Strings(insts)
		for _, inst := range insts {
			wd := m.workdirOf(inst)
			if wd == "" {
				continue
			}
			stored, err := m.readValues(ctx, inst)
			if err != nil {
				m.logf("codebase: 配置刷新读取失败（instance=%s）：%v", inst, err)
				continue
			}
			if _, err := m.apply(ctx, inst, wd, settingsOf(stored)); err != nil {
				m.logf("codebase: 配置刷新下发引擎失败（instance=%s）：%v", inst, err)
			}
		}
	}()
}
