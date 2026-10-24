// chonkpilot-memory —— 记忆沉淀插件（**进程外 exe 形态**，2026-10-24 从宿主编译期内嵌改为外挂）。
//
// 业务实现 **逐字复用** `github.com/chonkpilot/chonkpilot-plugin-memory`（原 server 内嵌包）：
// 触发点（session-complete 每轮终态 + memory.flush 手动）、门控（memory.min-turn-tokens /
// memory.subsession / memory.enabled）、锚点推进（memory_extract 专用表）、子会话范围、
// per-(workdir, 类别) 串行/互斥、失败提示文案与数据落点**全部不变** —— 本 main 只做
// 「通道适配」（见 bridgebus）：把宿主 mq 总线的一个受限主题子集映射到插件桥能力面。
//
// 与宿主相关的三处接驳：
//   - Deps.Bus = bridgebus（mq.Bus 实现）：data-* → plugin/v1/data.*；llm-simple →
//     plugin/v1/llm.step；tool-notify → plugin/v1/tool.notify；memory.flush 等入站主题经
//     plugin.subscribe（事件）/ plugin.tool.bind（请求-应答）；
//   - Deps.Logf = 插件日志（宿主重定向到 <插件数据目录>/logs/plugin.log）；
//   - Deps.Notify = 插件失败的用户可见提示（与宿主原实现同口径：同 (实例,会话,轮次,插件,
//     类别) 只提示一次 → 既有通知面 tool-notify，**不新增主题/payload 字段**）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
	"github.com/chonkpilot/chonkpilot-plugin/bridgebus"
	memory "github.com/chonkpilot/chonkpilot-plugin-memory"
	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "memory",
		Name:    "记忆沉淀",
		Version: pluginVersion,
		// 单次宿主调用上限：覆盖插件侧 llmTimeout（60s）；data-* 等短调用由 bridgebus 另行收敛到 5s。
		Timeout:  120 * time.Second,
		Features: map[string]any{"mq": true},
	})
	if err != nil {
		slog.Error("memory: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	local, err := mq.New(mq.Options{})
	if err != nil {
		slog.Error("memory: 建本地派发总线失败", "err", err)
		os.Exit(1)
	}
	bus := bridgebus.New(local, bridgebus.Options{
		Read:  p.DataRead,
		Write: p.DataWrite,
		Call:  p.Call,
		Subscribe: func(pattern string, fn func(ctx context.Context, topic string, payload []byte)) {
			p.Subscribe(pattern, fn)
		},
		Bind: func(subject string, fn func(ctx context.Context, payload json.RawMessage) (any, error)) error {
			return p.BindSubject(subject, fn)
		},
	})
	defer bus.Close()

	logf := func(format string, args ...any) { p.Logger().Info(fmt.Sprintf(format, args...)) }

	plug := memory.New(memory.DefaultOptions())
	deps := plugin.Deps{Bus: bus, Logf: logf, Notify: newNotifier(bus).Notify}
	if err := plug.Start(deps); err != nil {
		slog.Error("memory: 启动失败", "err", err)
		os.Exit(1)
	}
	defer plug.Stop()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		slog.Error("memory: 运行结束", "err", err)
	}
}

// ─── 失败提示（Deps.Notify 的插件侧实现）──────────────────────
//
// 口径与改造前宿主实现（chonkpilot-llm/server/pluginnotice.go）一致：
//   - 判重：同 (实例, 会话, 轮次, 插件, 类别) 只提示一次（键表 FIFO 淘汰，上限 512）；
//   - 呈现：既有通知面 tool-notify（notice=plugin-failure），字段与文案逐字不变；
//   - 不阻塞：只发布不等应答；实例字段缺失 → 不广播（61 §0）。

// notifyWhat 插件名 → 用户可读措辞（与宿主同名表一致；memory 为本插件自身）。
var notifyWhat = map[string]string{
	"memory":   "记忆沉淀",
	"compress": "上下文压缩",
}

// notifyDedupCap 判重表上限（超出按插入序淘汰最旧键）。
const notifyDedupCap = 512

// notifier 是「同轮同类只提示一次」判重 + tool-notify 投递（记忆类别并行沉淀 → 并发安全）。
type notifier struct {
	bus mq.Bus

	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// newNotifier 构建判重表。
func newNotifier(bus mq.Bus) *notifier {
	return &notifier{bus: bus, seen: map[string]struct{}{}}
}

// Notify 实现 plugin.Deps.Notify。
func (n *notifier) Notify(v plugin.Notice) {
	if n == nil || n.bus == nil || v.Plugin == "" || v.InstanceID == "" {
		return
	}
	kind := v.Kind
	if kind == "" {
		kind = "failed"
	}
	key := strings.Join([]string{v.InstanceID, v.Session, v.Turn, v.Plugin, kind}, "\x00")
	if !n.mark(key) {
		return
	}
	what := notifyWhat[v.Plugin]
	if what == "" {
		what = v.Plugin
	}
	reason := strings.TrimSpace(v.Reason)
	if reason == "" {
		reason = "原因未知（详见日志文件）"
	}
	n.bus.Emit(context.Background(), "tool-notify", map[string]any{
		"instance_id": v.InstanceID,
		"session_id":  v.Session,
		"turn_id":     v.Turn,
		"notice":      "plugin-failure",
		"plugin":      v.Plugin,
		"kind":        kind,
		"reason":      reason,
		"message":     "⚠️ " + what + "失败：" + reason + "（本轮对话未受影响；详情见日志文件）",
		"message_id":  "msg-pnotice-" + noticeKeyID(key),
	})
}

// mark 登记键：首次出现返回 true（应提示），重复返回 false。
func (n *notifier) mark(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.seen[key]; ok {
		return false
	}
	n.seen[key] = struct{}{}
	n.order = append(n.order, key)
	for len(n.order) > notifyDedupCap {
		old := n.order[0]
		n.order = n.order[1:]
		delete(n.seen, old)
	}
	return true
}

// noticeKeyID 由判重键派生稳定短 id（同一键 → 同一 id；与宿主实现同算法）。
func noticeKeyID(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%08x", h.Sum32())
}
