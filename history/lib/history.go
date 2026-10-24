// Package history 是文件历史插件（**独立 ref 的检查点链**，server 内嵌形态）。
//
// 定位（2026-09-27 批次③改造）：不再往**用户分支**提交快照（移除 `git add -A` + `git commit`），
// 改用**独立 ref 的检查点链**：
//
//   - 临时 index（**持久**，每 workdir 一个）：`<workdir>/.chonkpilot/history/index`
//     （`.chonkpilot/` 已被 ensureGitignore 保证忽略）。所有打点走它，
//     **绝不碰 `.git/index`、绝不碰 HEAD**；
//   - 检查点 = `commit-tree`（复用该临时 index 的 tree）+ `update-ref refs/chonkpilot/<slug>`；
//     `<slug>` = **根会话**标识（优先 top_session，缺省 session；父子会话共享一条链）；
//   - 链 = parent 指针线性串联 → `git rev-list` 可枚举、`git diff A B` 可用；ref 只指向链头
//     （对象靠可达性保活，gc 安全）。
//
// 打点触发点：
//
//  1. gateway **前置打点钩子**（执行工具前，同步；见 callgate.go 的 pre_hook_subject）：
//     启用 && 未熔断 && 脏 → 同步打点；打点失败 → 写 error → gateway 拒绝该工具调用（工具不执行）；
//  2. **轮末补点**（`session-complete`，主轮次）：保证"最后一步的产像"存在。
//     **不依赖脏位**（强制走一次打点流程），避免被 `filesys.changed` 的 60ms 去抖竞态跳过；
//     流程内以「新 tree == 链头 tree」跳过无变化的建点（不产生空点）。
//
// 熔断：连续打点失败 ≥ fuseThreshold 次 → 进入 `fused` 模式（**放行、不再拦截**），
// 成功一次即复位；失败次数与模式状态经 `history.status` 回写可见（见 chain.go）。
//
// 降级：`git` 不存在 / workdir 无 `.git` → 功能禁用（不视为启动失败），**工具摘除**（syncTools）。
//
// 语义边界（**有意**为之）：
//   - 被 `.gitignore` 忽略的文件**不进检查点、也回滚不了**（含 `.env`、`node_modules`、
//     `.chonkpilot/`）——不把密钥写进对象库；
//   - **未跟踪文件**（不在任何检查点里）回滚时**不动**；
//   - 回滚以检查点的 tree 为准；**不碰 `.git`**；
//   - 检查点**遵守 git 的 ignore**，与"索引排除规则"（lib/ignore）是**两套独立规则**，不联动。
package history

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
	"github.com/chonkpilot/chonkpilot-plugin/dataclient"
	"github.com/chonkpilot/chonkpilot-plugin/instance"
)

