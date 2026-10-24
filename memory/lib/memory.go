// Package memory 是记忆库沉淀插件（server 内嵌形态，对齐 docs/spec/20-modules/28-plugins.md）。
//
// 定位（42 §2 (27) / 41 D-16）：**每轮（turn 结束）异步**把本轮新信息沉淀进记忆库，
// 与 capability 原语库（LLM 经 mcp_find 获取的"普通知识"）无关。
//
// 触发：订阅 server 每轮终态事件 `session-complete`（**不新增消息面**；事件由 server.finish 在
// 终态落库 + 写快照后发出）。2026-10-24 用户口径：压缩与记忆**毫不相干** —— 压缩服务于
// 「发送前的上下文」，记忆沉淀发生在「轮次结束 / 会话恢复之后」，故**不得借用压缩的触发主题**
// （原订阅 `session-compress` 已解除）。载荷取 `instance_id` / `work_dir` / `data_dir` /
// `session` / `turn`（work_dir/data_dir 为 server 在 session-complete 上**只增**的字段）。
// **队列 + 合并（2026-10-06，用户拍板）**：
// 订阅回调只**置待处理标记**（key = (instance, session)，同一 key **覆盖合并**）并立即返回；
// **每 instance 一个串行 worker** 取标记 → 串行处理（同会话/同实例不并发、不重入；
// 进程退出时未完成任务允许丢弃，Hook 无 Stop）。处理流程：
//
//	读项目配置 memory.enabled（默认 false）→ 关则不动
//	→ 判主/子会话（data-session-get.parent_id；OP-07）
//	→ 取类别清单（data-memory-list；首次访问由 persist 预置类别文件）
//	→ 读会话全部轮（data-session-history）+ 各类别「上次提取 turn」进度（**专用表 memory_extract**，
//	   经 data-memory-extract-load；落 prjusr）
//	→ **累计门控**（OP-05）：自锚点（各启用类别中最旧的已提取轮）以来累计 token ≥
//	   memory.min-turn-tokens（默认 200）才提取；否则跳过（含此前已跳过的轮）
//	→ 按启用类别**并行**：按该类进度取「上次提取轮 → 最新轮」的全部轮消息
//	   （**含此前被阈值跳过的轮**）→ 读旧全文（data-memory-read）+ 新信息 → 经 llm-simple
//	   重写全文（请求按 usr `llm.memory` 带可选 `llm`；`system` = 该类别的沉淀提示词 —— 取自
//	   `data-memory-list` 的 `prompt` 字段；**空 = 该类别无提示词文件 → 不提取**）
//	   → 保存（data-memory-save）→ **成功后推进该类进度**到最后处理的轮（OP-05/06）
//	→ 无新轮（各类进度均已在最新轮）→ 跳过（**不算失败**；天然幂等：已提取的轮因进度推进不重提）
//
// 遍历范围按级别不同（OP-07）：**项目级类别含子 session**（子会话自身的轮终态事件照常提取
// 项目级记忆；子步骤也要提）；**用户偏好仅主 session**（顶层，子会话不含个人表达）。
//
// 写冲突（OP-08）：**per-(workdir/项目, 类别) 进程内互斥 + 读-改-写临界区** —— 同进程多会话
// 并发写同一类别 md 不并发（跨进程由单持有者保证，见 OP-14/D-45，另项）。
//
// 手动沉淀（2026-09-20，批 3 ⑱；2026-10-06 并入队列；2026-10-07 改投递即回，I-128）：另订阅
// 点分主题 `memory.flush`（payload `{instance_id, session}`）—— 置**待处理标记**（force=true）
// 并**立即回执** `{ok, queued, session}`（不再同步等各类别 LLM 跑完）；进度经既有通知面
// tool-notify（`memory-queued`/`memory-start`/`memory-done`）推送，供 statusbar 显示队列状态。
// **不受 memory.min-turn-tokens 门控**（显式动作即用户意图），范围同自动路径（各进度 → 最新轮）。
// 前置不满足 → `{ok:false, reason}`（不静默）。
//
// 不阻塞对话：任一环节失败只记日志 + 上报一次用户可见提示（Deps.Notify），跳过该类，
// 不降级、不抛出（宿主负责提示的去重/限频，见 chonkpilot-llm/server/pluginnotice.go）。
package memory

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chonkpilot/chonkpilot-data"
	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-lib/msgkeys"
	"github.com/chonkpilot/chonkpilot-plugin"
	"github.com/chonkpilot/chonkpilot-plugin/instance"
)

// 订阅/请求主题（相对主题；chonk. 前缀在 mq 初始化注入一次）。
const (
	// turnEndSubject 每轮终态事件（server.finish 终态落库后发；与压缩无关，不新增消息面）。
	turnEndSubject = msgkeys.TopicSessionComplete
	// flushSubject 手动触发一次沉淀（点分相对主题；桥/服务端「点分直通总线」分支受理 →
	// 不属 data-* 数据面，故由本插件订阅应答）。payload `{instance_id, session}`
	// （instance_id 由前端 mq / 桥统一注入）；作用域 = 当前 instance 的**指定会话**，
	// 不跨实例、不跨会话（61-消息一览 §7.1 插件视图新增行，草稿见任务报告）。
	flushSubject = "memory.flush"
	// 数据面（persist 订阅）。
	prjConfigListSubject  = "data-prj-config-list"
	userConfigLoadSubject = "data-user-config-load"
	sessionGetSubject     = "data-session-get" // 判主/子会话（parent_id；OP-07）
	sessionLoadSubject    = "data-session-load-messages"
	sessionHistorySubject = "data-session-history"
	memoryListSubject     = "data-memory-list"
	memoryReadSubject     = "data-memory-read"
	memorySaveSubject     = "data-memory-save"
	// 提取进度专用表（OP-05/06，2026-10-06）：(会话, 类别) → 最后已成功提取的 turn，
	// 落 prjusr 表 memory_extract（此前用 config 键 `memory-extract.<会话>.<类别>`，已废弃：
	// 进度是记录不是配置，不得写入 config）。
	memoryExtractLoadSubject = "data-memory-extract-load"
	memoryExtractSaveSubject = "data-memory-extract-save"
	llmSimpleSubject         = "llm-simple"
	dataTimeout              = 5 * time.Second
	llmTimeout               = 60 * time.Second
	memoryEnabledKey         = "memory.enabled"
	memoryMinTokensKey       = "memory.min-turn-tokens"
	// memoryLLMKey 是记忆子系统的默认 LLM 配置键（usr；SL-3 / SL-C2，40-演进计划 §SL）。
	// 值 = llmref（provider name；键缺失/空串的回落 defaultLLM 由数据层读侧完成，见 SL-1）。
	memoryLLMKey = "llm.memory"
	// memoryCategoryMaxTokensKey 是**单类别 token 告警阈值**（prj 键，见 64-配置项一览 §4.1）：
	// 语义与前端一致 —— **仅告警、不截断、不阻断沉淀**（前端按 data-memory-list 的 tokens 标红，
	// 本插件在沉淀后按同一口径记告警日志，使该键在**服务端亦有读点**；键缺失/非正 → 不限）。
	memoryCategoryMaxTokensKey = "memory.category-max-tokens"
	memoryCategoryPrefix       = "memory.category."
	// memorySubsessionKey 是「提取子会话记忆」项目级开关键（DSL-4，42 §2 (253)）：默认**关闭**
	// （键缺失 / 非 "true"）。关 = 子会话（session.parent_id != ""）turn 跳过自动沉淀；主会话恒不跳过。
	// 注：该键只关**沉淀（写路径）**；记忆清单带出指引（读路径）只随 memory.enabled 门控，与子会话无关。
	memorySubsessionKey  = "memory.subsession"
	defaultMinTurnTokens = 200
	// allTurnsTarget 是 data-session-history 的 target_messages/target_bytes 取值（足够大 →
	// 返回会话**全部轮**，供跨轮提取范围与累计门控）。
	allTurnsTarget = 1 << 30
	// 进度通知取值（I-128）——在**既有通知面** tool-notify 上新增（不新增主题，61-消息一览 §4.3）：
	// 入队（排队）/ 处理前（进行中）/ 处理后（完成）各发一次，供 statusbar 显示沉淀队列状态。
	noticeMemoryQueued = "memory-queued"
	noticeMemoryStart  = "memory-start"
	noticeMemoryDone   = "memory-done"
	// 注（OP-04，2026-10-06）：类别沉淀提示词**已文件化** —— 出厂默认提示词 = 纯文本文件
	// `<级别>/capability/system/memory/<类别名>.md`（读序 项目级 → 用户级 → 系统级磁盘 →
	// embed 内置），由**数据层**（`data-memory-list` 的 `prompt` 字段）解析后下发；本插件不再
	// 持提示词常量，也不再读 prj `memory.prompt.<类别名>` / usr `memory_prompts` 键载体。
)

