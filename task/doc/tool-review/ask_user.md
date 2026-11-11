---
plugin: task
tool: ask_user
contract: src/tools/task/ask_user.tool.md
handler: E:/BizWorks/chonkpilot-plugins/task/src/main.go:514（插件）→ E:/BizWorks/chonkpilot/src/lib/llm/server/plugin_caps.go:374（能力）→ E:/BizWorks/chonkpilot/src/lib/llm/server/handler_ask.go:388（ask 通道）
reviewed_at: 2026-11-10
status: verified（S4 重构复核 + 2026-11-10 去重/落库/批次修复）
---

# ask_user 审查

> 路径约定：插件仓 = `E:/BizWorks/chonkpilot-plugins`，主仓 = `E:/BizWorks/chonkpilot`；文中 `src/...` 指对应仓内的相对路径。
> 2026-11-08 的旧审查基于迁移前实现，`async: never` / `inProcessTool` / 行号均已失效；本文以 2026-11-09 的 S4 重构实现为准。
> 不重复《工具审查.md》的调用链、展示名、`_meta`、路径规则等共通基线。

## 1. 一句话结论

S4 重构后的非阻塞、批次、历史回写、恢复路径整体成立；复核发现并修复了 **1 个 P1：契约 `async: async` 让 gateway 走后台任务路径，绕过 turn 侧 ask 的“任务节点保持 running、答复后收尾”分支**。2026-11-10 又按用户要求修掉三项遗留：历史答案双份、投递失败仅记日志、批次等待全部作答；另补齐 `task_stop` 撤销提问与选项空 value 归一。当前无未决 P0/P1/P2。

## 2. 调用链（重构后）

LLM `ask_user` tool_call
→ llm-server `dispatchToolCall`：为 ask 预生成 `ask_id` 并 `persistToolRunning`（`turn.go:1432-1437`），建 `Kind=ask_user` 任务节点（`turn.go:1458-1460`）
→ `dispatchToolWorker`（`turn.go:1510`）→ gateway `mcp-tools-call`
→ gateway 按契约 `_meta` 裁决：`async: sync` + `timeout: 0` → 同步等待（`mcpgateway.go:1064-1095`）
→ task 插件 MCP `tools/call` → `taskPlugin.askUser`（插件 `src/main.go:514`）：
  - 先做参数校验（`validateAskArgs`，`main.go:552`）：`question` 去空白非空/≤2000，`context`≤4000，`options`≤20，选项/推荐值≤500；
  - 再调宿主能力 `plugin/v1/ask.user{instance_id,turn,tool_call_id,args}`
→ 宿主能力（`plugin_caps.go:374-419`）：按 `(instance,turn,tool_call_id)` 找回 `task_id`/`ask_id`；**广播前**落占位 tool-result（`:408-415`）→ `askWithID` 登记批次/定时器并广播 `session-ask`（`handler_ask.go:395-522`）
→ 能力返回 `{text: askAsyncHandoffResult, ask_id}` → 插件 `pluginsdk.Text` → MCP `TextContent`（`isError=false`）
→ worker（`turn.go:1546-1581`）：
  - `isError=true` → 落 `failed` + 任务节点 `error` + 引擎 `isError=true`（2026-11-09 修复）；
  - 正常占位 → **跳过重复落库/`tasks.done`**，任务节点保持 `running`；
  - 占位文本经 engine Inbox 回喂，本轮继续。

用户答复 / 超时 / 「完成」：
- `session-ask-reply` → `onAskUserReply`（`handler_ask.go:203`）：`tasks.done`、tool-result 写“已回复”标记、`recordAskAnswer` **立即投递**当前已答集合；
- `deliverAskAnswers`（`:614`）：主会话写持久化 inbox `next_turn`，群聊/子会话有运行轮则 `sendTextToTurn`，否则起新轮；投递失败 → `persistUndeliveredAskAnswer` 落到原轮次；
- `ask-resolve`（`:298`）：仅改写历史 + `tasks.done`，不投递 LLM 消息；
- 超时 → `expireAsk`（`:548`）：自动答复同样立即投递。

## 3. 契约面

