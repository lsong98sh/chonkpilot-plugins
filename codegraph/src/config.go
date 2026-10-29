// config.go —— codegraph 插件配置面（PLUGIN-UI-1 第三段）。
//
// 唯一链路（28-plugins §3A.5「单源不双写」）：
//
//	插件页 webui/config.html → POST /plugins/codegraph/api/{load,save}
//	  → 宿主动作面（plugin.ui.rpc）→ 本文件 → plugin/v1/config.project.{get,set,delete}
//	  → prj 配置（plugins.codegraph.*）→ 本插件比对配置变化 → 引擎 vfts_configure / codegraph_configure
//
// 约定：
//   - 用户配置键一律 `plugins.codegraph.<引擎>.<项>`；**旧宿主键一次性迁入后不再读取**
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
	"github.com/lsong98sh/chonkpilot-util/configkit"
	"github.com/lsong98sh/chonkpilot-util/pathx"
	"github.com/lsong98sh/chonkpilot-util/strlist"
)

// ─── 键命名空间（唯一源）────────────────────────────────────────────
const (
	KeyCodegraphEnabled        = "plugins.codegraph.enabled"
	KeyCodegraphExts           = "plugins.codegraph.exts"
	KeyCodegraphSkipDirs       = "plugins.codegraph.skip-dirs"
	KeyCodegraphStackGitignore = "plugins.codegraph.stack-gitignore"

	KeyVftsEnabled        = "plugins.codegraph.vfts.enabled"
	KeyVftsExts           = "plugins.codegraph.vfts.exts"
	KeyVftsSkipDirs       = "plugins.codegraph.vfts.skip-dirs"
	KeyVftsStackGitignore = "plugins.codegraph.vfts.stack-gitignore"
	KeyVftsDocs           = "plugins.codegraph.vfts.docs"
	KeyVftsDocMaxMB       = "plugins.codegraph.vfts.doc-max-mb"
)