// Options 是记忆库插件配置（构造基线；项目配置可覆盖阈值）。
type Options struct {
	// MinTurnTokens 沉淀阈值：本轮新增（未提取）token 低于该值 → 跳过（不调 LLM）。
	MinTurnTokens int
}

// DefaultOptions 提供默认阈值（寒暄类如"你好"远低于此，不产生记忆）。
func DefaultOptions() Options {
	return Options{MinTurnTokens: defaultMinTurnTokens}
}

// Plugin 记忆库沉淀插件（server 插件钩子实现）。
type Plugin struct {
	opts     Options
	deps     plugin.Deps
	sub      mq.Sub
	flushSub mq.Sub
	// locks 是 per-(workdir/项目, 类别) 进程内互斥表（OP-08）：键 = workDir + "\x00" + 类别，
	// 值 = *sync.Mutex。读旧全文 → LLM 重写 → 写回整段临界区持锁（同类别重写不并发）。
	locks sync.Map

	// im 是实例视图（订阅 instance-*）：把 instance id 解析回 work_dir，供锁键统一口径
	// （手动沉淀 memory.flush 载荷不含 work_dir，须与自动路径落在同一把 per-(workdir,类别) 锁上）。
	im *instance.Manager

	// 队列（2026-10-06）：每 instance 一个串行 worker + 按 (instance, session) **覆盖合并**的
	// 待处理表。订阅回调只 enqueue（置标记 + 必要时拉起该 instance 的 worker）后立即返回。
	mu      sync.Mutex
	pending map[string]*extractTask // key = instance + "\x00" + session
	running map[string]bool         // key = instance → 该实例 worker 是否在运行
}

// extractTask 是一次待处理的沉淀（enqueue 时构造，worker 消费）。
//   - ev = 最近一次事件载荷（同 key 合并：只留最新；WorkDir/DataDir 空则沿用旧值）；
//   - force = 手动沉淀（memory.flush）→ 跳过 memory.min-turn-tokens 门控。
type extractTask struct {
	ev    turnEvent
	force bool
}

// New 构建记忆库插件（阈值给零值 → 回落默认）。
func New(opts Options) *Plugin {
	if opts.MinTurnTokens <= 0 {
		opts.MinTurnTokens = DefaultOptions().MinTurnTokens
	}
	return &Plugin{opts: opts}
}

// Name 插件名。
func (p *Plugin) Name() string { return "memory" }

// Start 订阅每轮终态事件 + 手动沉淀入口（server 全部插件加载完成后由宿主调用）。
func (p *Plugin) Start(d plugin.Deps) error {
	p.deps = d
	// 实例视图（订阅 instance-register/heartbeat/exit）：仅用于把 instance id 解析回 work_dir，
	// 使手动沉淀（载荷无 work_dir）与自动沉淀共用同一把 per-(workdir,类别) 锁（见 resolveWorkDir）。
	im := instance.New(d.Bus)
	im.SetLogf(d.Logf) // 告警上报宿主日志（d.Logf 为 nil 时 Manager 回落 stderr）
	if err := im.Start(); err != nil {
		return err
	}
	p.im = im
	sub, err := d.Bus.On(turnEndSubject, 0, func(_ context.Context, subj string, v *mq.Value) error {
		p.onTurnEnd(subj, v.Payload)
		return nil
	})
	if err != nil {
		return err
	}
	p.sub = sub
	// memory.flush：手动触发一次沉淀——置待处理标记（force=true）并**投递即回**（I-128：
	// 不再同步等各类别 LLM 跑完，≈60s → 立即返回）；进度经既有通知面 tool-notify 推送；
	// 与自动路径共用同一每 instance 串行 worker（顺序化不并发）。
	fsub, err := d.Bus.On(flushSubject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		v.Result = p.flush(v.Payload)
		return nil
	})
	if err != nil {
		return err
	}
	p.flushSub = fsub
	p.logf()("memory: subscribed %s + %s (min_turn_tokens=%d, gate=%s)", turnEndSubject, flushSubject, p.opts.MinTurnTokens, memoryEnabledKey)
	return nil
}

// Stop 释放插件资源（plugin.Stopper；宿主退出时按 Start 逆序调用）：退订订阅 + 停实例视图。
// 沉淀 worker 为 fire-and-forget（进程退出允许直接丢弃，不 Join/不阻塞退出）。幂等。
func (p *Plugin) Stop() {
	if p.sub != nil {
		_ = p.sub.Unsubscribe()
		p.sub = nil
	}
	if p.flushSub != nil {
		_ = p.flushSub.Unsubscribe()
		p.flushSub = nil
	}
	if p.im != nil {
		p.im.Stop()
	}
}

// logf 取日志函数（未装配时静默）。
func (p *Plugin) logf() func(string, ...any) {
	if p.deps.Logf != nil {
		return p.deps.Logf
	}
	return func(string, ...any) {}
}