// 订阅主题（相对主题；chonk. 前缀在 mq 初始化 Options.Prefix 注入一次，业务层不出现 chonk. 字面）。
const (
	// completeSubject：主轮次终态 → **轮末补点**（保证最后一步的产像存在）。
	completeSubject = "session-complete"
	// filesysChangedSubject：该 workdir 的文件变更广播 → 置"脏"位（打点前置条件）。
	filesysChangedSubject = "filesys.changed"
	// instanceRegisterSubject：实例注册后异步回读 history.enabled（门控）+ 建立 workState。
	instanceRegisterSubject = "instance-register"
	// data 面（persist 订阅）：配置实时同步 + 单键回读 / 回写。
	prjConfigRefreshSubject = "data-prj-config-refresh"
	prjConfigLoadSubject    = "data-prj-config-load"
	prjConfigSaveSubject    = "data-prj-config-save"
	// gateway 方法面：tools/register | tools/unregister → mcp-tools-register/unregister。
	subjectToolRegister   = "mcp-tools-register"
	subjectToolUnregister = "mcp-tools-unregister"
	// toolCallSubject = 本插件声明的 gateway 工具回调主题（gateway regProv 命中后发
	// {tool,args,context}，订阅者写回 v.Result，同一主题 promise）。
	toolCallSubject = "history-tool-call"
	// preHookSubject = gateway 前置打点钩子主题（执行工具前 gateway → 本插件，同步；
	// 见 callgate.go 注册载荷的可选字段 pre_hook_subject）。
	preHookSubject = "history-pre-tool-hook"

	// prj-config 键（见 [64 §4]）。
	historyEnabledKey = "history.enabled"             // "true"/"false"（**仅显式 "true" 才开启**）
	keepKey           = "history.checkpoint_keep"     // 检查点保留个数（默认 500）
	ttlKey            = "history.checkpoint_ttl_days" // 保留窗口天数（默认 7；锚点 = 链上最新点时间）
	// statusKeyPrefix / timelineKeyPrefix：**按会话**的内部键（键名带会话后缀 = 链 slug；
	// 落 prjusr，见 persist localRuntimeKeys），插件回写、前端只读回显当前会话的链
	// （I-135 闭环：多会话并发不再互相覆盖）。见 [64 §4.2]。
	statusKeyPrefix   = "history.status."   // 状态 JSON（插件回写，内部键）
	timelineKeyPrefix = "history.timeline." // 检查点时间线 JSON 数组（最新在前，≤200，内部键）
	clearKey          = "history.clear"     // 动作信号键：前端写入 JSON {"ts","session"} → 清**该会话**链

	// 链 ref 前缀。
	chainRefPrefix = "refs/chonkpilot/"

	// 保留/修剪缺省与阈值。
	defaultKeep    = 500
	defaultTTLDays = 7
	fuseThreshold  = 3
	timelineMax    = 200
	// diffMaxBytes：history_diff 单次输出上限（超出截断并标注）。
	diffMaxBytes = 32 * 1024
	// showMaxBytes：history_show 单次输出上限（超出截断并标注；与 diff 同口径）。
	showMaxBytes = diffMaxBytes
	// gitTimeout：单次 git 子进程执行上限（本地操作；防止 git 卡死长期持有 ws.mu 阻塞同 workdir）。
	gitTimeout = 120 * time.Second

	dataTimeout = 5 * time.Second
)

// History 是文件历史插件（plugin.Hook 实现）。
type History struct {
	deps plugin.Deps
	im   *instance.Manager
	subs []mq.Sub

	mu    sync.Mutex // 保护 works 映射
	works map[string]*workState

	gateMu   sync.Mutex      // gate/gateBusy 访问
	gate     map[string]bool // instanceID → 是否启用（存在 = 已回读；缺失 = 未回读 → 关闭）
	gateBusy map[string]bool // instanceID → 回读中（去重异步回读）

	cfgMu sync.Mutex // keep/ttl（v1 全局生效，见 readOpts 注释）
	keep  int
	ttl   int

	sessMu sync.Mutex // sess 映射
	sess   map[string]sessRec

	syncMu     sync.Mutex // syncTools 单飞
	registered bool       // 4 个工具是否已注册到 gateway（全局仅一份）
}

// sessRec 单会话（含子会话）的链归属（轮末补点用）：根会话 + 所属 workdir + 实例。
type sessRec struct {
	root       string
	workdir    string
	instanceID string
}

// workState 单个 workdir 的历史状态。
//
// 字段锁约定：works 映射由 h.mu 保护；本记录内的 git 操作与轮次跟踪由 ws.mu 串行
// （同一 workdir 的 git 操作串行；不同 workdir 各持独立锁、互不影响）；
// enabled/dirty 为**原子**位（filesys 广播 / 配置刷新可能来自其它 goroutine，走原子避免与 ws.mu 争用）。
type workState struct {
	workDir string
	index   string // <workDir>/.chonkpilot/history/index（持久临时 index）

	mu sync.Mutex

	hasGit bool // 创建时探测（.git 存在；worktree/submodule 的 .git 文件亦可）

	enabled atomic.Bool // 配置 history.enabled（门控）
	dirty   atomic.Bool // 订阅 filesys.changed 置位；打点成功后清位

	fused     bool   // 熔断：连续失败 ≥ 阈值 → 放行、不再拦截
	failCount int    // 连续失败次数
	lastErr   string // 最近一次失败原因

	lastAt    time.Time // 最近一次成功打点时间
	lastDurMs int64     // 最近一次打点耗时

	lastTurn    string // 最近一次打点所属轮次
	lastSlug    string // 最近一次打点所属链 slug（根会话；按会话回写状态时用于归属判定）
	turnStartID string // 当前轮起点检查点（`to="turn-start"`）
	prevTurnID  string // 上一轮起点检查点（保底永不删）
}

