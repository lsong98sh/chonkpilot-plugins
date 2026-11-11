---
plugin: task
tool: task_stop
contract: src/tools/task/task_stop.tool.md
handler: src/main.go:684（插件）→ E:/BizWorks/chonkpilot/src/lib/llm/server/plugin_caps.go:107（能力）→ E:/BizWorks/chonkpilot/src/lib/llm/server/handler_task.go:23（onTaskStop）
reviewed_at: 2026-11-10
status: verified
---

# task_stop 审查

> 2026-11-08 旧审查的 P0（data.write 成功被报超时）与 P1（cascade 被忽略）均已失效：插件已改走 `plugin/v1/task.stop`（带 `request_id`，直接读取宿主 `v.Result`），`cascade=false` 已真正生效。

## 1. 一句话结论

当前可用：成功终止在毫秒级返回 `{ok,cancelled,task_id}`；`cascade` 按契约生效；2026-11-10 补齐 **命中 `ask_user` 任务时撤销提问**，不再出现“任务节点已取消、提问仍可作答”的分裂状态。

## 2. 调用链

1. LLM `tool_call task_stop`
2. turn 建节点/worker → gateway → task 插件 MCP `tools/call`
3. `taskPlugin.stop`（插件 `src/main.go:684-717`）：校验 `instance_id/task_id`，`cascade` 缺省 true；调 `plugin/v1/task.stop{request_id,instance_id,task_id,cascade}`，30s 超时
4. 宿主能力（`plugin_caps.go:107-122`）→ `onTaskStop`（`handler_task.go:23-93`）：
   - id 归一：`task_id` 优先，缺省按 `tool_call_id` 反查；
   - 层判不可逆终态 → 明确拒绝；
   - `cascade=true` → `s.tasks.cancelSubtree`（层权威可取消集合 + `ExecSink.CancelExec` 真打断）+ `closeDelegatedInSubtree`；
   - `cascade=false` → `cancelNodeOnly`（只停目标节点/其委派子轮次）；
   - **ask 撤销**：`cancelAsksForStoppedTasks`（见 §5）；
   - 写 `v.Result={cancelled:true,task_id,cascade}`。
5. 插件返回 `{ok:true,cancelled:true,task_id}`。

## 3. 契约面

- YAML：`async: never`、`timeout: 120`、`idempotent: false`。
- 参数：`task_id`（required）、`cascade`（可选，缺省 true）。
- 返回：`{ok:true,cancelled:true,task_id}`；失败返回明确错误文本。
- 摘要已写明：目标任务或其级联子树包含 `ask_user` 时，提问随任务终止撤销；`task_stop` 由 LLM 发起时同样适用。

## 4. 实现面

- 校验：`cx.InstanceID` 为空 / `task_id` 为空 → 插件级错误。
- `cascade`：显式 bool 才传给宿主，缺省 true。
- 终态拒绝：层已 done/error/cancelled → `任务已结束（<st>），无需停止`。
- 执行侧取消：层持 `gw_task_id` 时经进程内 sink `CancelExec` 真打断；sink 缺失且确有在飞执行 → 明确报错。
- 文本/结果：成功只写 `v.Result`；插件经 `CallIntoWithTimeout` 直接拿到，不再等 data 面 `{req_id,ok}` 信封。

## 5. 与 ask_user 的交互（2026-11-10 修复）

旧行为：`onTaskStop` 只取消任务节点，不碰 `s.asks`/Ask 中心/tool-result。LLM 或 UI 停掉 ask 节点后：
- 任务节点 `cancelled`，但提问仍可作答；
- 答复到达仍会作为用户消息投递，`tasks.done(answer)` 因节点已终态而被幂等忽略；
- 无 timeout 时 ask 登记永久残留。

现行语义（与 `llm-cancel` 的 `turn_closed` 对齐）：

1. **取消前**快照目标节点或子树中的非终态节点 id（`handler_task.go:64-72`）。
2. 取消成功后 `cancelAsksForStoppedTasks`（`handler_ask.go:405-418`）：
   - `clearAsksByTaskLocked`（`instancescope.go:210-229`）按 `w.taskID` 摘除 `s.asks`、丢弃批次已收答复、停 timer；
   - tool-result 改写为 `cancelled` + `提问已终止（任务已停止）`（`updateAskToolResultStatus`，`handler_ask.go:369-395`），保留 `ask_id`；
   - Ask 中心 remove（reason=`task_stopped`）；
   - **不**向 LLM 投递消息（当前轮占位已消费）。
3. `cascade=false` 只撤销目标 ask；`cascade=true` 撤销子树内全部非终态 ask。
4. 之后再作答：`takeAskLocked` 未命中 → `replyExpiredAsk` 回查落库行 `status=cancelled` → 明确失败（`ask 已回答或已关闭`）。

## 6. 剩余问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P2 | 无会话/轮次作用域限制：同 instance 内任意 `task_id` 可停 | `handler_task.go:23-93` | LLM 误停他会话任务（id 难猜，风险低） |
| P2 | `idempotent:false` 与“二次停止返回业务错误、状态不变”略不符 | 契约 `:10`；`handler_task.go:45-50` | 文档级 |
| P3 | 返回无 `state` 字段；契约示例未含 `cascade` | 契约 `:37-41` | 模型需按 `ok/cancelled` 判定 |

## 7. 测试/验收

- `TestEngineTurnAskTaskStop`：ask 占位后 `task-stop{task_id}` → tool 行 `cancelled`、任务节点 `cancelled`、`s.asks/s.askBatches` 清空、无 `【用户回答】` 消息。
- 既有：`TestTaskStopCascadeCancel` / `TestTaskStopCascadeFalseOnlyTarget` / `TestTaskStopDuringAwaiting` / `TestTaskStopByIdAndToolCall`。
- 回归：宿主 `go test ./src/lib/llm/server` 全量通过。