// notify 上报一次**用户可见**的沉淀失败（2026-09-20，B 批）：宿主未注入 → 静默（行为同改前）。
// 只上报事实（插件/类别/实例/会话/轮次/原因），是否提示、是否去重由宿主决定（不阻塞、不抛出）。
func (p *Plugin) notify(ev turnEvent, kind, reason string) {
	if p.deps.Notify == nil {
		return
	}
	p.deps.Notify(plugin.Notice{
		Plugin: "memory", Kind: kind,
		InstanceID: ev.InstanceID, Session: ev.Session, Turn: ev.LastTurn,
		Reason: reason,
	})
}

// turnEvent 是本轮沉淀需要的 session-complete 载荷字段（server.finish 发出）。
//
// JSON 键取**事件既有字段名** `turn`（= 本轮轮次 id）；Go 字段沿用 LastTurn（语义 = 本轮沉淀
// 范围的最新轮）。work_dir / data_dir = server 在 session-complete 上**只增**的字段。
type turnEvent struct {
	InstanceID string `json:"instance_id"`
	WorkDir    string `json:"work_dir"`
	DataDir    string `json:"data_dir"`
	Session    string `json:"session"`
	LastTurn   string `json:"turn"`
	// DialogID 一树一库（P3-4b，61 §0.6）：事件同级回带；空 = 旧布局（不写键）。
	DialogID string `json:"dialog_id"`
}

// onTurnEnd 每轮终态 → 置待处理标记（不阻塞对话；由每 instance 串行 worker 异步处理）。
func (p *Plugin) onTurnEnd(_ string, payload []byte) {
	var ev turnEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}
	if ev.InstanceID == "" || ev.WorkDir == "" || ev.Session == "" || ev.LastTurn == "" {
		return
	}
	p.enqueue(ev, false)
}

// sessionTaskKey 队列键：实例 + 会话（不同实例的同名会话互不干扰；同 key 覆盖合并）。
func sessionTaskKey(instanceID, session string) string { return instanceID + "\x00" + session }

// enqueue 置待处理标记并（必要时）拉起该 instance 的串行 worker，**立即返回**。
//
// 合并语义：同一 (instance, session) **只保留最近一次**事件载荷（同会话在沉淀进行中再来的
// 事件被合并，只跑最后一次；沉淀自「进度 → 最新轮」幂等重算，无新轮即跳过）。
// force 取「或」（任一为手动沉淀即跳过阈值门控）。
// 该实例 worker 已在运行 → 本次进队列（排队）→ 广播 `memory-queued`（I-128，statusbar 可见）。
func (p *Plugin) enqueue(ev turnEvent, force bool) {
	if ev.InstanceID == "" || ev.Session == "" {
		return
	}
	key := sessionTaskKey(ev.InstanceID, ev.Session)
	p.mu.Lock()
	if p.pending == nil {
		p.pending = make(map[string]*extractTask)
	}
	if p.running == nil {
		p.running = make(map[string]bool)
	}
	if t, ok := p.pending[key]; ok {
		// 合并：手动沉淀载荷无 work_dir → 沿用前一事件。锁键口径以 process 内的
		// resolveWorkDir（实例视图优先）为准，此处仅为事件载荷补全，不承担锁语义。
		if ev.WorkDir == "" {
			ev.WorkDir = t.ev.WorkDir
		}
		if ev.DataDir == "" {
			ev.DataDir = t.ev.DataDir
		}
		t.ev = ev
		t.force = t.force || force
	} else {
		p.pending[key] = &extractTask{ev: ev, force: force}
	}
	start := !p.running[ev.InstanceID]
	if start {
		p.running[ev.InstanceID] = true
	}
	p.mu.Unlock()
	if start {
		go p.worker(ev.InstanceID)
		return
	}
	// 该实例 worker 已在跑 → 本次事件排在其后（覆盖合并）→ 广播「排队」
	p.emitMemoryNotice(ev.InstanceID, ev.Session, ev.LastTurn, noticeMemoryQueued, "记忆沉淀排队中…")
}

// worker 按 instance 串行消费待处理表：取出任一同实例标记 → 处理 → 循环；无待处理 → 置回
// running 并退出（下次 enqueue 再拉起）。同实例不并发、不重入。
// 处理前/后各广播一次进度（`memory-start` / `memory-done`，I-128）。
// 进程退出时未完成任务允许直接丢弃（不 Join、不阻塞退出）。
func (p *Plugin) worker(instanceID string) {
	prefix := instanceID + "\x00"
	for {
		p.mu.Lock()
		var key string
		var task *extractTask
		for k, t := range p.pending {
			if strings.HasPrefix(k, prefix) {
				key, task = k, t
				break
			}
		}
		if task == nil {
			// 无待处理 → 退出；与 enqueue 在同一把锁下判定 running 归属，避免竞态丢事件。
			delete(p.running, instanceID)
			p.mu.Unlock()
			return
		}
		delete(p.pending, key)
		p.mu.Unlock()
		// 本轮依赖**解析一次**（项目配置 + 会话父级），供子会话门控与 process 复用——消除
		// 改前 skipSubsession + process 各自重复读配置/父级的 2+2 次多余消息往返。
		deps := p.resolveTurnDeps(task.ev.InstanceID, task.ev.Session, task.ev.DialogID)
		// 子会话门控（DSL-4，42 §2 (253)）：在**广播进度前**判定 —— 子会话且「提取子会话记忆」未开
		// → 跳过（不广播 `memory-*` 进度，子会话不入沉淀队列状态展示，见 I-128）。
		if deps.skipSubsession() {
			continue
		}
		p.emitMemoryNotice(task.ev.InstanceID, task.ev.Session, task.ev.LastTurn, noticeMemoryStart, "正在沉淀记忆…")
		p.safeProcessResolved(task, deps)
		p.emitMemoryNotice(task.ev.InstanceID, task.ev.Session, task.ev.LastTurn, noticeMemoryDone, "记忆沉淀完成")
	}
}

// emitMemoryNotice 经**既有通知面** tool-notify（61-消息一览 §4.3；不新增主题）投递沉淀队列
// 进度（`memory-queued` / `memory-start` / `memory-done`），供 statusbar 显示队列状态。
// 只发布不等应答（不阻塞、不抛出）；实例字段必带（61 §0），缺失 → 不广播。
func (p *Plugin) emitMemoryNotice(instanceID, session, lastTurn, notice, message string) {
	if p.deps.Bus == nil || instanceID == "" {
		return
	}
	p.deps.Bus.Emit(context.Background(), msgkeys.TopicToolNotify, map[string]any{
		"instance_id": instanceID,
		"session_id":  session,
		"turn_id":     lastTurn,
		"notice":      notice,
		"message":     message,
		"message_id":  "msg-memory-" + notice + "-" + session + "-" + lastTurn,
		"plugin":      "memory",
	})
}