// New 构建 history 插件。
func New() *History {
	return &History{
		works:    make(map[string]*workState),
		gate:     make(map[string]bool),
		gateBusy: make(map[string]bool),
		sess:     make(map[string]sessRec),
		keep:     defaultKeep,
		ttl:      defaultTTLDays,
	}
}

// Name 插件名。
func (h *History) Name() string { return "history" }

// Start 订阅事件并自检（宿主在全部插件前序就绪后调用）。
func (h *History) Start(d plugin.Deps) error {
	h.deps = d
	logf := d.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if _, err := exec.LookPath("git"); err != nil {
		logf("history: git 不可用（%v）——文件历史禁用，安装 git 后生效", err)
		return nil // git 缺失：功能禁用，不视为启动失败
	}
	h.im = instance.New(d.Bus)
	h.im.SetLogf(d.Logf) // 告警上报宿主日志（d.Logf 为 nil 时 Manager 回落 stderr）
	if err := h.im.Start(); err != nil {
		return err
	}
	// 订阅失败回滚：任一步失败 → 退订已注册的全部订阅 + 停实例视图，回到"未启动"态，
	// 避免半订阅残留（对齐 codegraph/vfts 的 unsubscribeAll）。
	ok := false
	defer func() {
		if !ok {
			h.unsubscribeAll()
			h.im.Stop()
		}
	}()
	// 事件类订阅（fire-and-forget 形态；handler 不写 v.Result）。
	for _, s := range []struct {
		subject string
		h       mq.Handler
	}{
		{instanceRegisterSubject, h.onInstanceRegister},
		{instance.SubjectExit, h.onInstanceExit}, // 实例退出 → 回收 gate/sess 映射（防无界增长）
		{filesysChangedSubject, h.onFilesysChanged},
		{completeSubject, h.onComplete},
	} {
		sub, err := d.Bus.On(s.subject, 0, func(_ context.Context, subj string, v *mq.Value) error {
			s.h(subj, v.Payload)
			return nil
		})
		if err != nil {
			return err
		}
		h.subs = append(h.subs, sub)
	}
	// data-prj-config-refresh → 开关/保留参数实时同步 + history.clear 动作（复用既有主题，不新增 MQ）。
	cfgSub, err := d.Bus.On(prjConfigRefreshSubject, 0, h.onPrjConfigRefresh)
	if err != nil {
		return err
	}
	h.subs = append(h.subs, cfgSub)
	// gateway 前置打点钩子（执行工具前同步；订阅方写回 v.Result/v.Errors）。
	hookSub, err := d.Bus.On(preHookSubject, 0, h.onPreToolHook)
	if err != nil {
		return err
	}
	h.subs = append(h.subs, hookSub)
	// gateway 工具执行回调（regProv 向本主题发请求，写回 v.Result）。
	callSub, err := d.Bus.On(toolCallSubject, 0, h.onToolCall)
	if err != nil {
		return err
	}
	h.subs = append(h.subs, callSub)
	ok = true
	logf("history: 检查点链就绪（前置钩子=%s / 回调=%s；**默认关闭**，仅 prj-config %s=\"true\" 时打点）",
		preHookSubject, toolCallSubject, historyEnabledKey)
	return nil
}

// unsubscribeAll 退订全部订阅（Start 失败回滚用；与 codegraph/vfts 的 unsubscribeAll 一致）。
func (h *History) unsubscribeAll() {
	for _, s := range h.subs {
		_ = s.Unsubscribe()
	}
	h.subs = nil
}

// Stop 释放插件资源（plugin.Stopper；宿主退出时按 Start 逆序调用）：退订全部订阅 + 停实例视图
// （git 缺失时 im 为 nil → 仅退订）。幂等。
func (h *History) Stop() {
	h.unsubscribeAll()
	if h.im != nil {
		h.im.Stop()
	}
}

