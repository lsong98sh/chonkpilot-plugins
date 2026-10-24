// 手动沉淀（memory.flush）**端到端集成**（真实 persist 服务 + 真实总线 + 真实插件，仅 LLM 打桩）：
// 与 chonkpilot-test/chonkpilot-gui/systest/run_memory_ctx.py 同法（mock LLM + 真落盘），
// 但不起 GUI/前端 —— 覆盖「立即沉淀确实产生写入」的可核证据：
//
//	① 消息回执：memory.flush 的 Value.Result = {ok, queued, session}（**投递即回**，I-128）；
//	   处理完成由通知面 tool-notify（`memory-start` → `memory-done`）判定；
//	② 落盘：<workdir>/.chonkpilot/memory/<类别>.md 内容变化（沉淀 LLM 产物）——记忆库唯一落点；
//	③ 广播：每次写入后 persist 发 data-memory-refresh（前端据此刻刷新，与既有面同源）；
//	④ 作用域：只写该会话最近一轮所在 instance 的记忆目录（不越界）。
package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chonkpilot/chonkpilot-data"
	"github.com/chonkpilot/chonkpilot-data/persist"
	"github.com/chonkpilot/chonkpilot-lib/mq"
	"github.com/chonkpilot/chonkpilot-plugin"
)

// call 发一次 data-* 请求并等 persist 应答（测试用最小客户端；与外部黑盒用例同法）。
func call(t *testing.T, bus mq.Bus, subject string, body map[string]any) map[string]any {
	t.Helper()
	body["req_id"] = "t-" + subject
	ch := make(chan map[string]any, 1)
	sub, err := bus.On(subject, 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) != nil {
			return nil
		}
		if _, hasOK := m["ok"]; !hasOK {
			return nil // 请求无 ok，应答有
		}
		select {
		case ch <- m:
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatalf("sub %s: %v", subject, err)
	}
	defer sub.Unsubscribe()
	raw, _ := json.Marshal(body)
	bus.Emit(context.Background(), subject, raw).Wait()
	select {
	case m := <-ch:
		return m
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting reply for %s", subject)
		return nil
	}
}

