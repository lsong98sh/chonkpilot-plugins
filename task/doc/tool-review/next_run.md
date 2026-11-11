---
plugin: task
tool: next_run
contract: src/tools/task/next_run.tool.md
handler: src/main.go:654（插件）→ E:/BizWorks/chonkpilot/src/lib/llm/server/plugin_caps.go:422（能力）→ E:/BizWorks/chonkpilot/src/lib/llm/server/nextrun.go:50（登记）
reviewed_at: 2026-11-10
status: verified
---

# next_run 审查

> 2026-11-08 旧审查的 P1（恢复轮 `nextRunMax=0` → 恒“未启用”、`nextRunItemID` 丢失）与 P2（插件丢弃 `is_error`、契约缺返回值/上限）已修，见 §4/§6。

## 1. 一句话结论

可用。登记、last-wins、上限、主会话限制、异常终态丢弃均成立；2026-11-10 补齐**恢复轮运行态重建**（`nextRunMax` / agent 白名单 / deny 集 / 已登记 inbox 项回查）、**错误语义**（插件透传 `is_error` → worker 落 failed + 任务节点 error）与 **append 幂等命中后的 content 强制覆盖**；契约同步补上限/返回值/丢弃规则，成功返回改为可读的第几次/上限文案。

## 2. 调用链

LLM `next_run` tool_call
→ gateway（契约 `async: never` → `_meta.async=sync`、`timeout=0`；同步等待、无超时点）
→ task 插件 `taskPlugin.nextRun`（插件 `main.go:654`）：校验 `turn`，转发 `plugin/v1/next.run{instance_id,turn,args}`
→ 宿主能力（`plugin_caps.go:422`）→ `runNextRun`（`nextrun.go:44`）：
  参数/长度/主会话/开关校验 → `SessionNextRunCheckAndAdd`（原子判上限+自增）→ inbox `next_turn` 追加或同轮覆盖（`source=llm`、幂等键 `next-run:<turn>`；append 后强制覆盖 content）→ 返回 `已登记下一轮（第 N 次，上限 M）` / 到限文案 / 错误文本
→ 插件：`is_error=true` 转 Go error（2026-11-10）→ worker 落 failed + 任务节点 error；`false` 走正常文本
→ 本轮正常结束 → `consumeNextTurn` 起新轮（新轮 user 消息 `kind=next_run`）。

## 3. 契约面

- YAML：`async: never`（解析为 sync）、`timeout: 0`、`runtime: plugin`。`policy_lock` 未声明；本工具毫秒级同步返回，gateway 侧无超时点。
- 摘要已说明：`prompt` 必须自成一体、成功后本轮收尾、同轮 last-wins、到限文案、仅主会话；2026-11-10 增补**默认上限 30（`nextRunMax`，0=关闭）**与**本轮 error/interrupted/cancelled 会丢弃已登记项**。
- schema：`prompt` required、`maxLength: 16384`、`additionalProperties:false`。
- 新增 `# 返回值`：成功 `已登记下一轮（第 N 次，上限 M）`（N = 本次受理后的计数、M = `nextRunMax`）；到限 `已自动接续 N 次，需用户判断是否继续`；未启用文案；参数/会话/数据失败 `isError=true`；异常终态丢弃无额外 tool 结果。

## 4. 实现面

- 校验（`nextrun.go:44-65`）：`tc` 非空 → prompt 去空白非空 → ≤16384 码点 → 仅主会话（`Parents` 空且会话 `kind==""`）→ 数据门面可用 → `nextRunMax>0`。
- 计数/上限：`SessionNextRunCheckAndAdd` 原子；拒绝不 +1、不动已登记项；inbox 写失败 `nextRunCompensate` 回滚。成功文案用返回的 `Count`（本次受理后的次数）与 `max` 拼装。
- last-wins：`tc.nextRunItemID` 命中 → `inboxUpdate` 覆盖 content；未命中 → append 一行。**append 后无论是否命中数据层幂等键，都先 `inboxRemember` 回填内存索引（带 SessionID/TurnID/Type/Content/Source/Status），再 `inboxUpdate(instanceID, id, "", "", prompt)` 强制覆盖 content**（2026-11-10 修）：恢复回查失败时 `nextRunItemID` 为空，append 会返回旧 id 且不更新正文，不强制覆盖会静默丢弃新 prompt；update 失败按既有口径补偿一次并返回错误文本。
- **恢复轮（2026-11-10 修）**：`recoverTurnCtx` 现与 `newTurnCtx` 对齐重建：
  - `nextRunMax = llmNextRunMaxFrom(contCfg)`；
  - agent `allowedTools`（含场景级别过滤）/ `work_session` `denyTools`；
  - `nextRunMax<=0` / 非主会话 → deny `next_run`；
  - `nextRunItemForTurn`（`nextrun.go:129`）按 `source=llm + turn_id + type=next_turn + pending` 回查本轮已登记项，设置 `tc.nextRunItemID` 并回填内存索引（否则后续 `inboxUpdate` 缺 dialog_id）。回查失败（`inboxList` 错误）仅日志，但 append 后的强制覆盖保证 content 仍为本次 prompt（见上条）。
