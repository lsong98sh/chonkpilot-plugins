---
plugin: task
tool: subchat_open
contract: src/tools/task/subchat_open.tool.md
handler: src/main.go:679（插件）→ E:/BizWorks/chonkpilot/src/lib/llm/server/plugin_caps.go:440（能力）→ E:/BizWorks/chonkpilot/src/lib/llm/server/subchat.go:100（单聊）/ groupchat_sched.go:749（群聊）
reviewed_at: 2026-11-10
status: verified
---

# subchat_open 审查

> 2026-11-08 旧审查的部分 P1 已在当日修复：`participants` 坏项/重复项不再静默降级单聊（`groupParticipantsArg` 明确报错）；框架 `describe` 已返回完整 `# 摘要`，不再是“只取第一段”。本文按 2026-11-10 现状更新。

## 1. 一句话结论

可用。单聊/群聊主体、立即返回、结果不回流、参与者坏项报错均成立；2026-11-10 补齐：
- 插件透传 `is_error` → 失败落 failed + 任务节点 error；
- 成功返回 `mode=single|group`，模型可确认实际形态；
- 固定参数 `agent/scenario_id/llm` 在能力注册表 / usr `llms` 可用时做存在性校验；
- `prompt` 16384 上限、schema 上限/封闭、`# 返回值` 文档。

剩余为部分失败无回滚、父会话类型不限、非幂等重试多开等 P2/P3（见 §6）。

## 2. 调用链

LLM `subchat_open` tool_call
→ gateway（契约 `async: never` → `_meta.async=sync`、`timeout=0`；同步等待）
→ task 插件 `taskPlugin.subchatOpen`（`main.go:679`）：校验 `turn`，转发 `plugin/v1/subchat.open{instance_id,turn,display_name,args}`
→ 宿主能力（`plugin_caps.go:440`）→ `runSubChatOpen`（`subchat.go:100`）：
  1. 四必填非空 + `prompt <= 16384`；
  2. `groupParticipantsArg` 解析参与者（坏项/重复/非数组 → 明确错误；1 项=单聊、≥2 有效=群聊）；
  3. `validateSubChatRefs`（注册表/llms 可用时校验 agent/scenario/llm）；
  4. 单聊：建 `sub_chat` 会话 → 回填 dialog → 标题 → 建首轮 + 写 user 交接消息 → `subchat-opened` → 异步首轮；
  5. 群聊：建 `group_chat` 会话 → 回填 dialog → 标题 → 写名册 → 展示板首轮 + 主 agent 布置消息 → `subchat-opened` → 异步调度器；
  6. 返回 `{ok:true,session_id,mode}`（失败 `{ok:false,error}` + `isErr=true`）。
→ 插件：`is_error=true` 转 Go error → worker 落 failed + 任务节点 error；`false` 落 completed + done。

**结果不回流**：调用返回即终态；子聊天后续产出不会回到父会话。

## 3. 契约面

- YAML：`async: never`（解析为 sync）、`timeout: 0`、`runtime: plugin`。同步语义由契约 meta 决定（gateway 不再强制 always）；毫秒级返回。
- 摘要（框架返回完整摘要）已覆盖：单聊/群聊判定、立即返回、结果不回流、群聊 goal/playbook/convergence、主 agent 必须在名单内、`mode`。
- schema：顶层 `additionalProperties:false`；`agent/scenario_id/llm` maxLength 256、`prompt` 16384、`goal/playbook/convergence` 16384、`participants` maxItems 20 + 项内 `additionalProperties:false`、`role` 4000；四必填不变。
- 新增 `# 返回值`：`{ok,session_id,mode}`、失败 JSON、非幂等/重复调用多开、部分失败可能留下会话行、开窗失败由前端处理。

## 4. 实现面

- 参与者解析（`groupchat_sched.go:684`）：非数组/坏项/缺 agent/重复 agent → 明确错误；仅 1 项有效 → 单聊；有效 <2 且原始 ≥2 → 错误；`ensureMainParticipant` 保证主 agent 在名册（不在则置首位）。
- 存在性校验（`subchat.go:67`）：
  - usr `llms` 非空时，非 `auto` / 非内置 `echo` 的 provider 必须命中；
  - `s.agents` 非空时，场景必须存在且 main agent / participants 必须在场景团队或全局注册表；
  - 注册表/llms 为空（未注册能力、测试环境）→ 无法判定，保持宽松。
