---
plugin: task
tool: task_llm
contract: src/tools/task/task_llm.tool.md
handler: src/main.go:266
reviewed_at: 2026-11-08
status: verified
---

# task_llm 审查

## 1. 一句话结论

不建议当前形态：契约默认“等到子代理收尾”，但插件→宿主的桥调用被 SDK 默认 **15s** 超时截断，**任何超过 15 秒的子代理都会让工具直接报错**，子任务继续在后台跑且 `task_id` 丢失；此外 `agent` 拼错会静默退化为全能力子代理。

## 2. 调用链

1. LLM `tool_call task_llm` → `turn.dispatchToolCall`（建节点、running 行、独立 worker，`turn.go:1522-1578`）。
2. `gatewayArgs` 注入 `async=always`（`turn.go:342-358`，默认配置下；用户 `tool_async` 覆盖例外）→ gateway `doCall` 以 `always` 档位立即 pending、后台执行、**无超时点**（`mcpgateway.go:1027-1037`）。
3. 插件 handler `taskPlugin.llm`（`src/main.go:266-296`）→ `p.CallInto(ctx,"plugin/v1/agent.delegate",in,&out)`。
4. `p.Call` 给桥调用套 `context.WithTimeout(ctx, p.opts.Timeout)`，`Options.Timeout` 未设置 → 默认 `defaultWait=15s`（`E:/BizWorks/chonkpilot-framework/plugin/src/sdk/plugin.go:39,54,149-151,316-331`；`frame.go:353-358`）。
5. 宿主 `plugin/v1/agent.delegate`：`startChildTurn`（独立子会话/子轮次，`subllm.go:42-70`）→ 建 `kind=llm` 任务节点 → `detach` 立即回 running；否则 `delegatedWait`，`wait<=0` **阻塞到子轮次结束**（`plugin_caps.go:236-252`、`subllm.go:276-296`）。
6. 完成经 `mcp-tasks-report` 回执 → message 表 → 续轮（`handler_task.go:128-181`、`turn.go:1257-1293`）。

## 3. 契约面

YAML：`async: never`、`timeout: 600`（`task_llm.tool.md:3-13`）。参数：`prompt`（唯一 required）、`agent`、`session_id`、`title`、`waitSec`、`detach`（`task_llm.tool.md:20-52`）。返回：`{ok,session_id,turn_id,task_id,state,text}`；`state=running` 不带正文；`state=error,code=LLM_INCOMPLETE` 带 `partial`（`task_llm.tool.md:54-70`）。

契约问题：
- `waitSec` “0/缺省 = 等到收尾”（`task_llm.tool.md:44`）与 SDK 15s 桥超时**直接矛盾**（P0）。
- 未说明 `waitSec` 实际有效上限（≤15s 才有机会拿到终态）；schema 无 minimum/maximum，负值会被当作“缺省=等到收尾”。
- `async: never`/`timeout: 600` 同样未下发 gateway（见 task_run §9），实际 `always`（无超时点）。
- `agent` 描述“收窄提示词/工具白名单/LLM”（`task_llm.tool.md:32`），但实现未对未知 agent 报错（见 P1-1）。
- `title` 称“缺省取 prompt 首行”（`task_llm.tool.md:40`），实现取整段 prompt 压成单行后截 60 rune（`plugin_caps.go:214-217`、`527-535`）。
- 未给出 `session_id` 续用约束（并发/重复使用时宿主 `busy` 键会被覆盖，见 P2）。
- 未声明嵌套深度/并发上限（子代理可再次委派，无深度闸门）。

## 4. 实现面

- 解析：`prompt` `TrimSpace` 后空 → 插件级错误；`cx.Turn` 为空 → 错误（`main.go:267-273`）。
- 转发字段：`instance_id/turn/session/tool_call_id/prompt/agent/session_id/title/purpose`；`wait_sec` 仅在 `waitSec>0` 时带上；`detach` 仅在 bool 时带上（`main.go:274-290`）。因此 `waitSec<=0` 或负数 → 宿主视角“未设置” → `wait=0` → 一直等（`plugin_caps.go:244-247`）。
- 宿主行为：
  - 新会话 id 缺省 `sub-<id>`；`EnsureSessionWithParent(child, 父会话)`、`EnsureTurn`（`subllm.go:43-50`）。
  - 子轮次 ctx = `context.Background()`（不随父收尾取消），取消经 `task_stop` → `closeDelegatedTurn` 递归关子树（`plugin_caps.go:172-206`、`subllm.go:134-174`）。
  - 任务节点挂在发起该调用的工具节点下，`title` 缺省 `oneLine(prompt,60)`（`plugin_caps.go:207-223`）。
  - 终态映射：`complete` 且有正文 → `done`；`incomplete` → `error` + `code=LLM_INCOMPLETE` + `partial`；`interrupted/cancelled` → `state=cancelled`；空回复 → `EMPTY_REPLY`（`plugin_caps.go:479-525`）。