- YAML：`async: sync`、`timeout: 0`、`policy_lock: true`（与 `task_status` 同族）。`policy_lock` 锁死 md 值，界面/CLI/调用级不可改为后台异步。
- **P1（已修）**：旧契约 `async: async`。gateway 会立即返回 `pending + task_id`，走 `handleAsyncDone`/`onGatewayTaskDone` 异步回执路径：
  - `onGatewayTaskDone` 在占位返回时就把 ask 任务节点置 `done`（`handler_task.go:193-197`），答复到达后的 `tasks.done(answer)` 因幂等不再生效；
  - `handleAsyncDone` 用 `persistToolResult`（不带 `ask_id`）回填占位（`turn.go:1228-1235`），极端竞态下若答复先到还会覆盖答案；
  - 因此必须同步返回占位，让 turn 侧 ask 分支保持任务节点 running。已改为 `async: sync`，并加契约测试 `TestAskUserContractSyncPolicy`。
- **P1（已修）**：摘要第一段旧文案“并等待回答”与运行时非阻塞冲突（框架 `describe` 返回完整 `# 摘要`，但仍会误导模型）。已改为“立即返回占位结果（非阻塞）”，并补充：
  - 主会话答复走 `next_turn`（本轮结束后才送达），群聊/子会话可喂入运行轮；
  - 同会话多问**每条答复立即投递**，未答问题不再阻塞已答内容（2026-11-10）；
  - 历史/重载时 tool 行只写“已回复/已超时”标记，答案正文只在后续用户消息里出现一次。
- 选项：`options` 省略/空数组 = 自由文本问答（用户可直接输入，等价「其他」）；有选项时 `custom=true` 额外提供「其他」输入框；`multi=true` 支持多选。选项对象 `value/label` 至少一个非空；显式空 `value` + 非空 `label` 会归一为 label。`{}` / 全空白 → 参数错误。
- **P2（已修）**：无 `# 返回值`。已补：占位原文、单问/多问用户消息格式、超时文案、`参考后续的聊天记录`、参数错误行为。
- **P2（已修）**：schema 无上限、无 `additionalProperties:false`。已补 `question.maxLength=2000`、`context.maxLength=4000`、`options.maxItems=20`、选项/推荐值 `maxLength=500`、`options.items.anyOf(string, {value,label})`、顶层与对象 `additionalProperties:false`、`timeout.minimum=-1`。
- 运行时兜底与 schema 同步（`validateAskArgs`）：空/超长 question、超长 context、选项数量/形状/长度、recommended 数量/形状均返回 `isError=true` 的工具错误，不登记 ask。旧审查“question 零校验”已失效。

## 4. 实现面

- 插件侧（`main.go:514`）：仅校验 + 转发；能力返回 `is_error` 现在也会转成 Go error（修复旧 P3），不再静默当成功。`question` 去首尾空白后下发。
- 宿主能力（`plugin_caps.go:374`）：`task_id` 先按本轮运行登记回填，缺失再 `data-ask-pending` 回查；`ask_id` 先按 `tc.askIDOf` 取，缺失再回查。占位结果**先于广播**落库（`persistToolResultAsk`），避免答复先到被占位覆盖。
- worker（`turn.go:1546-1581`）：新增 `res["isError"]` 识别（2026-11-09）。非 ask 工具同样受益：MCP 工具级错误不再被当作 completed。
- 批次（`handler_ask.go:52-69,256-284`）：键 = `(instance,session)`；`open` 未答、`answered` 为本次待投递已答项。`recordAskAnswer` 取走当前已答集合并**立即**返回投递文本，清空 `answered`；`open` 为空才删除批次。并发答复可能合并在同一条，未答问题不阻塞。
- 超时（`:586-605`）：`time.AfterFunc` → 单 worker 串行 `expireAsk`；自动答复取 `recommended`：单选取首个、多选“、”拼接，否则固定文案；同样立即投递。
- 历史回写（`:369-395`）：按 `(turn_id, tool_call_id)` 复用同一行，`tool_call_status=completed`，保留 `ask_id`；文本只写 `用户已回复（答案见后续用户消息）` / `已超时，自动答复见后续用户消息`，**不复制答案正文**（2026-11-10 去重）。
- 投递失败兜底（`:614-710`）：主会话检查 `onInboxAdd` 的 `v.Result.ok`（业务失败也返回 nil error）；非主会话检查 `sendTextToTurn/startAskAnswerTurn` 返回的 error。失败时 `persistUndeliveredAskAnswer` 把答复作为 user 消息写入原 `turn_id`（重载可见，不重试、不新起轮）；写库也失败才仅日志。
- 恢复（`askrecover.go`）：启动扫描只留痕，随后 `cleanupStaleToolPairs` 置 interrupted；答复到达时等待登记缺失 → `replyExpiredAsk` 按落库行状态决定“转新轮”或明确失败。
- 清理：`llm-cancel` 按 turn 摘除 Ask 中心/批次（已收答复一并丢弃）；实例退出按 instance 摘除；超时/答复/完成分别 reason=`timeout/answered/resolved`。
- `task_stop`（LLM 或 UI）：命中 `ask_user` 任务节点（或级联子树内含 ask 节点）时同步撤销提问（`handler_task.go:64-87`）：
  - 按 task_id 摘除 `s.asks`、丢弃批次已收答复、停 timer（`clearAsksByTaskLocked`）；
  - 历史 tool-result 改写为 `cancelled` + `提问已终止（任务已停止）`（`updateAskToolResultStatus`）；
  - Ask 中心 remove（reason=`task_stopped`）；
  - **不**向 LLM 投递消息；之后再作答会按失效回执明确失败（落库行已 cancelled）。