- 单聊：`SessionEnsure(kind=sub_chat,parent_id,lifecycle=chatting,agent/scenario_id/llm,purpose)` → `rememberDialog` → `SessionTitle`（失败仅日志）→ `EnsureTurn` → `AppendFullKeyed(user 交接)` → `subchat-opened` → `go startSubChatFirstTurn`。
- 群聊：`SessionEnsure(kind=group_chat,goal/playbook/convergence)` → `rememberDialog` → 标题 → `GroupParticipantsSave` → 展示板 `EnsureTurn` + `AppendMsgMap(user,sender_agent=main)` → `subchat-opened` → `go startGroupChat`。
- 部分失败无回滚：会话行/名册/首轮已建但后续失败 → 返回 `ok:false`，已建行保留（§6）。
- 非幂等：每次调用新建 `subchat-` / `group-` 随机 id；重试会多开。
- 开窗：`subchat-opened` 广播后由前端 `open-chat` 执行；窗口上限等失败工具不感知（契约已写明）。
- 异步首轮 panic 有 recover；不随父 ctx 取消（子聊天独立作业）。

## 5. 返回面

| 形态 | 引擎 isError | LLM 可见文本 | 任务节点 |
|---|---|---|---|
| 单聊成功 | false | `{"ok":true,"session_id":"subchat-…","mode":"single"}` | done |
| 群聊成功 | false | `{"ok":true,"session_id":"group-…","mode":"group"}` | done |
| 必填缺失 / prompt 超长 | **true** | `{"ok":false,"error":"…"}` | error |
| participants 坏项/重复/有效不足 2 | **true** | `{"ok":false,"error":"participants …"}` | error |
| 未知 agent/scenario/llm（注册表/llms 可用） | **true** | `{"ok":false,"error":"… 不在场景 … / 未配置"}` | error |
| 建会话/首轮/写交接/名册失败 | **true** | `{"ok":false,"error":"…"}` | error；可能留孤儿行 |
| 插件缺 turn / 能力失败 | true | `缺少会话轮次（turn）…` / 桥错误文本 | error |
| 前端开窗失败（窗口上限） | false | 仍为成功 JSON | done（设计如此） |

## 6. 剩余问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P2 | 部分失败无回滚：会话行/名册/展示板已建但后续失败，返回 `ok:false` 后残留孤儿 | `subchat.go:100-192`；`groupchat_sched.go:749-805` | 会话列表出现无首轮/无消息空子聊天；重试会多一份 |
| P2 | 不限制父会话类型：可从 work_session / sub_session / 群聊调用 | `subchat.go:100-192`；`groupchat_sched.go:749-805` | 无窗口父链/开窗假设可能不符；契约未限制 |
| P2 | 非幂等：每次调用新建会话，LLM 重试/超时重发会多开 | 契约 `# 返回值` 已写明 | 重复子聊天；无幂等键 |
| P3 | 存在性校验是“注册表/llms 可用时”生效；两者皆空时跳过 | `subchat.go:67-96` | 未注册能力的极简环境仍可能宽松放行 |
| P3 | 单聊模式静默忽略 goal/playbook/convergence（契约已写明仅群聊用） | `subchat.go:100-192` | 模型误传时参数无声丢弃 |

已修问题（供回归对照）：participants 静默降级单聊（P1，2026-11-08）、描述截断（P1，框架已返回完整摘要）、插件丢弃 `is_error`（P2，2026-11-10）、成功无 mode（P2，2026-11-10）、固定参数无存在性校验（P1，2026-11-10，best-effort）、契约无返回值/上限（P2，2026-11-10）。

## 7. 测试/验收

- `TestSubChatOpenTool`：单聊会话/固定参数/交接消息只落一次/opened/异步首轮；新增 `mode=single`。
- `TestSubChatOpenToolArgsMissing`：缺参明确失败。
- `TestSubChatOpenParticipantsInvalid`：坏项/重复/非数组明确失败，不降级。
- `TestSubChatOpenRejectsUnknownRefs`（2026-11-10）：usr llms 有配置时未知 llm 失败；`s.agents` 非空时未知 scenario 失败。
- 群聊：`openGroup` 断言 `mode=group`；名册顺序/主 agent 补入/调度继续。
- 回归：宿主 `go test ./src/lib/llm/server`、插件 `go test ./...` 全量通过。