- 插件的桥超时：ctx 到期后 `conn.Call` 丢弃 pending 返回 `context deadline exceeded`（`frame.go:353-358`）；宿主 handler 忽略 ctx（`func(_ context.Context, ...)`，`plugin_caps.go:172`）继续阻塞到子轮次结束并推进节点终态。
- 副作用：新建子会话/轮次、子 LLM 调用（真实计费）、任务节点、可能的下一级委派。
- 幂等/并发：`idempotent:false` 正确；同 `session_id` 并发调用会互相覆盖 `busy[instance,session]`（`subllm.go:62`）且共享会话上下文，属**未定义行为** [推断]。
- 资源清理：无插件侧 goroutine/句柄；子轮次由宿主管理；桥超时后宿主与插件的一次请求 goroutine 会滞留到子轮次结束（有界但不理想）。

## 5. 返回面：LLM 实际看到的所有形态

> 生产链路强制 `async=always`：LLM 不直接看到插件返回体，而是看到 `mcp-tasks-report` 回执文本。

A. 子代理正常完成（`state=done`）——LLM 看到完整 JSON：
`{"ok":true,"session_id":"sub-…","turn_id":"…","task_id":"tk-…","state":"done","text":"<子代理最终回复>"}`（`plugin_caps.go:504-516`）。

B. 子代理输出不完整——`{"ok":false,"session_id", "turn_id", "task_id", "state":"error","code":"LLM_INCOMPLETE","error":"子代理输出不完整","partial":"<已产出>"}`；`partial` 非终态（`plugin_caps.go:511-516`、契约 66-70）。

C. 子代理被取消/中断——`{"ok":false,...,"state":"cancelled","error":"子代理未完成（cancelled|interrupted）"}`（`plugin_caps.go:517-518`）。

D. 空回复——`{"ok":false,...,"state":"error","code":"EMPTY_REPLY","error":"子代理返回空回复"}`（`plugin_caps.go:519-520`）。

E. `detach=true` 或 `waitSec` 到期（≤15s 内）——`{"ok":true,"session_id","turn_id","task_id","state":"running","message":"子代理仍在运行；结果请用 task_status(task_id=…) 获取"}`；**不带正文**（`plugin_caps.go:236-243`）。

F. **默认路径（>15s）——P0**：插件桥超时 → 插件把 Go error 包成 MCP `isError`，正文仅 **`context deadline exceeded`**（无 JSON、无 `task_id`）。注意网关执行池把 `res.IsError` 仍记为 `state=done`（`mcpgateway.go:987-996`、`tasks.go:306-345` 执行结束只看 `err`），所以回执是 `state=done` + 该裸文本，**不是** `任务结束(error)`；`isError` 不进入 LLM 工具结果。LLM 既拿不到子代理结果，也拿不到 task_id，无法 `task_status`/`task_stop`；子轮次继续在后台运行。

G. 参数级错误：`prompt 不能为空` / `缺少会话轮次（turn）…` / 宿主 `turn_not_found` / `delegate_failed`——裸文本 isError；经回执照样作为 `done` 文本投喂（`main.go:268-273`、`plugin_caps.go:189-204`）。

H. 宿主任务节点/会话创建失败：`delegate_failed: <err>`（`plugin_caps.go:201-204`、`224-227`），裸文本。

I. 父轮次取消：已登记 pending 的调用随轮次结束，LLM 不再收到该工具结果；子轮次按设计**不**随父取消（`subllm.go:40-41`），可能继续消耗额度，需 `task_stop`。