// logf 取日志函数（未装配时静默）。
func (h *History) logf() func(string, ...any) {
	if h.deps.Logf != nil {
		return h.deps.Logf
	}
	return func(string, ...any) {}
}

// ─── 门控（history.enabled）────────────────────────────────

// gateFromValue 判定 prj-config history.enabled 的配置值是否启用文件历史。
// **口径（42 §2 (125)）：只有显式 "true" 才开启**；缺失 / "" / "false" / 其他非法值 → 关闭
// （默认不开启；避免"缺省即视为启用"在用户仓库产生未预期的 git 操作）。
func gateFromValue(val string) bool {
	return val == "true"
}

// gateEnabled 判断实例是否启用文件历史：**只有显式开启（回读到 "true"）才为真**；
// 未回读（gate 缺失）→ 关闭（默认不开启）。缺失时异步触发一次回读。
func (h *History) gateEnabled(instanceID string) bool {
	h.gateMu.Lock()
	enabled, ok := h.gate[instanceID]
	if !ok && !h.gateBusy[instanceID] {
		h.gateBusy[instanceID] = true
		go h.safeReadGate(instanceID)
	}
	h.gateMu.Unlock()
	return ok && enabled
}

// safeReadGate 在独立 goroutine 中执行 readGate（裸 goroutine 已脱离 mq.dispatch 的 recover，
// 兜底恢复 panic 仅记日志，不使常驻宿主进程终止）。
func (h *History) safeReadGate(instanceID string) {
	defer func() {
		if r := recover(); r != nil {
			h.logf()("history: readGate panic recovered（instance=%s）：%v", instanceID, r)
		}
	}()
	h.readGate(instanceID)
}

// onInstanceRegister 实例注册 → 建立 workState + 触发 history.enabled 异步回读。
func (h *History) onInstanceRegister(_ string, payload []byte) {
	var ev struct {
		InstanceID string `json:"instance_id"`
		WorkDir    string `json:"work_dir"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.InstanceID == "" {
		return
	}
	if ev.WorkDir != "" {
		h.ensureWork(ev.WorkDir)
	}
	h.gateEnabled(ev.InstanceID) // 触发（或复用）回读
	h.syncTools()                // 已回读的实例：立即收敛工具面
}

// onInstanceExit 实例退出 → 回收 gate/sess 映射：删该实例的门控项 + 清其名下会话归属。
// 不清理会让已退出实例的条目永驻（无界增长）。载荷仅 {instance_id}（61-消息一览 §4.1 ③）。
// 幂等：重复到达无副作用。
func (h *History) onInstanceExit(_ string, payload []byte) {
	var ev struct {
		InstanceID string `json:"instance_id"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.InstanceID == "" {
		return
	}
	h.gateMu.Lock()
	delete(h.gate, ev.InstanceID)
	delete(h.gateBusy, ev.InstanceID)
	h.gateMu.Unlock()
	h.sessMu.Lock()
	for sess, rec := range h.sess {
		if rec.instanceID == ev.InstanceID {
			delete(h.sess, sess)
		}
	}
	h.sessMu.Unlock()
}

// readGate 经 data-prj-config-load 回读 history.enabled：只有显式 "true" 落 gate=true；
// 缺失/非法/读失败 → 关闭（默认不开启）。
func (h *History) readGate(instanceID string) {
	val, err := dataclient.ReadKey(h.deps.Bus, instanceID, historyEnabledKey)
	enabled := err == nil && gateFromValue(val)
	h.gateMu.Lock()
	delete(h.gateBusy, instanceID)
	h.gate[instanceID] = enabled
	h.gateMu.Unlock()
	if err != nil {
		h.logf()("history: 读 prj-config %s 失败（instance=%s）：%v（按默认关闭）", historyEnabledKey, instanceID, err)
	} else {
		h.logf()("history: %s=%q（instance=%s）→ 门控 %v", historyEnabledKey, val, instanceID, enabled)
	}
	// 该实例 workdir 的 enabled 同步 + 保留参数回读。
	if rec, ok := h.im.Lookup(instanceID); ok && rec.WorkDir != "" {
		h.setEnabled(rec.WorkDir, enabled)
	}
	h.readOpts(instanceID)
	h.syncTools()
}

// readOpts 回读保留参数（history.checkpoint_keep / history.checkpoint_ttl_days）。
// v1 限制：单宿主典型形态为单实例/单 workdir，保留参数**全局生效**（多 workdir 差异化留待 v2）。
func (h *History) readOpts(instanceID string) {
	if v, err := dataclient.ReadKey(h.deps.Bus, instanceID, keepKey); err == nil {
		h.setKeep(parseKeep(v))
	}
	if v, err := dataclient.ReadKey(h.deps.Bus, instanceID, ttlKey); err == nil {
		h.setTTL(parseTTL(v))
	}
}

// parseKeep 解析 history.checkpoint_keep（缺失/非法/<=0 → 默认 defaultKeep）。
func parseKeep(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultKeep
	}
	return n
}