// TestManualFlushIntegrationWritesMemoryFiles：一次 memory.flush → 真落盘 + 回执 + 刷新广播。
func TestManualFlushIntegrationWritesMemoryFiles(t *testing.T) {
	data.Reset()
	bus, err := mq.New(mq.Options{Prefix: "chonk."})
	if err != nil {
		t.Fatalf("mq.New: %v", err)
	}
	// 临时目录先建：其清理（RemoveAll）恒晚于下方注册的资源回收（LIFO）→ 关闭库后再删目录
	usrDir := t.TempDir()
	wd := t.TempDir()
	svc := persist.New(bus, persist.Options{UsrPath: filepath.Join(usrDir, "usr.db")})
	if err := svc.Start(); err != nil {
		t.Fatalf("persist.Start: %v", err)
	}
	t.Cleanup(func() {
		svc.Stop()
		data.Reset() // 关闭缓存库连接（Windows 下占用文件会阻塞目录清理）
		_ = bus.Close()
	})

	// 实例登记（persist 据此解析 work_dir / data_dir）
	reg, _ := json.Marshal(map[string]any{
		"instance_id": "ins-int", "client_type": "unittest", "work_dir": wd,
	})
	bus.Emit(context.Background(), "instance-register", reg).Wait()
	time.Sleep(30 * time.Millisecond)
	data.Register("ins-int", wd, "")

	saveCfg := func(key, value string) {
		r := call(t, bus, "data-prj-config-save", map[string]any{
			"instance_id": "ins-int",
			"data":        map[string]any{"key": key, "value": value},
		})
		if ok, _ := r["ok"].(bool); !ok {
			t.Fatalf("save %s=%s failed: %+v", key, value, r)
		}
	}
	saveCfg("memory.enabled", "true")
	// 阈值极大：反证手动沉淀**不受** memory.min-turn-tokens 门控（自动沉淀同配置下必被挡）
	saveCfg("memory.min-turn-tokens", "999999")
	// 本用例按旧布局（会话行落 prj 库、调用不带 dialog_id）取证，显式关闭分库
	saveCfg("session.db.shard", "off")

	// 真实落库：会话 + 最近一轮 + 该轮消息
	call(t, bus, "data-session-ensure-session", map[string]any{
		"instance_id": "ins-int", "data": map[string]any{"session_id": "s-int"},
	})
	call(t, bus, "data-session-ensure-turn", map[string]any{
		"instance_id": "ins-int", "data": map[string]any{"turn_id": "t-1", "session_id": "s-int"},
	})
	call(t, bus, "data-session-ensure-turn", map[string]any{
		"instance_id": "ins-int", "data": map[string]any{"turn_id": "t-2", "session_id": "s-int"},
	})
	for _, m := range []map[string]any{
		{"role": "user", "content": "决定采用 bbolt 做本地存储"},
		{"role": "assistant", "content": "好的，记录到用户决策"},
	} {
		r := call(t, bus, "data-session-append-message", map[string]any{
			"instance_id": "ins-int",
			"data":        map[string]any{"turn_id": "t-2", "msg": m},
		})
		if ok, _ := r["ok"].(bool); !ok {
			t.Fatalf("append-message failed: %+v", r)
		}
	}

	// mock LLM（llm-simple）：回显类别 → 便于逐类别断言落盘归属（同 run_memory_ctx.py 口径）
	if _, err := bus.On("llm-simple", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(v.Payload, &req)
		cat := "unknown"
		if i := strings.Index(req.Prompt, "【类别】"); i >= 0 {
			cat = strings.SplitN(req.Prompt[i+len("【类别】"):], "\n", 2)[0]
		}
		v.Result = map[string]any{"text": "DISTILLED-" + cat}
		return nil
	}); err != nil {
		t.Fatalf("stub llm-simple: %v", err)
	}

	// data-memory-refresh 收集（写入后 persist 主动广播）
	var mu sync.Mutex
	var refreshes []map[string]any
	if _, err := bus.On("data-memory-refresh", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) == nil {
			mu.Lock()
			refreshes = append(refreshes, m)
			mu.Unlock()
		}
		return nil
	}); err != nil {
		t.Fatalf("sub refresh: %v", err)
	}

	// 启用态首次 list → persist 预置 9 个类别文件（项目级 8 个落 <wd>/.chonkpilot/memory）
	if ok, _ := call(t, bus, "data-memory-list", map[string]any{"instance_id": "ins-int"})["ok"].(bool); !ok {
		t.Fatal("data-memory-list 失败")
	}
	memDir := filepath.Join(wd, ".chonkpilot", "memory")
	readDir := func() map[string]string {
		out := map[string]string{}
		entries, err := os.ReadDir(memDir)
		if err != nil {
			t.Fatalf("读记忆目录: %v", err)
		}
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(memDir, e.Name()))
			info, _ := e.Info()
			mt := ""
			if info != nil {
				mt = info.ModTime().Format(time.RFC3339Nano)
			}
			out[e.Name()] = string(b) + "|mtime=" + mt
		}
		return out
	}
	before := readDir()
	if len(before) < 8 {
		t.Fatalf("预置类别文件不足 8 个：%+v", before)
	}

	// 真插件（Start 订阅 session-complete + memory.flush）
	p := New(DefaultOptions())
	if err := p.Start(plugin.Deps{Bus: bus, Logf: t.Logf}); err != nil {
		t.Fatalf("plugin Start: %v", err)
	}

	// tool-notify 通知面收集（I-128：进度经既有通知面推送）
	noticeCh := make(chan string, 32)
	if _, err := bus.On("tool-notify", 0, func(_ context.Context, _ string, v *mq.Value) error {
		var m map[string]any
		if json.Unmarshal(v.Payload, &m) == nil {
			select {
			case noticeCh <- strval(m["notice"]):
			default:
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("sub tool-notify: %v", err)
	}

	// 触发手动沉淀（payload 与前端 mq.emit 一致：{instance_id, session}）——**投递即回**（I-128）
	v := bus.Emit(context.Background(), "memory.flush", map[string]any{
		"instance_id": "ins-int", "session": "s-int",
	}).Wait()
	if err := v.Err(); err != nil {
		t.Fatalf("memory.flush 派发错误：%v", err)
	}
	res, _ := v.Result.(map[string]any)
	if res == nil || res["ok"] != true {
		t.Fatalf("memory.flush 回执异常：%#v", v.Result)
	}
	if queued, _ := res["queued"].(bool); !queued {
		t.Fatalf("memory.flush 应投递即回（queued=true，不再同步等）：%#v", res)
	}
	// ① 消息回执（promise 语义；异步处理 → 不含 saved/turn）
	t.Logf("raw evidence: flush ack=%v", res)

	// 等异步处理完成（tool-notify: memory-start → memory-done）
	seen := map[string]bool{}
	deadline := time.After(15 * time.Second)
	for !seen["memory-done"] {
		select {
		case n := <-noticeCh:
			seen[n] = true
		case <-deadline:
			t.Fatalf("等 memory-done 超时（已见通知=%v）", seen)
		}
	}
	if !seen["memory-start"] {
		t.Fatalf("应广播 memory-start：%v", seen)
	}

	// ② 落盘证据：项目级类别文件内容变化且含 mock LLM 产物（逐类别归属）
	after := readDir()
	var changed []string
	for name, body := range after {
		if filepath.Ext(name) != ".md" {
			continue
		}
		cat := strings.TrimSuffix(name, ".md")
		if body == before[name] {
			continue
		}
		if !strings.Contains(body, "DISTILLED-"+cat) {
			t.Fatalf("类别 %s 内容非本类沉淀产物（串味/未写入）：%q", cat, body)
		}
		changed = append(changed, cat)
	}
	if len(changed) < 8 {
		t.Fatalf("项目级类别落盘数不足 8：changed=%v", changed)
	}
	// 用户偏好（用户级）亦落盘
	b, err := os.ReadFile(filepath.Join(usrDir, "用户偏好.md"))
	if err != nil || !strings.Contains(string(b), "DISTILLED-用户偏好") {
		t.Fatalf("用户偏好（用户级）未落盘/未写入：err=%v body=%q", err, string(b))
	}
	t.Logf("raw evidence: 记忆目录 %s 落盘 %d 个项目级类别 + 用户偏好", memDir, len(changed))

	// ③ 广播证据：save 后 data-memory-refresh（op=save、id=类别）
	mu.Lock()
	defer mu.Unlock()
	if len(refreshes) < len(changed) {
		t.Fatalf("refresh 广播数 %d < 写入类别数 %d：%+v", len(refreshes), len(changed), refreshes)
	}
	ops := map[string]bool{}
	ids := map[string]bool{}
	for _, r := range refreshes {
		ops[strval(r["op"])] = true
		ids[strval(r["id"])] = true
	}
	if !ops["save"] {
		t.Fatalf("应有 op=save 的刷新广播：%+v", refreshes)
	}
	for _, cat := range changed {
		if !ids[cat] {
			t.Fatalf("类别 %s 缺 data-memory-refresh(id=%s)", cat, cat)
		}
	}
	t.Logf("raw evidence: refresh ids=%v", ids)

	// ④ 作用域：写入只落在该 instance 的 work_dir 记忆目录（无越界文件）
	if _, err := os.Stat(filepath.Join(wd, "项目概要.md")); !os.IsNotExist(err) {
		t.Fatal("记忆文件不应落在 work_dir 根（须在 .chonkpilot/memory 内）")
	}
}