// defaultVftsDocMaxMB / maxVftsDocMaxMB 与引擎默认（50MiB）及输入校验区间一致。
const (
	defaultVftsDocMaxMB = 50
	maxVftsDocMaxMB     = 4096

	// rebuildDebounce 配置生效后自动重建索引的去抖窗口（合并一次保存里的多键变更；
	// 与旧内嵌 hook 的 400ms 同口径）。rebuildTimeout 覆盖大项目全量索引耗时。
	rebuildDebounce = 400 * time.Millisecond
	rebuildTimeout  = 15 * time.Minute
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

// codegraphSettings 是两引擎 + 文档索引的完整配置快照（内存视图，落盘形态 = allKeys 的字符串 KV）。
type codegraphSettings struct {
	Codegraph    engineSettings
	Vfts         engineSettings
	VftsDocs     bool
	VftsDocMaxMB int
}

// defaultSettings 缺键兜底（与实际生效值保持一致：引擎开关默认关、扩展名/排除规则空 = 引擎默认集、
// 叠加 gitignore 默认关、文档索引默认关、单文件上限 50MB）。
func defaultSettings() codegraphSettings {
	return codegraphSettings{VftsDocMaxMB: defaultVftsDocMaxMB}
}

// settingsOf 由「已存在的 KV 键」构造配置快照（缺失键按默认）。
func settingsOf(values map[string]string) codegraphSettings {
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
func (s codegraphSettings) values() map[string]string {
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
func (s codegraphSettings) canonical() string {
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

	// applyMu 串行化「读配置 → 下发引擎 → 记 applied」全过程（2026-10-26）。
	// 背景：`onPrjConfigRefresh` 每个事件起一个 goroutine，读与下发之间存在间隙 →
	// 后到的**旧快照**可能覆盖刚下发的新配置（表现为引擎按旧 skip-dirs 建索引，而 prj 回读/status
	// 已是新值）。串行化后每次都在锁内重新读取，旧事件读到的是最新值 → canon 相同即幂等跳过。
	applyMu sync.Mutex

	// rebuildTimers 是「配置生效 → 自动重建索引」的去抖计时器（key = engine|workdir）。
	// D-71：主仓内嵌 hook 退场后自动索引归插件（SRCH-004-S01：开启开关即自动开始索引）。
	rebuildMu     sync.Mutex
	rebuildTimers map[string]*time.Timer

	// 文档转换服务探测缓存（TTL 30s；配置下发前强制复探）。
	probeMu  sync.Mutex
	probeAt  time.Time
	probeSvc *docService
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
		workdirs:      map[string]string{},
		applied:       map[string]string{},
		rebuildTimers: map[string]*time.Timer{},
	}
}

// Actions 返回本插件承担 UI 动作名（宿主 `POST /plugins/codegraph/api/<action>`）。
func (m *configManager) Actions() map[string]pluginsdk.RequestFunc {
	return map[string]pluginsdk.RequestFunc{
		"load": m.handleLoad,
		"save": m.handleSave,
		// clear 是**维护动作**（清除索引产物，不改配置）：同样经插件转引擎，页面不直调引擎管理工具。
		"clear": m.handleClear,
		// badge 是**状态栏紧凑视图**的数据源（PLUGIN-UI-2）：索引进度 UI 归插件，
		// 宿主不再读 `codegraph.status` / `vfts.status`。
		"badge": m.handleBadge,
		// engine-status 是**完整状态**读面（配置页 / 状态页 / L4 门禁）：返回两引擎原始状态。
		// PLUGIN-UI-2 起替代旧 prj 状态键（宿主/插件都不再回写 `*.status`）。
		// 注：不能叫 `status` —— 那是插件的**健康/就绪** rpc（main.go `OnRequest("status")`）。
		"engine-status": m.handleStatus,
	}
}

// handleStatus 返回两引擎的**原始状态**（与旧 `*.status` 回写内容同源同形）。
//
// payload 支持 `instance_id` 或 `workdir`（同 resolveInstance / resolveWorkdir 口径）。
// vfts 额外补 `docsPort`（转换服务端口是**插件探测结果**，引擎状态里没有）。
func (m *configManager) handleStatus(ctx context.Context, payload map[string]any) (any, error) {
	inst, err := m.resolveInstance(ctx, payload)
	if err != nil {
		return nil, err
	}
	wd, err := m.resolveWorkdir(inst, payload)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"workdir": wd, "codegraph": nil, "vfts": nil}
	if wd == "" {
		return out, nil
	}
	// `engine` 可选：只查一个引擎（门禁轮询高频调用，避免每次多打一次引擎 + docs 探测）。
	only := strings.TrimSpace(pluginsdk.Str(payload["engine"]))
	engines := []string{"codegraph", "vfts"}
	if only == "codegraph" || only == "vfts" {
		engines = []string{only}
	}
	for _, eng := range engines {
		v, err := m.engine(ctx, eng+"_status", map[string]any{"workdir": wd})
		if err != nil {
			out[eng+"Error"] = err.Error()
			continue
		}
		if eng == "vfts" {
			// 引擎状态无 docsPort（转换服务端口是**插件探测结果**）→ 透出（2B §142 既有口径）。
			// 走一次 JSON 往返而不是就地改 map：引擎应答的具体类型不保证是可写的 map[string]any。
			if svc := m.probeDocsService(false); svc != nil {
				if b, err := json.Marshal(v); err == nil {
					var obj map[string]any
					if json.Unmarshal(b, &obj) == nil {
						obj["docsPort"] = svc.Port
						v = obj
					}
				}
			}
		}
		out[eng] = v
	}
	return out, nil
}

// ─── 动作：badge（状态栏紧凑视图，PLUGIN-UI-2）────────────────────

// badgeEngine 单引擎的栏内紧凑状态（由插件 badge 视图直接渲染）。
type badgeEngine struct {
	Engine  string `json:"engine"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Text    string `json:"text"`
	Tone    string `json:"tone"`
}

// handleBadge 供 badge 视图（状态栏 iframe）拉取紧凑进度。
//
// instance_id 由宿主拼进 iframe URL（PLUGIN-INSTANCE-1）→ 反查该项目的 workdir → 读两引擎状态。
// 未打开项目（无 instance 登记）→ engines 为空（视图显示占位）。
func (m *configManager) handleBadge(ctx context.Context, payload map[string]any) (any, error) {
	out := map[string]any{"ok": true, "instance_id": "", "workdir": "", "engines": []badgeEngine{}}
	inst, err := m.resolveInstance(ctx, payload)
	if err != nil {
		// 无活动项目 / 多实例歧义 → 视图显示占位（不把栏内小控件变成报错面）。
		out["reason"] = err.Error()
		return out, nil
	}
	out["instance_id"] = inst
	wd := m.workdirOf(inst)
	out["workdir"] = wd
	if wd == "" {
		return out, nil
	}
	cgEnabled, vftsEnabled, err := m.badgeEnabled(ctx, inst)
	if err != nil {
		return nil, err
	}
	list := make([]badgeEngine, 0, 2)
	for _, e := range []struct {
		name    string
		enabled bool
	}{{"codegraph", cgEnabled}, {"vfts", vftsEnabled}} {
		be := badgeEngine{Engine: e.name, Enabled: e.enabled}
		if !e.enabled {
			be.Text, be.Tone = "未启用", "disabled"
			list = append(list, be)
			continue
		}
		v, err := m.engine(ctx, e.name+"_status", map[string]any{"workdir": wd})
		if err != nil {
			be.Text, be.Tone = "不可用", "error"
			list = append(list, be)
			continue
		}
		be.State = statusString(v, "state")
		be.Done = statusInt(v, "progressDone")
		be.Total = statusInt(v, "progressTotal")
		be.Text, be.Tone = badgeText(be)
		list = append(list, be)
	}
	out["engines"] = list
	return out, nil
}

// badgeEnabled 只读两枚引擎开关（badge 视图 2s 轮询一次）。
//
// 不必为两个布尔值读全套 10 个配置键：`plugin/v1/config.project.get` 每次调用都会在宿主
// 审计日志里留一条记录 —— 旧实现（readValues）每轮询产生 10 条 / 2s 的日志量。
func (m *configManager) badgeEnabled(ctx context.Context, inst string) (cg, vfts bool, err error) {
	read := func(key string) (bool, error) {
		v, ok, err := m.store.Get(ctx, inst, key)
		if err != nil {
			return false, fmt.Errorf("读取配置 %s 失败（instance=%s）：%w", key, inst, err)
		}
		return ok && v == "true", nil
	}
	if cg, err = read(KeyCodegraphEnabled); err != nil {
		return false, false, err
	}
	if vfts, err = read(KeyVftsEnabled); err != nil {
		return false, false, err
	}
	return cg, vfts, nil
}

// badgeText 把引擎状态收敛为栏内文案 + tone（与旧宿主徽标同一展示口径）。
func badgeText(be badgeEngine) (string, string) {
	switch {
	case be.State == "ready":
		return "完成", "ready"
	case be.State == "error":
		return "失败", "error"
	case be.State == "indexing" || be.Total > 0:
		// state 尚未落定为 indexing 但已见进度 → 视为正在索引（引擎建库中途快照）
		if be.Total > 0 {
			return fmt.Sprintf("索引中 %d/%d", be.Done, be.Total), "progress"
		}
		return "索引中", "progress"
	}
	return "未初始化", "disabled"
}

// statusString / statusInt 从引擎 status 应答（map[string]any）取字段。
func statusString(v any, key string) string {
	m, _ := v.(map[string]any)
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func statusInt(v any, key string) int {
	m, _ := v.(map[string]any)
	if m == nil {
		return 0
	}
	switch n := m[key].(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
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
	// PLUGIN-UI-2：不再回写 `codegraph.status`（宿主零引用；状态由本插件 `engine-status` / `badge`
	// 动作现取，见 handleStatus / handleBadge）。
	m.logf("codegraph: 已清除 codegraph 索引（instance=%s，workdir=%s）", inst, workdir)
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

// handleSave 校验 → 写 prj（plugins.codegraph.*）→ 回读 → 下发引擎。
//
// 入参：`{instance_id?, workdir?, entries:{<plugins.codegraph.* 键>: <值>}}`（可只提交部分键）。
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
			return nil, fmt.Errorf("config_invalid: 未知配置键 %s（只接受 plugins.codegraph.* 命名空间）", k)
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
	// 读+下发须在 applyMu 内完成：并发 refresh 事件不能把旧快照盖回引擎。
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	stored, err := m.readValues(ctx, inst)
	if err != nil {
		return nil, fmt.Errorf("配置已写入，但回读失败：%w", err)
	}
	applied, err := m.apply(ctx, inst, workdir, settingsOf(stored))
	if err != nil {
		return nil, fmt.Errorf("配置已写入，但引擎下发失败：%w", err)
	}
	if len(applied) > 0 {
		m.scheduleRebuild(applied, workdir) // 配置变更 → 自动重建索引（SRCH-004-S01）
	}
	m.logf("codegraph: 配置已保存（instance=%s，keys=%s，引擎下发=%v）", inst, strings.Join(keys, ","), applied)
	return map[string]any{
		"ok":          true,
		"instance_id": inst,
		"keys":        keys,
		"applied":     applied,
	}, nil
}

// saveEntries 取动作载荷里的 `entries`（对象形态；缺省 → 明确错误）。
func saveEntries(payload map[string]any) (map[string]any, error) {
	return configkit.SaveEntries(payload)
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
	return strlist.SplitDedup(s)
}

// splitRules 解析排除规则（逗号/分号/换行分隔；去空、**保序保重复**——gitignore 顺序有意义）。
func splitRules(s string) []string {
	return strlist.Split(s)
}

// nonNil 把 nil 切片换成空切片（引擎侧 `getStrings` 的「nil = 不改」与「空数组 = 清空/默认集」
// 语义不同：配置里显式清空必须下发空数组，不能下发 null）。
func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
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
	return pathx.Same(a, b)
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
func (m *configManager) apply(ctx context.Context, inst, workdir string, s codegraphSettings) ([]string, error) {
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
			"exts":            nonNil(splitList(s.Codegraph.Exts)),
			"skip_dirs":       nonNil(splitRules(s.Codegraph.SkipDirs)),
			"stack_gitignore": s.Codegraph.StackGitignore,
		}); err != nil {
			return applied, fmt.Errorf("codegraph_configure: %w", err)
		}
		applied = append(applied, "codegraph")
	}
	if s.Vfts.Enabled {
		// 文档接入字段**恒下发**（含不可用态）：引擎按「键存在即覆盖」应用；服务不可用时下发
		// 空 endpoint/token，明确清掉上次残留（否则引擎会按旧 endpoint 误判服务可用）。
		docSvc := m.probeDocsService(true)
		vargs := map[string]any{
			"workdir":            workdir,
			"enabled":            true,
			"exts":               nonNil(splitList(s.Vfts.Exts)),
			"skip_dirs":          nonNil(splitRules(s.Vfts.SkipDirs)),
			"stack_gitignore":    s.Vfts.StackGitignore,
			"docs":               s.VftsDocs,
			"doc_endpoint":       "",
			"doc_token":          "",
			"doc_max_bytes":      int64(s.VftsDocMaxMB) << 20,
			"doc_text_max_bytes": docTextMaxBytes,
			"doc_cache_dir":      filepath.Join(workdir, ".chonkpilot", "vfts", "doc_text"),
		}
		if docSvc != nil {
			vargs["doc_endpoint"] = docSvc.baseURL()
			vargs["doc_token"] = docSvc.Token
		} else if s.VftsDocs {
			m.logf("codegraph: %s 文档索引已开启但转换服务未运行 → 本次整批跳过文档类", workdir)
		}
		if _, err := m.engine(ctx, "vfts_configure", vargs); err != nil {
			return applied, fmt.Errorf("vfts_configure: %w", err)
		}
		applied = append(applied, "vfts")
	}
	m.mu.Lock()
	m.applied[inst] = canon
	m.mu.Unlock()
	return applied, nil
}

// ─── 配置生效后的自动重建（D-71：主仓内嵌 hook 退场后归插件）──────────

// scheduleRebuild 对已下发配置的引擎安排一次去抖重建（同 engine|workdir 只保留最后一次）。
//
// SRCH-004-S01「开启开关 → 自动开始索引」/ SRCH-005-S01「启用且未索引 → 全量索引」：
// 旧内嵌 hook 的 `ensureWorkspace` 承担该职责，D-71 后必须由插件补齐。
func (m *configManager) scheduleRebuild(engines []string, workdir string) {
	for _, eng := range engines {
		key := eng + "|" + workdir
		m.rebuildMu.Lock()
		if old := m.rebuildTimers[key]; old != nil {
			old.Stop()
		}
		m.rebuildTimers[key] = time.AfterFunc(rebuildDebounce, func() {
			m.rebuildMu.Lock()
			delete(m.rebuildTimers, key)
			m.rebuildMu.Unlock()
			m.rebuild(eng, workdir)
		})
		m.rebuildMu.Unlock()
	}
}

// rebuild 全量重建某引擎索引 → 回写清单（vfts）与运行时状态键。
// 引擎侧自持串行（codegraph 每 workdir、vfts 全局 recMu），与页面「立即索引」并发安全。
func (m *configManager) rebuild(eng, workdir string) {
	ctx, cancel := context.WithTimeout(context.Background(), rebuildTimeout)
	defer cancel()
	name := "codegraph_initialize"
	if eng == "vfts" {
		name = "vfts_index"
	}
	out, err := m.engine(ctx, name, map[string]any{"workdir": workdir})
	if err != nil {
		m.logf("codegraph: 配置生效后自动重建失败（%s %s）：%v", eng, workdir, err)
		return
	}
	if eng == "vfts" {
		writeVftsManifest(m.logf, workdir, out)
	}
}

// engineEnabled 判定快照里该引擎是否启用。
func engineEnabled(s codegraphSettings, eng string) bool {
	if eng == "vfts" {
		return s.Vfts.Enabled
	}
	return s.Codegraph.Enabled
}

// enabledEngines 列出快照里启用的引擎（重建顺序固定：codegraph → vfts）。
func enabledEngines(s codegraphSettings) []string {
	out := make([]string, 0, 2)
	for _, eng := range []string{"codegraph", "vfts"} {
		if engineEnabled(s, eng) {
			out = append(out, eng)
		}
	}
	return out
}

// engineReady 查询该引擎是否已就绪（未初始化/索引中 → false）。
func (m *configManager) engineReady(ctx context.Context, eng, workdir string) bool {
	out, err := m.engine(ctx, eng+"_status", map[string]any{"workdir": workdir})
	if err != nil {
		return false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return false
	}
	var st struct {
		State string `json:"state"`
	}
	if json.Unmarshal(b, &st) != nil {
		return false
	}
	return st.State == "ready"
}

// ─── 运行时状态回写（D-71 迁移收口，2026-10-27）──────────────────────
// 引擎状态键（prj 运行时键，非用户配置：不迁移、不参与 config_invalid 校验）。
const (
	statusKeyCodegraph = "codegraph.status"
	statusKeyVfts      = "vfts.status"
)

// instForWorkdir 反查该 workdir 的活跃实例 id（无则空串；多实例取字典序最小，稳定）。
func (m *configManager) instForWorkdir(wd string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var inst string
	for id, w := range m.workdirs {
		if w == wd && (inst == "" || id < inst) {
			inst = id
		}
	}
	return inst
}

// PLUGIN-UI-2（2026-10-28）：旧的 `mirrorStatus` / `mirrorStatusByWorkdir` 已**删除**——
// 插件不再把引擎状态回写到 prj 键 `codegraph.status` / `vfts.status`。状态一律经本插件
// 动作面现取：`engine-status`（完整状态，配置页 / 状态页 / L4 门禁）/ `badge`（状态栏紧凑视图）。

// isStatusKey 判定是否为运行时状态键（迁移/校验时需要放过）。
func isStatusKey(key string) bool {
	return key == statusKeyCodegraph || key == statusKeyVfts
}

// ─── 一次性迁移（旧宿主键 → plugins.codegraph.*）─────────────────────

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
			m.logf("codegraph: 配置迁移读新键 %s 失败（instance=%s）：%v（保留旧键，下次重试）", lm.New, inst, err)
			continue
		}
		if newOK {
			continue
		}
		oldVal, oldOK, err := m.store.Get(ctx, inst, lm.Old)
		if err != nil {
			m.logf("codegraph: 配置迁移读旧键 %s 失败（instance=%s）：%v（下次重试）", lm.Old, inst, err)
			continue
		}
		if !oldOK {
			continue
		}
		if err := m.store.Set(ctx, inst, map[string]string{lm.New: oldVal}); err != nil {
			m.logf("codegraph: 配置迁移写新键 %s 失败（instance=%s）：%v（保留旧键，下次重试）", lm.New, inst, err)
			continue
		}
		if err := m.store.Delete(ctx, inst, lm.Old); err != nil {
			// 回滚新键 → 维持「旧键仍在」的单源状态，下次启动整体重试
			// （否则新键已写、旧键永留的中间态会被后续迁移判为「新键已存在」而跳过）。
			if rbErr := m.store.Delete(ctx, inst, lm.New); rbErr != nil {
				m.logf("codegraph: 配置迁移回滚新键 %s 失败（instance=%s）：%v（旧键残留，需人工清理）", lm.New, inst, rbErr)
			}
			m.logf("codegraph: 配置迁移删除旧键 %s 失败（instance=%s）：%v（已回滚新键，下次重试）", lm.Old, inst, err)
			continue
		}
		migrated++
		m.logf("codegraph: 配置迁移 %s → %s（instance=%s）", lm.Old, lm.New, inst)
	}
	if migrated > 0 {
		m.logf("codegraph: 配置迁移完成（instance=%s，共 %d 键）", inst, migrated)
	}
	return migrated
}

// migrateAll 对宿主当前全部实例执行一次迁移（启动 / server-ready 时机）。
func (m *configManager) migrateAll(ctx context.Context) {
	ids, err := m.hostInstances(ctx)
	if err != nil {
		m.logf("codegraph: 配置迁移跳过（%v）", err)
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
		// 与 handleSave / 配置刷新共用 applyMu：避免旧快照覆盖并发的新配置（见 applyMu 注释）。
		m.applyMu.Lock()
		defer m.applyMu.Unlock()
		stored, err := m.readValues(ctx, ev.InstanceID)
		if err != nil {
			m.logf("codegraph: 读项目配置失败（instance=%s）：%v", ev.InstanceID, err)
			return
		}
		if _, err := m.apply(ctx, ev.InstanceID, wd, settingsOf(stored)); err != nil {
			m.logf("codegraph: 项目 %s 配置下发引擎失败：%v", wd, err)
			return
		}
		// SRCH-005-S01：启用且未索引（未初始化 / 中断）→ 后台全量索引。
		var need []string
		for _, eng := range enabledEngines(settingsOf(stored)) {
			if !m.engineReady(ctx, eng, wd) {
				need = append(need, eng)
			}
		}
		if len(need) > 0 {
			m.scheduleRebuild(need, wd)
		}
		// 说明：启动期**不**回写状态键（PLUGIN-UI-2 起插件根本不写 `*.status`；
		// 状态由配置页 / 状态栏 / L4 门禁经插件动作 `status` / `badge` 现取）。
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
			// 读+下发串行（见 applyMu 注释）：保证不会用旧快照覆盖并发 handleSave 的新配置。
			m.applyMu.Lock()
			stored, err := m.readValues(ctx, inst)
			if err != nil {
				m.applyMu.Unlock()
				m.logf("codegraph: 配置刷新读取失败（instance=%s）：%v", inst, err)
				continue
			}
			if _, err := m.apply(ctx, inst, wd, settingsOf(stored)); err != nil {
				m.logf("codegraph: 配置刷新下发引擎失败（instance=%s）：%v", inst, err)
			}
			m.applyMu.Unlock()
			// 去抖窗口内的多次刷新只重建一次（引擎配置已在锁内下发完成）。
			m.scheduleRebuild(enabledEngines(settingsOf(stored)), wd)
		}
	}()
}