// parseTTL 解析 history.checkpoint_ttl_days（缺失/非法/<=0 → 默认 defaultTTLDays）。
func parseTTL(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultTTLDays
	}
	return n
}

func (h *History) setKeep(n int) {
	h.cfgMu.Lock()
	h.keep = n
	h.cfgMu.Unlock()
}

func (h *History) setTTL(n int) {
	h.cfgMu.Lock()
	h.ttl = n
	h.cfgMu.Unlock()
}

func (h *History) keepVal() int {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if h.keep <= 0 {
		return defaultKeep
	}
	return h.keep
}

func (h *History) ttlVal() int {
	h.cfgMu.Lock()
	defer h.cfgMu.Unlock()
	if h.ttl <= 0 {
		return defaultTTLDays
	}
	return h.ttl
}

// onPrjConfigRefresh 处理 data-prj-config-refresh：history.enabled（门控）/
// 保留参数 / history.clear（清链动作）。
// 广播载荷 {instance_id?, id, ids?, op, list}——本实现忽略 instance_id（v1 限制：单宿主典型形态
// 单实例/单 workdir，把开关应用到全部已知实例；多 workdir 差异化留待 v2）。
// **批量写**（61 §3.1）载荷带 `ids`（全组键）→ 逐个键按下述语义处理（= 与改前「逐键广播」等价）；
// 缺省回落单键 `id`（向后兼容旧广播/旧发送方）。
func (h *History) onPrjConfigRefresh(_ context.Context, _ string, v *mq.Value) error {
	var ev struct {
		ID   string         `json:"id"`
		IDs  []string       `json:"ids"`
		Op   string         `json:"op"`
		List map[string]any `json:"list"`
	}
	if json.Unmarshal(v.Payload, &ev) != nil {
		return nil
	}
	keys := ev.IDs
	if len(keys) == 0 {
		keys = []string{ev.ID}
	}
	for _, key := range keys {
		switch key {
		case historyEnabledKey:
			// 只有显式 "true" 才开启；缺失/""/非法/op=delete → 关闭（默认不开启）。
			enabled := false
			if ev.List != nil {
				if raw, ok := ev.List[historyEnabledKey]; ok {
					enabled = gateFromValue(dataclient.Strval(raw))
				}
			}
			h.gateMu.Lock()
			for _, inst := range h.im.List() {
				h.gate[inst.ID] = enabled
				h.setEnabled(inst.WorkDir, enabled)
			}
			h.gateMu.Unlock()
			h.logf()("history: prj-config %s → %v（op=%s）", historyEnabledKey, enabled, ev.Op)
			h.syncTools()
		case keepKey:
			if ev.Op != "delete" && ev.List != nil {
				h.setKeep(parseKeep(dataclient.Strval(ev.List[keepKey])))
			} else {
				h.setKeep(defaultKeep)
			}
		case ttlKey:
			if ev.Op != "delete" && ev.List != nil {
				h.setTTL(parseTTL(dataclient.Strval(ev.List[ttlKey])))
			} else {
				h.setTTL(defaultTTLDays)
			}
		case clearKey:
			// 动作信号（前端写入 JSON `{"ts":"…","session":"<根会话>"}`）：只清**目标会话**的检查点链
			// + 回写该会话状态（I-136 闭环：不再按仓库粒度清空全部链）。
			// 删键不作动作（与 codegraph.action 同口径）；值非法/无 session → 不动作（避免误清全仓）。
			if ev.Op == "delete" {
				continue
			}
			if slug := clearTargetSession(dataclient.Strval(ev.List[clearKey])); slug != "" {
				if err := h.clearChain(slug); err != nil {
					h.logf()("history: history.clear 清空目标会话链未完全成功（slug=%s）：%v", slug, err)
				}
			} else {
				h.logf()("history: history.clear 值非法或未带 session（忽略，不动作）：%q", dataclient.Strval(ev.List[clearKey]))
			}
		}
	}
	return nil
}