J. 输出体积：`text` 为子代理最终回复全文，无截断/无长度上限（受子代理上下文限制）；`partial` 可同样很长。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | 契约“缺省等到收尾”被 SDK 15s 桥超时击穿：>15s 的子代理一律工具报错，子任务继续跑、task_id 丢失、LLM 只看到裸文本 `context deadline exceeded`（`isError` 被抹平为任务 `done`） | `task_llm.tool.md:44`；`sdk/plugin.go:39,54,149-151,316-331`；`main.go:74-76`（未设 Timeout）；`plugin_caps.go:244-251`；`subllm.go:284-286` | 绝大多数真实委派失败且不可恢复 |
| P1 | `agent` 名拼错/不存在时静默退化为全能力子代理（无 persona、无工具收窄） | `turn.go:660-682`（`resolveAgentDef` 未命中返回空定义）；`turn.go:456-459`（`allowedTools=nil` → 不限制） | 误以为已收窄权限，实际放大权限 |
| P1 | **传输层级**失败（provider 断开/transport error，`err != nil`）只回 `任务结束(error)`，回执里的 `result_summary` 被丢弃；子轮次成为调用方不可见的孤儿 | `turn.go:1278-1280`；`mcpgateway.go:391-421`（report 带全文） | 无恢复路径；额度/任务泄漏 |
| P2 | 同 `session_id` 并发/在跑时复用会覆盖宿主 `busy` 键并共享上下文 | `subllm.go:59-63` | 会话状态串扰 [推断] |
| P2 | `title` 缺省与契约不一致（整段 prompt 60 rune，而非首行） | `plugin_caps.go:214-217`、`527-535`；契约 38-41 | 展示差异，无功能影响 |
| P2 | `waitSec` 无范围校验：负数/0 都等于“无限等”，但实际被 15s 截断，模型的“短等”意图无法表达 | `main.go:285-287`；schema 42-45 | 参数易填错、失败无解释 |
| P2 | 无嵌套深度/并发限制，子代理可无限再委派 | `subllm.go:1-12`；`plugin_caps.go:164-253` | 递归放大与成本失控 |
| P2 | running 回执多出未文档化的 `message` 字段；契约示例未含 `code/partial/error` 全量形状 | `plugin_caps.go:236-243`；契约 54-70 | 模型需容错解析 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| **给 `plugin/v1/agent.delegate` 的调用单独放大超时**（如 `p.Call` 可选 timeout，或新增 `CallWithTimeout`；对 delegate 用 `waitSec*1s+缓冲`，缺省 ≤ 30s 而不是无上限） | 优化（correctness） | 默认路径可用 | 中，`main.go` + framework `sdk/plugin.go` | 需同步契约默认语义 | 建议现在做（P0 必修） |
| 缺省改为“有界等待 + 自动 detach”，返回 running+task_id，由 LLM 轮询 | 改善 | 超长任务可继续、可停止 | 低，`main.go:285-289` + 契约说明 | 语义变化，需改契约/测试 | 建议现在做 |
| `agent` 未解析时直接报错并提示可用 agent 名 | 改善 | 权限/身份符合预期 | 低，宿主 `plugin_caps.go:172-201` 或插件侧预校验 | 需新增 agent 列表能力/错误文案 | 排期做 |
| 传输层级失败时保留 `result_summary`（把 `任务结束(error)` 换成带详情文本） | 优化 | 失败可诊断、可恢复 | 低，`turn.go:1278-1280` | 文本变化，前端可能依赖旧文案 | 建议现在做（宿主侧） |
| `waitSec` 加 `minimum:1, maximum:600`，并在契约写明“>15s 当前不可靠” | 改善 | 少踩坑 | 低，契约 | 无 | 建议现在做 |
| 返回体固定包含 `task_id`，即使桥超时也由宿主先返回半成品（两阶段：先 ack task_id，再回结果） | 增强 | 任何超时都能 `task_status`/`task_stop` | 高，需改协议/宿主 | breaking-ish | 排期做 |
| 嵌套深度上限（如 5 层）与同会话并发互斥 | 优化 | 防递归爆炸 | 中，宿主 `delegate` | 无 | 排期做 |
| `title` 缺省严格取 prompt 首行（与契约一致） | 改善 | 展示一致 | 低，`plugin_caps.go` | 无 | 仅记录 |

## 8. 建议的测试 / 验收（不执行）

1. 子代理人为 sleep 30s（如提示词要求长任务），缺省调用 → 断言当前 15s 失败；修复后断言拿到 done 或 running+task_id。
2. `waitSec=1` + 子代理 sleep 10s → 断言返回 running 且 task_id 可 `task_status` 取终态。
3. `detach=true` → 断言立即返回 running、随后 `task_status` 得到 done+text。
4. `agent="不存在的名字"` → 断言（期望）报错；当前断言会出现全能力子代理（复现 P1-1）。
5. 子代理被 `task_stop` → 断言 `state=cancelled`、子会话递归关闭。
6. 子代理输出被上游截断（模拟 finish_reason=length）→ 断言 `code=LLM_INCOMPLETE` + `partial`。
7. 同 `session_id` 连续两轮委派 → 断言上下文延续；并发两轮 → 断言有明确互斥/报错。
8. `prompt=""`、无 turn 上下文、宿主不可达 → 断言错误文本可读且不产生孤儿节点。
9. 父轮次取消后再 `task_status` 子 turn → 断言可查询、任务树归属正确。
10. 15s 桥超时场景下检查网关最终回执与任务节点终态是否一致（避免“工具失败但节点 done”）。

## 9. 基线修正（仅当共通基线不成立时）

- 同 task_run：**基线 6 不成立**。契约 `async/timeout` 只存在于插件 embed 的 md，不进 gateway；实际档位由宿主强制 `always`，因此“超时裁决 wait/cancel”对本工具在正常链路不可达。
- 基线 2（展示名）：`task_llm` 的 `title` 是工具**自有**参数，与 gateway 注入的 `tool_call_display_name`（→ `purpose`）并存；插件把 `cx.Purpose` 也转给宿主作为任务节点 purpose（`main.go:283`），两者不要混为一谈。