- 终态收敛：error/interrupted/cancelled → `discardNextRunForTurn` 把本 turn 的 LLM pending 项置 `discarded`；计数保留，等用户下次发送归零。
- 重置：用户发新轮/发送/插话/inbox promote → `resetNextRunCount`。
- 限制：inbox 每会话 pending 上限 50；next_run 本身同轮只占一行。

## 5. 返回面

| 形态 | 引擎 isError | LLM 可见文本 | 任务节点 |
|---|---|---|---|
| 成功登记 / 同轮覆盖 | false | `已登记下一轮（第 N 次，上限 M）` | done；N = 本次受理后的计数 |
| 到上限 | false（业务文案） | `已自动接续 N 次，需用户判断是否继续` | done；已登记项保留 |
| 未启用（max=0） | false | `next_run 未启用（自动接续上限为 0）` | done；正常工具面已 deny |
| 非主会话 | **true** | `next_run 仅支持主会话（…）` | error |
| 空 / 超长 prompt | **true** | `错误: prompt 必填` / `错误: prompt 超长（N 字符，上限 16384）` | error |
| 数据门面 / 计数 / inbox 失败 | **true** | `错误: 登记下一轮失败（…）` | error；计数已补偿 |
| 插件缺 turn / 能力失败 | true | `缺少会话轮次（turn）…` / 桥错误文本 | error |
| 本轮异常终结 | — | 已登记项 discarded，无回执 | discarded |

## 6. 剩余问题

无 P0/P1/P2/P3。

已修问题（供回归对照）：恢复轮 `nextRunMax` 零值（P1）、恢复后 `nextRunItemID` 丢失导致 last-wins 失效（P1，2026-11-10）、插件丢弃 `is_error` 导致失败显示成功（P2，2026-11-10）、契约缺返回值/上限/丢弃规则（P2）、恢复轮 agent 白名单/deny 集未重建（P2，2026-11-10）。

- P3（2026-11-10 修）：恢复回查依赖 `inboxList` 成功；数据面读取失败时 `nextRunItemID` 为空，同 turn 再次登记会命中 append 幂等键（`next-run:<turn>`）返回旧 id 且**不更新 content**，新 prompt 被静默丢弃。修法：append 成功后先 `inboxRemember` 回填内存索引（带 SessionID/TurnID/Type/Content/Source/Status），再 `inboxUpdate(instanceID, id, "", "", prompt)` 强制覆盖；update 失败按既有 `nextRunCompensate` 回滚本次自增并返回错误文本（append / update 失败各只补偿一次）。
- P3（2026-11-10 修）：成功返回裸 `OK`，模型不知道是第几次、还剩多少配额。修法：返回 `已登记下一轮（第 N 次，上限 M）`（N = `SessionNextRunCheckAndAdd` 返回的 `Count`，M = `max`）；到限 / 未启用 / 错误文案保持不变。

## 7. 测试/验收

- `TestNextRunRegistersAndStartsNewTurn`：登记（成功文案 `已登记下一轮（第 1 次，上限 30）`）→ 本轮 complete → 新轮 `kind=next_run`。
- `TestNextRunLimitAndReset`：last-wins、成功文案随次数递增（第 1/2 次）、到限文案、计数不 +1、用户发送重置后再受理（文案回到第 1 次）。
- `TestNextRunRejectsNonMainAndDisabled`：非主会话 / 关闭 / 空 prompt；拒绝不落 inbox。
- `TestNextRunDiscardedOnError`：本轮 error → pending 项 discarded、计数保留。
- `TestNextRunRecoveryRestoresState`（2026-11-10）：恢复轮 `nextRunMax` 默认、`next_run` 未被 deny、回查已登记项、同 turn 再次登记覆盖 content。
- `TestNextRunAppendOverwritesOnIdempotencyHit`（2026-11-10）：模拟恢复回查失败（`nextRunItemID` 为空）后同 turn 再登记，append 命中幂等键返回旧 id 时 content 仍被强制覆盖为本次 prompt、计数为 1。
- 回归：宿主 `go test ./src/lib/llm/server`、插件 `go test ./...` 全量通过。