// clearTargetSession 解析 history.clear 的值（JSON `{"ts","session"}`）→ 目标链 slug（根会话）。
// 解析失败 / `session` 为空 → ""（不动作）。兼容旧发送方（任意非 JSON 串）→ 同样 ""（不误清全仓）。
func clearTargetSession(raw string) string {
	var v struct {
		Session string `json:"session"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &v) != nil {
		return ""
	}
	return chainSlug(v.Session)
}

// clearChain 只清**目标会话**的检查点链（`update-ref -d refs/chonkpilot/<slug>`）+ 回写该会话状态。
// 其它会话的链（不同 slug）**保留**；该 slug 不在某 workdir 时跳过（零副作用）。
// `update-ref -d` 失败 → 记 error、**跳过**该 workdir 的锚点/熔断态复位与成功日志（链仍在，
// 复位会让锚点指向已不存在的检查点）并返回错误；其余 workdir 仍 best-effort 继续处理。
func (h *History) clearChain(slug string) error {
	h.mu.Lock()
	list := make([]*workState, 0, len(h.works))
	for _, ws := range h.works {
		list = append(list, ws)
	}
	h.mu.Unlock()
	var firstErr error
	for _, ws := range list {
		if !ws.hasGit {
			continue
		}
		ws.mu.Lock()
		if _, err := h.git(ws, "rev-parse", "--verify", "--quiet", chainRefPrefix+slug); err != nil {
			ws.mu.Unlock()
			continue // 该 workdir 无此会话链 → 跳过（零副作用）
		}
		if _, err := h.git(ws, "update-ref", "-d", chainRefPrefix+slug); err != nil {
			ws.mu.Unlock()
			h.logf()("history: history.clear 删除目标会话链失败（workdir=%s，slug=%s）：%v", ws.workDir, slug, err)
			if firstErr == nil {
				firstErr = err
			}
			continue // 链未删 → 不复位锚点/熔断态、不写成功日志
		}
		// 仅当该 workdir 的轮次锚点/熔断态归属本会话时才一并复位（不波及其它会话的保底项）。
		if ws.lastSlug == slug {
			ws.fused = false
			ws.failCount = 0
			ws.lastErr = ""
			ws.lastAt = time.Time{}
			ws.lastDurMs = 0
			ws.turnStartID = ""
			ws.prevTurnID = ""
			ws.lastTurn = ""
			ws.lastSlug = ""
		}
		ws.mu.Unlock()
		h.logf()("history: history.clear → 已清空目标会话检查点链（workdir=%s，slug=%s）", ws.workDir, slug)
		h.writeback(ws, slug)
	}
	return firstErr
}

// ─── workState 生命周期 ────────────────────────────────────

// ensureWork 取/建 workdir 的 workState（首次探测 .git）。
func (h *History) ensureWork(wd string) *workState {
	wd = filepath.Clean(wd)
	h.mu.Lock()
	defer h.mu.Unlock()
	if ws := h.works[wd]; ws != nil {
		return ws
	}
	ws := &workState{
		workDir: wd,
		index:   filepath.Join(wd, ".chonkpilot", "history", "index"),
	}
	if _, err := os.Stat(filepath.Join(wd, ".git")); err == nil {
		ws.hasGit = true
	}
	h.works[wd] = ws
	return ws
}

// work 取 workdir 的 workState（不存在 → nil）。
func (h *History) work(wd string) *workState {
	wd = filepath.Clean(wd)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.works[wd]
}

// setEnabled 设置 workdir 的门控（workState 不存在则忽略）。
func (h *History) setEnabled(wd string, enabled bool) {
	if wd == "" {
		return
	}
	if ws := h.work(wd); ws != nil {
		ws.enabled.Store(enabled)
	}
}

// onFilesysChanged 置"脏"位（该 workdir 的文件变更广播）。
func (h *History) onFilesysChanged(_ string, payload []byte) {
	var ev struct {
		WorkDir string `json:"work_dir"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.WorkDir == "" {
		return
	}
	if ws := h.work(ev.WorkDir); ws != nil {
		ws.dirty.Store(true)
	}
}

// rememberSession 记录会话 → 链归属（轮末补点定位用）。
func (h *History) rememberSession(session, root, workdir, instanceID string) {
	if session == "" {
		return
	}
	h.sessMu.Lock()
	h.sess[session] = sessRec{root: root, workdir: workdir, instanceID: instanceID}
	h.sessMu.Unlock()
}

// ─── 前置打点钩子 / 轮末补点 ────────────────────────────────

// onPreToolHook 处理 gateway 前置打点钩子（执行工具前，同步）。
// 启用 && 未熔断 && 脏 → 同步打点；打点失败 → 返回 error（gateway 拒绝该工具调用）。
// 未启用 / 未熔断但脏=false / 无 workdir → 直接放行（不做任何 git 调用）。
//
// 「涉及文件变动」（payload `touch_files`，用户口径 2026-09-28）：显式 `false` → **直接放行、不打点**
// （省 8–9 次 git 进程）；仍已登记会话归属 → **轮末补点（force）照常保底**。payload 缺该字段 /
// `true` → 与改前一致（打点）。标错只会让检查点粒度变粗（前后点仍在、`git diff` 一致性校验仍生效），
// **不丢安全**。
func (h *History) onPreToolHook(_ context.Context, _ string, v *mq.Value) error {
	var msg struct {
		Tool       string `json:"tool"`
		TouchFiles *bool  `json:"touch_files"`
		Context    *struct {
			Session    string `json:"session"`
			Turn       string `json:"turn"`
			InstanceID string `json:"instance_id"`
			TopSession string `json:"top_session"`
		} `json:"context"`
	}
	if json.Unmarshal(v.Payload, &msg) != nil || msg.Context == nil {
		return nil // 载荷不明 → 放行（不拦工具）
	}
	c := msg.Context
	if c.InstanceID == "" {
		return nil
	}
	rec, ok := h.im.Lookup(c.InstanceID)
	if !ok || rec.WorkDir == "" {
		return nil
	}
	ws := h.work(rec.WorkDir)
	if ws == nil || !ws.hasGit {
		return nil
	}
	root := c.TopSession
	if root == "" {
		root = c.Session
	}
	h.rememberSession(c.Session, root, rec.WorkDir, c.InstanceID)
	if msg.TouchFiles != nil && !*msg.TouchFiles {
		return nil // 不涉及文件变动 → 不打点（轮末补点保底仍在）
	}
	tool := msg.Tool
	if tool == "" {
		tool = "unknown"
	}
	return h.checkpointSync(ws, root, tool, c.Turn, false)
}

// onComplete 主轮次终态 → 轮末补点（保证最后一步的产像存在）。
// session-complete 载荷不带 instance_id/parents → 用前置钩子登记过的会话归属定位；
// 未登记（本轮无工具调用）/ 子会话 → 跳过。
// 补点**不依赖脏位**（force=true）：避免「工具改文件 → 轮次结束」间隔 < filesys.changed
// 的 60ms 去抖窗口时脏位未置上而漏点；流程内以树比较跳过无变化的建点。
func (h *History) onComplete(_ string, payload []byte) {
	var ev struct {
		Session    string `json:"session"`
		Turn       string `json:"turn"`
		TopSession string `json:"top_session"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Session == "" {
		return
	}
	h.sessMu.Lock()
	rec, ok := h.sess[ev.Session]
	h.sessMu.Unlock()
	if !ok {
		return // 本轮无工具调用 → 无变更来源
	}
	if ev.TopSession != "" {
		rec.root = ev.TopSession
	}
	if rec.root != "" && rec.root != ev.Session {
		return // 仅主轮次补点
	}
	ws := h.work(rec.workdir)
	if ws == nil {
		return
	}
	if err := h.checkpointSync(ws, rec.root, "session-complete", ev.Turn, true); err != nil {
		h.logf()("history: 轮末补点失败（workdir=%s）：%v", ws.workDir, err)
	}
}

// ─── 工具注册/注销收敛（gateway 全局一份）────────────────────

// syncTools 决定并执行 gateway 工具注册/注销（syncMu 单飞）：
//
//	应注册 = 存在某 workdir：hasGit 且 enabled。
//
// 工具面全局一份（gateway tools/register 无 scope 扩展）：多个 workdir 同时启用时仅注册一份，
// 执行时经回调 context.instance_id 路由回正确 workdir。
func (h *History) syncTools() {
	h.syncMu.Lock()
	defer h.syncMu.Unlock()
	want := h.anyActive()
	if want == h.registered {
		return
	}
	if want {
		if err := h.registerAll(); err != nil {
			h.logf()("history: gateway 工具注册失败（保持未注册，后续事件重试）：%v", err)
			return
		}
		h.registered = true
		h.logf()("history: gateway 工具已注册（%d 个）", len(historyTools))
		return
	}
	if err := h.unregisterAll(); err != nil {
		h.logf()("history: gateway 工具注销失败（后续事件重试）：%v", err)
		return
	}
	h.registered = false
	h.logf()("history: gateway 工具已注销（无启用中的 workdir）")
}

// anyActive 是否存在"已启用且是 git 仓库"的 workdir。
func (h *History) anyActive() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ws := range h.works {
		if ws.hasGit && ws.enabled.Load() {
			return true
		}
	}
	return false
}

// ─── prj-config 请求-响应 / 回写（镜像 codegraph 模式）────────
// dataEmit / newReqID / strval / prjConfigReadKey / prjConfigSaveKey 已收口到 plugin/dataclient
// （三插件逐字重复 → 单点实现），本包改为调用 dataclient.*。

// instanceForWorkdir 返回该 workdir 任一活跃实例 id（回写 prj-config 用；无则空串）。
func (h *History) instanceForWorkdir(wd string) string {
	var inst string
	for _, ins := range h.im.List() {
		if filepath.Clean(ins.WorkDir) == wd && (inst == "" || ins.ID < inst) {
			inst = ins.ID
		}
	}
	return inst
}

// ensureGitignore 保证 work-dir .gitignore 含 .chonkpilot/（chonkpilot 数据目录不入库）。
// best-effort：写失败仅告警不阻断打点（但 .chonkpilot/ 可能未被忽略，需可见诊断）。
func (h *History) ensureGitignore(wd string) {
	p := filepath.Join(wd, ".gitignore")
	raw, err := os.ReadFile(p)
	if err == nil {
		// 按行匹配（而非子串），避免 `.chonkpilotfoo` 之类被误判为已含规则。
		for _, line := range strings.Split(string(raw), "\n") {
			switch strings.TrimSpace(line) {
			case ".chonkpilot/", ".chonkpilot":
				return
			}
		}
	}
	// 不存在或未忽略 → 追加（创建）
	add := ""
	if err != nil {
		add = "# chonkpilot\n"
	} else if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		// 原文件末尾无换行 → 先补换行，避免与末行规则粘连（既破坏用户规则又使新规则不生效）。
		add = "\n"
	}
	add += ".chonkpilot/\n"
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		h.logf()("history: 打开 .gitignore 失败（%s）：%v——.chonkpilot/ 可能未入忽略，请手工添加", p, err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(add); err != nil {
		h.logf()("history: 写 .gitignore 失败（%s）：%v——.chonkpilot/ 可能未入忽略，请手工添加", p, err)
	}
}