// safeProcessResolved 包裹 processResolved 并兜住 panic —— worker 是裸 goroutine（已脱离
// mq.dispatch 的 recover），不兜会使 panic 直接终止进程（行为对齐 compress「回调 panic 不致命」）。
func (p *Plugin) safeProcessResolved(t *extractTask, d turnDeps) {
	defer func() {
		if r := recover(); r != nil {
			p.logf()("memory: recovered panic in worker: %v", r)
		}
	}()
	p.processResolved(t.ev, t.force, d)
}

// memoryConfig 是本轮消费的项目配置（memory.*；缺失回落默认）。
type memoryConfig struct {
	Enabled   bool
	MinTokens int
	// Subsession 是「提取子会话记忆」开关（prj `memory.subsession`，DSL-4 42 §2 (253)；默认**关闭**）：
	// 关 = 子会话 turn 跳过自动沉淀（见 process）；主会话恒不跳过。只影响沉淀（写路径）。
	Subsession bool
	// MaxTokens 是单类别 token **告警阈值**（prj `memory.category-max-tokens`）：>0 = 超过即记
	// 告警日志（不截断、不阻断）；0 = 未设置 = 不限。
	MaxTokens  int
	Categories map[string]bool // 类别名 → 启用（键缺失 → 默认启用）
}

// categoryEnabled 判断类别是否启用：读项目配置 memory.category.<类别名>；键缺失 → 默认启用。
// 用户级"用户偏好"同此口径（默认启用、可经设置页显式关闭，键 = memory.category.用户偏好）。
func (c memoryConfig) categoryEnabled(name string) bool {
	if v, ok := c.Categories[name]; ok {
		return v
	}
	return true
}

// extract 执行一次沉淀（自动路径；同步，供测试直接调用——worker 走 process）。
func (p *Plugin) extract(ev turnEvent) { p.process(ev, false) }

// turnDeps 是本轮沉淀的**一次性**依赖解析结果（项目配置 + 会话父级）。worker 解析一次后
// 传给子会话门控与 processResolved 复用，避免同一轮重复读项目配置 / 会话父级（各多一次
// 消息往返）。解析失败时对应 err 非空、值为零值。
type turnDeps struct {
	cfg    memoryConfig
	parent string
	cfgErr error
	parErr error
}

// resolveTurnDeps 解析本轮依赖：先读项目配置；**仅当已启用**才读会话父级（与改前
// process / skipSubsession 口径一致——未启用时二者都不读父级）。
func (p *Plugin) resolveTurnDeps(instanceID, session, dialogID string) turnDeps {
	var d turnDeps
	cfg, err := p.resolveConfig(instanceID)
	if err != nil {
		d.cfgErr = err
		return d
	}
	d.cfg = cfg
	if !cfg.Enabled {
		return d
	}
	parent, err := p.sessionParent(instanceID, session, dialogID)
	if err != nil {
		d.parErr = err
		return d
	}
	d.parent = parent
	return d
}

// skipSubsession 子会话沉淀门控（DSL-4，42 §2 (253)「提取子会话记忆」，默认关闭）：
// 子会话（会话行 `parent_id != ""`）+ 开关未开 → 跳过自动沉淀（worker 在**广播进度前**调用，
// 子会话不入队列状态展示）。与 processResolved 内联门控同口径（读同一 prj 键与同一会话 parent）。
// 读配置 / 父级失败 → 不跳过（保守，不误伤主路径）。
func (d turnDeps) skipSubsession() bool {
	if d.cfgErr != nil || !d.cfg.Enabled || d.cfg.Subsession {
		return false
	}
	if d.parErr != nil {
		return false
	}
	return d.parent != ""
}

// process 执行一次会话沉淀（可同步调用；手动沉淀与 extract 共用）：解析本轮依赖后转
// processResolved（worker 复用已解析的 turnDeps，不重复解析）。
func (p *Plugin) process(ev turnEvent, force bool) map[string]any {
	return p.processResolved(ev, force, p.resolveTurnDeps(ev.InstanceID, ev.Session, ev.DialogID))
}

// processResolved 执行一次会话沉淀（可同步调用；worker 与手动沉淀共用）：配置门控 → 主/子会话判定 →
// 读该会话全部轮 + 各类别进度（专用表）→ **累计门控**（OP-05；force=true 跳过）→ 按启用类别
// 并行**跨轮**重写（各进度 → 最新轮；无新轮即跳过）。
//
// 返回值 = flush 风格回执（`{ok, session, turn, saved, failed, enabled}` /
// 前置不满足 `{ok:false, reason}`）；自动路径的调用方丢弃该值。
// 任一环节失败：记日志 + **上报一次用户可见提示**（宿主去重/限频）→ 跳过，**不阻塞对话**。
func (p *Plugin) processResolved(ev turnEvent, force bool, d turnDeps) map[string]any {
	logf := p.logf()
	// 锁键口径统一：优先经实例视图解析 work_dir（手动沉淀载荷无 work_dir），回落到事件载荷。
	// 否则手动与自动沉淀会对同一 (项目, 类别) 取到不同锁 → 读-改-写丢更新（OP-08）。
	ev.WorkDir = p.resolveWorkDir(ev)
	fail := func(reason string) map[string]any { return map[string]any{"ok": false, "reason": reason} }
	if d.cfgErr != nil {
		logf("memory: 读项目配置失败（instance=%s）：%v（跳过）", ev.InstanceID, d.cfgErr)
		p.notify(ev, "config", d.cfgErr.Error())
		return fail(d.cfgErr.Error())
	}
	cfg := d.cfg
	if !cfg.Enabled {
		return fail(memoryEnabledKey + " not enabled")
	}
	// OP-07：主/子会话判定 —— 用户偏好仅主会话（顶层）提取；项目级含子 session。
	if d.parErr != nil {
		logf("memory: 读会话父级失败（session=%s）：%v（跳过）", ev.Session, d.parErr)
		p.notify(ev, "session", d.parErr.Error())
		return fail(d.parErr.Error())
	}
	parent := d.parent
	// DSL-4（42 §2 (253)「提取子会话记忆」，默认关闭）：子会话 turn 跳过**自动沉淀**
	// （判定 = session.parent_id != ""，与 OP-07 同一 parent 读取）。主会话恒不跳过；
	// 只关沉淀（写路径），不影响记忆清单带出指引（读路径，随 memory.enabled 门控）。
	if parent != "" && !cfg.Subsession {
		logf("memory: 子会话跳过自动沉淀（session=%s；「提取子会话记忆」未开启）", ev.Session)
		return map[string]any{"ok": true, "session": ev.Session, "subsession_skipped": true}
	}
	cats, err := p.memoryCategories(ev.InstanceID)
	if err != nil {
		logf("memory: 读记忆类别失败（instance=%s）：%v（跳过）", ev.InstanceID, err)
		p.notify(ev, "categories", err.Error())
		return fail(err.Error())
	}
	enabled := 0
	for _, c := range cats {
		if cfg.categoryEnabled(c.Category) {
			enabled++
		}
	}
	cats = selectExtractCategories(cats, cfg, parent == "")
	if len(cats) == 0 {
		return map[string]any{"ok": true, "session": ev.Session, "turn": "", "saved": []string{}, "failed": []map[string]string{}, "enabled": enabled}
	}
	// 跨轮范围/累计门控的基础：该会话全部轮（created_at 升序，含预存 full_tokens）。
	turns, err := p.sessionTurns(ev.InstanceID, ev.Session, ev.DialogID)
	if err != nil {
		logf("memory: 读会话轮次失败（session=%s）：%v（跳过）", ev.Session, err)
		p.notify(ev, "turns", err.Error())
		return fail(err.Error())
	}
	if len(turns) == 0 {
		return fail("no turn in session " + ev.Session)
	}
	latest := turns[len(turns)-1].ID
	// OP-05/06：读该会话各类别「上次提取 turn」进度（专用表 memory_extract，落 prjusr）。
	prog := make(map[string]string, len(cats))
	processable := make(map[string]string, len(cats))
	all, perr := p.loadProgress(ev.InstanceID, ev.Session)
	if perr != nil {
		logf("memory: 读提取进度失败（session=%s）：%v（按未提取处理）", ev.Session, perr)
	}
	for _, c := range cats {
		last := all[c.Category]
		prog[c.Category] = last
		// 门控锚点只计**有提示词**的类别（无提示词文件的类别不提取、也不开闸，避免每轮空转）。
		if strings.TrimSpace(c.Prompt) != "" {
			processable[c.Category] = last
		}
	}
	done := func() map[string]any {
		return map[string]any{"ok": true, "session": ev.Session, "turn": latest, "saved": []string{}, "failed": []map[string]string{}, "enabled": enabled}
	}
	if len(processable) == 0 {
		return done()
	}
	cache := newTurnMsgCache(p, ev.InstanceID, ev.DialogID)
	// 累计门控（OP-05）：手动沉淀（force=true）跳过（显式动作即用户意图）。
	if !force {
		tokens, terr := p.rangeTokens(turnsAfter(turns, earliestAnchor(turns, processable)), cache)
		if terr != nil {
			logf("memory: 估算跨轮 token 失败（session=%s）：%v（跳过）", ev.Session, terr)
			p.notify(ev, "turns", terr.Error())
			return fail(terr.Error())
		}
		if tokens < cfg.MinTokens {
			logf("memory: 自上次提取累计 %d token < 阈值 %d → 跳过（session=%s）", tokens, cfg.MinTokens, ev.Session)
			return done()
		}
	}
	saved, failed := p.distillRanged(ev, cfg, cats, turns, prog, cache)
	return map[string]any{
		"ok": true, "session": ev.Session, "turn": latest,
		"saved": saved, "failed": failed, "enabled": enabled,
	}
}

