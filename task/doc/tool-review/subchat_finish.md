---
plugin: task
tool: subchat_finish
contract: src/tools/task/subchat_finish.tool.md
handler: src/main.go:695（插件）→ E:/BizWorks/chonkpilot/src/lib/llm/server/plugin_caps.go:457（能力）→ E:/BizWorks/chonkpilot/src/lib/llm/server/subchat.go:197（实现）
reviewed_at: 2026-11-10
status: verified
---

# subchat_finish 审查

> 2026-11-08 旧审查的 P1（无 `kind` 守卫，主会话/群聊也会被置 ready）已在当日修复；2026-11-10 继续补齐工具面收窄、`is_error` 透传与返回字段。

## 1. 一句话结论

可用。`ready` 建议态、不关窗/不回流、用户拍板才 `finished`、继续聊回 `chatting` 的语义成立；当前只允许 `kind=sub_chat`，并且非 `sub_chat` 会话（主会话 / 群聊 / `work_session` / `sub_session`）**工具面直接不出现**，绕过工具面直接调用也会明确失败。成功返回 `{ok,session_id,lifecycle:"ready"}`，失败 `isError=true`。

## 2. 调用链

LLM `subchat_finish` tool_call
→ gateway（`_meta.async=sync`、`timeout=0`；同步）
→ task 插件 `taskPlugin.subchatFinish`（`main.go:695`）：校验 `turn`，转发 `plugin/v1/subchat.finish{instance_id,turn}`
→ 宿主能力（`plugin_caps.go:457`）→ `runSubChatFinish`（`subchat.go:197`）：
  1. `SessionGet` 读取本 turn 会话；
  2. `Found && Kind != sub_chat` → 明确失败（不写 lifecycle）；
  3. `SessionLifecycleSet(ready)` → `emitSubchatState`；
  4. 返回 `{ok:true,session_id,lifecycle:"ready"}`。
→ 插件：`is_error=true` 转 Go error（2026-11-10）→ worker 落 failed + 任务节点 error；`false` 落 completed + done。

**工具面收窄**：`newTurnCtx` / `recoverTurnCtx` 对 `sc.kind != sub_chat` 把 `subchat_finish` 加入 `denyTools`（下发面 + 调用处双拦）。`sub_chat` 放行。

## 3. 契约面

- YAML：`async: never`（解析为 sync）、`timeout: 0`、`runtime: plugin`。
- 摘要：`ready` 是建议态、不等于结束、用户拍板才 `finished`、不关窗/不回流/不结束、继续聊回 `chatting`、仅 `sub_chat` 有意义；已补“非子聊天不会下发/直接调用会失败”。
- schema：空对象 + `additionalProperties:false`（无参数）。
- 新增 `# 返回值`：成功 `{ok,session_id,lifecycle}`、失败 JSON、广播 `subchat-state`、幂等、下一轮回 `chatting`。

## 4. 实现面

- 类型守卫：`SessionGet` 后 `resp.Found && Kind != sub_chat` → `{"ok":false,"error":"subchat_finish 仅可在子聊天（sub_chat）会话内调用；当前会话类型: <kind>"}`，`isErr=true`；不写 lifecycle、不广播。
- 副作用：`SessionLifecycleSet(lifecycle=ready)`（单事务 RMW，保留其余字段）+ `emitSubchatState`（不带 `instance_id`，前端按 `session_id`/`parent_session_id` 过滤）。
- 幂等：重复调用再次置 ready + 再次广播；无副作用累积。
- 回写：该会话下一轮开始 `reopenSubChatOnNewTurn` 把 lifecycle 从 ready 回写 `chatting`。
- 清理：无 goroutine / 文件句柄。
- 恢复轮：`recoverTurnCtx` 已重建 denyTools（非 sub_chat 排除），重启后的续轮同样不会下发/放行。

## 5. 返回面

| 形态 | 引擎 isError | LLM 可见文本 | 任务节点 |
|---|---|---|---|
| 成功 | false | `{"ok":true,"session_id":"subchat-…","lifecycle":"ready"}` | done |
| 非 sub_chat（主/群聊/子会话/work_session） | **true** | `{"ok":false,"error":"subchat_finish 仅可在子聊天（sub_chat）会话内调用；当前会话类型: …"}` | error（工具面通常已隐藏） |
| 会话不可用 / 缺数据门面 | **true** | `{"ok":false,"error":"子聊天会话不可用"}` | error |
| 写 lifecycle 失败 | **true** | `{"ok":false,"error":"置 ready 失败: …"}` | error |
| 插件缺 turn / 能力失败 | true | `缺少会话轮次（turn）…` / 桥错误文本 | error |

## 6. 剩余问题

无 P0/P1/P2。仅记录：

| 级别 | 问题 | 说明 |
|---|---|---|
| P3 | 重复调用会重复广播 `subchat-state` | 状态幂等；前端按当前 lifecycle 渲染，重复事件无副作用 |
| P3 | 工具无参数，但插件仍忽略 `args` | schema `additionalProperties:false` 约束客户端；网关展示名在调用前剥离 |

已修问题（供回归对照）：无 kind 守卫（P1，2026-11-08）、工具面对主/群聊未收窄（P2，2026-11-10）、插件丢弃 `is_error`（P2，2026-11-10）、返回值缺 session_id/lifecycle（P2，2026-11-10）、契约缺返回值/副作用说明（P2，2026-11-10）、恢复轮 denyTools 未重建（P2，2026-11-10）。

## 7. 测试/验收

- `TestSubChatFinishTool`：`sub_chat` 正路径 → `lifecycle=ready`、`subchat-state`、`{ok,session_id,lifecycle}`。
- `TestSubChatFinishRejectsNonSubChat`：主会话 / 群聊直接调用明确失败且 lifecycle 不变。
- `TestNonSubChatToolFaceExcludesSubchatFinish`（2026-11-10 扩展）：work_session / 主会话 / group_chat 下发面与调用处均排除；sub_chat 放行。
- 回归：宿主 `go test ./src/lib/llm/server`、插件 `go test ./...` 全量通过。