## 5. 返回面

| 形态 | 引擎 isError | LLM 可见文本 | 说明 |
|---|---|---|---|
| 受理成功 | false | `askAsyncHandoffResult`（“已向用户提问，正在等待回复…”） | 任务节点 running；tool 行 completed（占位） |
| 直接答复（后续用户消息） | — | `【用户回答】\n问：…\n答：…`；并发答复按序号合并 | **每条答复立即投递**；主会话 `next_turn`，非主会话可喂运行轮 |
| 超时自动答复 | — | 推荐值或 `用户未在时限内回复，请按推荐做法继续。` | 与用户答复同路径、立即投递 |
| 「完成」 | — | 无新消息；历史行改为 `参考后续的聊天记录` | 不投递；已投递答复不重复 |
| 投递失败 | — | 无新用户消息；答复作为 user 消息落到原轮次 | `persistUndeliveredAskAnswer` 兜底，重载可见 |
| 参数错误 | **true** | `question 不能为空` / `question 过长…` 等 | 2026-11-09 修复后落 failed + 任务节点 error；不登记 ask |
| 插件/能力 Go error | true | `错误: …` | worker 既有 `cerr != nil` 分支落 failed + error |
| 本轮取消 | — | 无工具结果 | Ask 中心 reason=`turn_closed`，批次丢弃 |
| 实例退出后答复 | — | 新轮 / 明确失败 | `replyExpiredAsk` |

## 6. 剩余问题

无 P0/P1/P2。仅保留一条设计边界（P3，按用户口径接受）：

| 级别 | 问题 | 证据 | 说明 |
|---|---|---|---|
| P3 | 投递失败兜底只“落库”，不重试、不触发 LLM | `handler_ask.go:657-680` | 用户要求“只要落库”；答案写到原轮次，重载/历史可见，下一轮可由用户/系统再推进 |

已修问题（供回归对照）：`async: async`（P1）、摘要首段“并等待回答”（P1）、空 question 无校验（P1）、plugin/能力错误导致 running 僵尸（P1）、无 `# 返回值`（P2）、schema 无上限（P2）、能力 `is_error` 被忽略（P3）、`task_stop` 只停节点不撤销提问（P1）、历史答案双份（P2，2026-11-10）、投递失败仅日志（P2，2026-11-10）、批次等待全部作答（P2，2026-11-10）、选项空 value 产生空答案（P3，2026-11-10）。

## 7. 测试/验收

已加：
- 插件 `TestAskUserContractSyncPolicy`：`async=sync`、`policy_lock=true`、`timeout=0` 显式。
- 插件 `TestAskUserArgLimits`：question/context/options/recommended 上限、形状、`{}`/全空白选项。
- 插件 `TestValidateAskArgsNormalizesEmptyValue`：空 value + 非空 label → 归一为 label。
- 宿主 `TestEngineTurnAskArgError`：ask 参数错误 → tool 行 `failed`、任务节点 `error`、不登记 ask、本轮 complete。
- 宿主 `TestEngineTurnAskTaskStop`：`task-stop` 命中 ask 节点 → tool 行 `cancelled`、任务节点 `cancelled`、ask/批次摘除、无答复消息。
- 宿主 `TestAskBatchDeliveryDoesNotWaitAll`：Q1 答复立即投递、不等待 Q2。
- 宿主 `TestAskResolveAfterAnsweredDoesNotRedeliver`：Q1 已投递后 Q2「完成」不重复。
- 宿主 `TestPersistUndeliveredAskAnswer`：投递失败兜底把答复写入原轮次。
- 宿主 `TestEngineTurnAsk` / `TestAskTimeoutAutoReplyFullChain` / `TestAskDialog...`：tool 行只写标记、答案仅在后续用户消息。
- 回归：插件 `go test ./...`、宿主 `go test ./src/lib/llm/server`（全量）通过。