// selectExtractCategories 过滤参与提取的类别（OP-07）：类别启用 + 主/子会话级别约束
// （用户级类别仅主会话；项目级类别主/子会话均提取）。
func selectExtractCategories(cats []categoryInfo, cfg memoryConfig, isMain bool) []categoryInfo {
	out := make([]categoryInfo, 0, len(cats))
	for _, c := range cats {
		if !cfg.categoryEnabled(c.Category) {
			continue
		}
		if !isMain && c.Level == "user" {
			continue // 用户偏好仅主 session
		}
		out = append(out, c)
	}
	return out
}

// turnRef 是会话内一轮的引用：turn_id + 预存全量 token 估算（turns.full_tokens；0 = 缺值）。
type turnRef struct {
	ID     string
	Tokens int
}

// sessionParent 经 data-session-get 读会话父 id（空 = 主会话/顶层；会话不存在 → 空，按主会话处置）。
func (p *Plugin) sessionParent(instanceID, session, dialogID string) (string, error) {
	payload := map[string]any{"session_id": session}
	if dialogID != "" {
		payload["dialog_id"] = dialogID // 一树一库：按对话定位会话行
	}
	res, err := p.request(sessionGetSubject, map[string]any{
		"instance_id": instanceID,
		"data":        payload,
	})
	if err != nil {
		return "", err
	}
	d, _ := res["data"].(map[string]any)
	if d == nil {
		return "", nil
	}
	return strval(d["parent_id"]), nil
}

// sessionTurns 经 data-session-history 读该会话**全部轮**（created_at 升序；含预存 full_tokens）。
// target_messages/target_bytes 取足够大 → 全量返回（跨轮提取范围与累计门控的基础）。
func (p *Plugin) sessionTurns(instanceID, session, dialogID string) ([]turnRef, error) {
	payload := map[string]any{
		"session_id": session, "target_messages": allTurnsTarget, "target_bytes": allTurnsTarget,
	}
	if dialogID != "" {
		payload["dialog_id"] = dialogID // 一树一库：轮次在对话库
	}
	res, err := p.request(sessionHistorySubject, map[string]any{
		"instance_id": instanceID,
		"data":        payload,
	})
	if err != nil {
		return nil, err
	}
	msgs, _ := res["messages"].(map[string]any)
	raw, _ := msgs["turns"].([]any)
	out := make([]turnRef, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		id := strval(m["turn_id"])
		if id == "" {
			continue
		}
		out = append(out, turnRef{ID: id, Tokens: atoiOK(strval(m["full_tokens"]))})
	}
	return out, nil
}

// loadProgress 经 data-memory-extract-load 一次读回该会话**全部类别**的最后已提取 turn
// （专用表 memory_extract，落 prjusr）；返回 类别名 → turn_id（无记录 → 空串，不算失败）。
func (p *Plugin) loadProgress(instanceID, session string) (map[string]string, error) {
	res, err := p.request(memoryExtractLoadSubject, map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"session_id": session},
	})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	raw, _ := res["list"].([]any)
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		cat := strval(m["category"])
		if cat == "" {
			continue
		}
		out[cat] = strval(m["last_turn_id"])
	}
	return out, nil
}

// saveProgress 经 data-memory-extract-save 写某 (会话, 类别) 的最后已提取 turn
// （仅在该类别**成功**写回后推进）。
func (p *Plugin) saveProgress(instanceID, session, category, turnID string) error {
	_, err := p.request(memoryExtractSaveSubject, map[string]any{
		"instance_id": instanceID,
		"data": map[string]any{
			"session_id": session, "category": category, "last_turn_id": turnID,
		},
	})
	return err
}

// earliestAnchor 取各启用类别进度中最旧的已提取轮 id（= 累计门控与跨轮范围的起点）。
// 任一类无进度（或进度轮不在当前轮列表中，如已被清理）→ 返回 ""（= 从头全量提取）。
func earliestAnchor(turns []turnRef, prog map[string]string) string {
	idx := make(map[string]int, len(turns))
	for i, t := range turns {
		idx[t.ID] = i
	}
	best := -1
	for _, last := range prog {
		if last == "" {
			return ""
		}
		i, ok := idx[last]
		if !ok {
			return ""
		}
		if best < 0 || i < best {
			best = i
		}
	}
	if best < 0 {
		return ""
	}
	return turns[best].ID
}

// turnsAfter 返回 anchor 之后的轮（anchor == "" → 全部轮；找不到 anchor → 全部轮）。
func turnsAfter(turns []turnRef, anchor string) []turnRef {
	if anchor == "" {
		return turns
	}
	for i, t := range turns {
		if t.ID == anchor {
			if i+1 >= len(turns) {
				return nil
			}
			return turns[i+1:]
		}
	}
	return turns
}

// turnMsgCache 在一次提取内缓存各轮消息（多类别范围重叠时避免重复拉取 data-session-load-messages）。
type turnMsgCache struct {
	p          *Plugin
	instanceID string
	dialogID   string // 一树一库：轮次消息在对话库（空 = 旧布局）
	mu         sync.Mutex
	m          map[string][]data.ChatMsg
}

// newTurnMsgCache 建本次提取的轮消息缓存。
func newTurnMsgCache(p *Plugin, instanceID, dialogID string) *turnMsgCache {
	return &turnMsgCache{p: p, instanceID: instanceID, dialogID: dialogID, m: map[string][]data.ChatMsg{}}
}

// load 取某轮全部消息（命中缓存直接返回）。
func (c *turnMsgCache) load(turnID string) ([]data.ChatMsg, error) {
	c.mu.Lock()
	if v, ok := c.m[turnID]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()
	msgs, err := c.p.loadTurnMessages(c.instanceID, c.dialogID, turnID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.m[turnID] = msgs
	c.mu.Unlock()
	return msgs, nil
}

// collectRange 拼接一段轮次的消息（按轮序）。
func collectRange(cache *turnMsgCache, rng []turnRef) ([]data.ChatMsg, error) {
	var out []data.ChatMsg
	for _, t := range rng {
		msgs, err := cache.load(t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// rangeTokens 估算一段轮次的累计 token（优先用 turns 预存 full_tokens；缺值轮回退拉消息估算）。
func (p *Plugin) rangeTokens(rng []turnRef, cache *turnMsgCache) (int, error) {
	total := 0
	for _, t := range rng {
		if t.Tokens > 0 {
			total += t.Tokens
			continue
		}
		msgs, err := cache.load(t.ID)
		if err != nil {
			return 0, err
		}
		total += data.EstimateTokensOfMessages(msgs)
	}
	return total, nil
}

// resolveWorkDir 解析锁键用的 work_dir：优先实例视图（instance id → work_dir；宿主登记），
// 无登记/未注入实例视图时回落到事件载荷 ev.WorkDir。
//
// 目的：手动沉淀（memory.flush 载荷只带 instance_id/session）与自动沉淀（session-complete
// 载荷带 work_dir）须落在**同一把** per-(workdir, 类别) 锁上，否则同一 (项目, 类别) 的
// 读-改-写并发执行会丢更新（OP-08）。
func (p *Plugin) resolveWorkDir(ev turnEvent) string {
	if p.im != nil {
		if rec, ok := p.im.Lookup(ev.InstanceID); ok && rec.WorkDir != "" {
			return rec.WorkDir
		}
	}
	return ev.WorkDir
}

// lockCategory 取 per-(workdir/项目, 类别) 进程内互斥锁（OP-08）并加锁，返回解锁函数：
// 「读旧全文 → LLM 重写 → 写回」整段临界区持锁（同进程多会话并发写同一类别 md 不并发）。
// 跨进程正确性依赖单持有者（OP-14/D-45，另项）。
func (p *Plugin) lockCategory(workDir, category string) func() {
	key := workDir + "\x00" + category
	v, _ := p.locks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// distillRanged 是**跨轮**沉淀回路（自动沉淀与手动沉淀共用）：逐类别按各自进度取
// 「上次提取轮 → 最新轮」的全部轮消息（**含此前被阈值跳过的轮**，OP-05）→ 读旧全文 +
// 新信息经 llm-simple 重写 → 保存；成功后**推进该类别的提取进度**（OP-05/06）。
// per-(workdir, 类别) 互斥覆盖整段读-改-写临界区（OP-08）。单类别失败：记日志 +
// 上报一次用户可见提示 → 跳过该类（不降级、不抛出）。可同步调用，供测试。
func (p *Plugin) distillRanged(ev turnEvent, cfg memoryConfig, cats []categoryInfo, turns []turnRef, prog map[string]string, cache *turnMsgCache) ([]string, []map[string]string) {
	logf := p.logf()
	llm := subsystemLLM(p.readUserConfig(ev.InstanceID))
	saved := make([]string, 0, len(cats))
	failed := make([]map[string]string, 0)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, c := range cats {
		wg.Add(1)
		go func(ci categoryInfo) {
			defer wg.Done()
			category := ci.Category
			// 类别沉淀提示词（OP-04）：取自 data-memory-list 的 `prompt`（数据层文件读序解析）。
			// 空 = 该类别**无任何提示词文件** → **不提取**：记日志 + 上报一次用户可见提示 + 计入失败。
			prompt := strings.TrimSpace(ci.Prompt)
			if prompt == "" {
				logf("memory: 类别 %s 无沉淀提示词文件（capability/system/memory/%s.md）→ 不提取", category, category)
				p.notify(ev, "prompt", category+"：无沉淀提示词文件")
				mu.Lock()
				failed = append(failed, map[string]string{"category": category, "kind": "prompt", "reason": "missing prompt file"})
				mu.Unlock()
				return
			}
			rng := turnsAfter(turns, prog[category])
			if len(rng) == 0 {
				return // 无新轮 → 无事可做（不算失败）
			}
			msgs, err := collectRange(cache, rng)
			if err != nil {
				logf("memory: 读轮次消息失败（%s）：%v（跳过）", category, err)
				p.notify(ev, "messages", category+"："+err.Error())
				mu.Lock()
				failed = append(failed, map[string]string{"category": category, "kind": "messages", "reason": err.Error()})
				mu.Unlock()
				return
			}
			newInfo := renderTurnInfo(msgs)
			if strings.TrimSpace(newInfo) == "" {
				return // 空内容 → 无事可做
			}
			// OP-08：读旧全文 → LLM 重写 → 写回，整段临界区（per-(workdir, 类别) 进程内互斥）。
			unlock := p.lockCategory(ev.WorkDir, category)
			defer unlock()
			old, err := p.memoryRead(ev.InstanceID, category)
			if err != nil {
				logf("memory: 读记忆失败（%s）：%v（跳过）", category, err)
				p.notify(ev, "read", category+"："+err.Error())
				mu.Lock()
				failed = append(failed, map[string]string{"category": category, "kind": "read", "reason": err.Error()})
				mu.Unlock()
				return
			}
			text, err := p.rewrite(ev.InstanceID, llm, prompt, category, old, newInfo)
			if err != nil {
				logf("memory: 沉淀 LLM 失败（%s）：%v（跳过）", category, err)
				p.notify(ev, "llm", category+"："+err.Error())
				mu.Lock()
				failed = append(failed, map[string]string{"category": category, "kind": "llm", "reason": err.Error()})
				mu.Unlock()
				return
			}
			// 单类别 token 告警阈值（prj `memory.category-max-tokens`）：语义 = **仅提醒**
			// （不截断、不阻断沉淀，与前端标红同口径）。超限只记日志，仍照常保存。
			if cfg.MaxTokens > 0 {
				if n := data.EstimateTokens(text); n > cfg.MaxTokens {
					logf("memory: 类别 %s 全文约 %d token 超告警阈值 %d（建议细分记忆；不截断）", category, n, cfg.MaxTokens)
				}
			}
			if err := p.memorySave(ev.InstanceID, category, text); err != nil {
				logf("memory: 保存记忆失败（%s）：%v（跳过）", category, err)
				p.notify(ev, "save", category+"："+err.Error())
				mu.Lock()
				failed = append(failed, map[string]string{"category": category, "kind": "save", "reason": err.Error()})
				mu.Unlock()
				return
			}
			// OP-05/06：仅在该类别**成功**写回后推进进度到最后处理的轮（失败不推进 → 下次重提该段）。
			last := rng[len(rng)-1].ID
			if err := p.saveProgress(ev.InstanceID, ev.Session, category, last); err != nil {
				logf("memory: 推进提取进度失败（session=%s/%s → %s）：%v（下次将重提该段）", ev.Session, category, last, err)
			}
			logf("memory: 沉淀 %s（session=%s, 轮 %s→%s, 累计 %d token）", category, ev.Session, rng[0].ID, last, rangeTokensOrZero(cache, rng))
			mu.Lock()
			saved = append(saved, category)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return saved, failed
}

// rangeTokensOrZero 仅供日志：估算一段轮次 token（失败 → 0，不影响主流程）。
func rangeTokensOrZero(cache *turnMsgCache, rng []turnRef) int {
	n := 0
	for _, t := range rng {
		if t.Tokens > 0 {
			n += t.Tokens
			continue
		}
		if msgs, err := cache.load(t.ID); err == nil {
			n += data.EstimateTokensOfMessages(msgs)
		}
	}
	return n
}

// flush 手动触发一次沉淀（memory.flush 订阅者）：解析 payload → 置待处理标记（force=true）→
// **立即回执**。与自动沉淀的差异：
//   - 触发源 = 用户显式动作（不经 session-complete 轮末事件）；
//   - **不受 `memory.min-turn-tokens` 门控**（force=true；显式动作即用户意图，短轮次也照沉淀）；
//   - **投递即回**（I-128）：回执 `{ok, queued, session}`（不再同步等各类别 LLM 跑完），进度由
//     既有通知面 tool-notify（`memory-queued`/`memory-start`/`memory-done`）推送。
//
// 作用域 = payload 指定的 instance + session（不跨实例、不跨会话）；前置不满足 →
// `{ok:false, reason}`（不抛错、不静默）；提取范围同自动路径（各进度 → 最新轮；无新轮即跳过）。
func (p *Plugin) flush(payload []byte) map[string]any {
	var req struct {
		InstanceID string `json:"instance_id"`
		Session    string `json:"session"`
	}
	if err := json.Unmarshal(payload, &req); err != nil || req.InstanceID == "" || req.Session == "" {
		return map[string]any{"ok": false, "reason": "instance_id/session required"}
	}
	p.enqueue(turnEvent{InstanceID: req.InstanceID, Session: req.Session}, true)
	return map[string]any{"ok": true, "queued": true, "session": req.Session}
}

// resolveConfig 经 data-prj-config-list 一次读回项目配置并解析 memory.*（缺失回落默认）。
func (p *Plugin) resolveConfig(instanceID string) (memoryConfig, error) {
	cfg := memoryConfig{
		Enabled:    false,
		MinTokens:  p.opts.MinTurnTokens,
		Categories: map[string]bool{},
	}
	res, err := p.request(prjConfigListSubject, map[string]any{"instance_id": instanceID})
	if err != nil {
		return cfg, err
	}
	list, _ := res["list"].(map[string]any)
	if raw, ok := list[memoryEnabledKey]; ok {
		cfg.Enabled = strval(raw) == "true"
	}
	if raw, ok := list[memoryMinTokensKey]; ok {
		if n := atoiOK(strval(raw)); n > 0 {
			cfg.MinTokens = n
		}
	}
	// 单类别 token 告警阈值（prj 键；非正/缺失 → 0 = 不限）。
	if raw, ok := list[memoryCategoryMaxTokensKey]; ok {
		cfg.MaxTokens = atoiOK(strval(raw))
	}
	// 「提取子会话记忆」开关（prj 键；缺失/非 "true" → 关闭）。
	if raw, ok := list[memorySubsessionKey]; ok {
		cfg.Subsession = strval(raw) == "true"
	}
	for k, v := range list {
		if name, ok := strings.CutPrefix(k, memoryCategoryPrefix); ok && name != "" {
			cfg.Categories[name] = strval(v) == "true"
		}
	}
	return cfg, nil
}

// readUserConfig 经既有 data-user-config-load 面一次读回 usr 配置对象（失败 → nil）。
//
// 热生效（SL-C9）：**每次沉淀现读**（消息面零新增），不缓存到进程级/包级变量 →
// 配置改动无需重启，生效粒度 = 按 turn。消费点 = 「子系统默认 LLM」（`llm.memory`；SL-3）。
func (p *Plugin) readUserConfig(instanceID string) map[string]any {
	res, err := p.request(userConfigLoadSubject, map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil // 读失败 → 按"不指定"处置；沉淀流程不中断
	}
	cfg, _ := res["data"].(map[string]any)
	return cfg
}

// subsystemLLM 取记忆子系统的默认 LLM（provider name）：读 usr `llm.memory`（SL-3）。
// 取值 = llmref（**键缺失/空串 → 回落 defaultLLM 已由数据层读侧完成**，此处不重做，见 SL-1）；
// 折算（旧 int 索引 → provider name）走 data.LLMRefName 单一来源。返回 ""（无法折算 /
// 无可用 LLM / 读失败）→ llm-simple 请求**不带 `llm` 字段**（server 回落 exe flags 默认）。
func subsystemLLM(userCfg map[string]any) string {
	if userCfg == nil {
		return ""
	}
	return data.LLMRefName(userCfg[memoryLLMKey], userCfg["llms"])
}

// categoryInfo 是 memory 类别（持久化侧为唯一来源，插件不硬编码类别清单）。
// Prompt = 该类别的沉淀提示词（数据层按文件读序解析后随 data-memory-list 下发，OP-04）；
// 空串 = 无任何提示词文件（如新增自定义类别未建提示词文件）→ 该类**不提取**。
type categoryInfo struct {
	Category string
	Level    string
	Prompt   string
}

// memoryCategories 经 data-memory-list 取类别清单。
func (p *Plugin) memoryCategories(instanceID string) ([]categoryInfo, error) {
	res, err := p.request(memoryListSubject, map[string]any{"instance_id": instanceID})
	if err != nil {
		return nil, err
	}
	raw, _ := res["list"].([]any)
	out := make([]categoryInfo, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		cat := strval(m["category"])
		if cat == "" {
			continue
		}
		out = append(out, categoryInfo{
			Category: cat,
			Level:    strval(m["level"]),
			Prompt:   strval(m["prompt"]), // 数据层解析的沉淀提示词（OP-04）
		})
	}
	return out, nil
}

// loadTurnMessages 经 data-session-load-messages 读本轮全部消息。
func (p *Plugin) loadTurnMessages(instanceID, dialogID, turnID string) ([]data.ChatMsg, error) {
	payload := map[string]any{"turn_id": turnID}
	if dialogID != "" {
		payload["dialog_id"] = dialogID // 一树一库：消息在对话库
	}
	res, err := p.request(sessionLoadSubject, map[string]any{
		"instance_id": instanceID,
		"data":        payload,
	})
	if err != nil {
		return nil, err
	}
	raw, _ := res["messages"].([]any)
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var msgs []data.ChatMsg
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// memoryRead 读某类全文（不存在 → 空串，不算失败）。
func (p *Plugin) memoryRead(instanceID, category string) (string, error) {
	res, err := p.request(memoryReadSubject, map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"category": category},
	})
	if err != nil {
		return "", err
	}
	d, _ := res["data"].(map[string]any)
	return strval(d["content"]), nil
}

// memorySave 保存某类全文。
func (p *Plugin) memorySave(instanceID, category, content string) error {
	_, err := p.request(memorySaveSubject, map[string]any{
		"instance_id": instanceID,
		"data":        map[string]any{"category": category, "content": content},
	})
	return err
}

// rewrite 经无上下文单轮 LLM（llm-simple）重写类别全文：读旧全文 + 本轮新信息 → 新全文。
// instanceID 为触发事件（session-complete）载荷中的实例 id，随请求透传（61-消息一览 §0
// 实例字段必带）。
// llm = 本子系统的默认 LLM（provider name，见 subsystemLLM）；空则**不带该字段**
// （载荷与改前逐字节等价，server 回落 exe flags 默认）。
// system = 该类别的沉淀提示词（取自 data-memory-list 的 `prompt`，OP-04）；调用方已保证非空
// （无提示词文件的类别在 distill 处即跳过），此处仍兜底拒绝空白（不臆造默认 —— 代码内不留副本）。
func (p *Plugin) rewrite(instanceID, llm, system, category, oldText, newInfo string) (string, error) {
	if p.deps.Bus == nil {
		return "", errors.New("no bus (llm-simple unavailable)")
	}
	if strings.TrimSpace(system) == "" {
		return "", errors.New("empty memory prompt (no prompt file for category " + category + ")")
	}
	prompt := fmt.Sprintf("【类别】%s\n\n【现有全文】\n%s\n\n【本轮新增信息】\n%s", category, oldText, newInfo)
	ctx, cancel := context.WithTimeout(context.Background(), llmTimeout)
	defer cancel()
	req := map[string]any{"prompt": prompt, "system": system, "instance_id": instanceID}
	if llm != "" {
		req["llm"] = llm // SL-2 可选字段：子系统默认 LLM
	}
	v := p.deps.Bus.Emit(ctx, llmSimpleSubject, req).Wait()
	if err := v.Err(); err != nil {
		return "", err
	}
	if m, ok := v.Result.(map[string]any); ok {
		if text, ok := m["text"].(string); ok && text != "" {
			return text, nil
		}
	}
	return "", errors.New("llm-simple empty result")
}

// renderTurnInfo 把本轮消息渲染为自然文本（供沉淀 LLM 阅读）。
func renderTurnInfo(msgs []data.ChatMsg) string {
	var b strings.Builder
	for _, m := range msgs {
		role := m.Role
		if role == "" {
			role = "msg"
		}
		fmt.Fprintf(&b, "[%s] %s\n", role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "  → 工具 %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
		}
	}
	return b.String()
}

// ─── data-* 请求-响应（persist 把应答发布到请求同主题，须按 req_id 关联收敛）──

// newReqID 生成一次请求的唯一 id（前缀 memory- 仅用于日志辨识，不参与过滤语义）。
//
// 为何**不用** `time.Now().UnixNano()`（I-78）：Windows 上该时钟实际刻度 ≈1ms
// （实测 999900ns），同一 tick 内并发发出的请求会拿到**相同**数值 → 撞号；
// 应答按 req_id 等值过滤随即失效，后发请求会收下先发请求的应答
// （记忆库 8 类并行读取 → 类别串味）。
//
// 为何**用随机 id** 而非 history/codegraph/vfts 的 `UnixNano + atomic.Uint64`：
// 自增序号能消除同 tick 撞号，但仍带时间戳（跨进程/跨实例/时钟回拨面存在）；
// 本插件每轮每类仅发一次请求，量级极低，随机 id 无包级可变状态、无需加锁、
// 与刻度完全解耦，最贴合"请求自身唯一"的语义。随机源用标准库 crypto/rand
// （Text() 返回 26 字符 base32 随机串，零新增第三方依赖）。
func newReqID() string {
	return "memory-" + rand.Text()
}

// request 发一次 data-* 请求并等应答（镜像 plugin/dataclient.Emit 形态）。
func (p *Plugin) request(subject string, req map[string]any) (map[string]any, error) {
	if p.deps.Bus == nil {
		return nil, errors.New("no bus")
	}
	reqID := newReqID()
	req["req_id"] = reqID
	done := make(chan map[string]any, 1)
	sub, err := p.deps.Bus.On(subject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m struct {
			ReqID  string         `json:"req_id"`
			OK     *bool          `json:"ok"`
			Result map[string]any `json:"result"`
		}
		if json.Unmarshal(v.Payload, &m) != nil || m.ReqID != reqID || m.OK == nil {
			return nil // 他人请求 / 非应答载荷 忽略
		}
		select {
		case done <- m.Result: // ok=false → Result 为 nil，下方按失败处理
		default:
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	defer sub.Unsubscribe()
	f := p.deps.Bus.Emit(context.Background(), subject, req)
	v := f.Wait()
	if err := v.Err(); err != nil {
		return nil, err
	}
	select {
	case res := <-done:
		if res == nil {
			return nil, fmt.Errorf("%s 失败应答", subject)
		}
		return res, nil
	case <-time.After(dataTimeout):
		return nil, fmt.Errorf("%s 应答超时", subject)
	}
}

// strval 任意值 → 字符串（配置/结果值归一）。
func strval(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// atoiOK 解析十进制整数（失败 → 0）。
func atoiOK(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
