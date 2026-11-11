# 基本信息
```yaml
name: subchat_finish
title: subchat_finish
args: subchat_finish --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
output: stdout
runtime: plugin
timeout: 0
```

# 摘要
子 agent 自报：本次子聊天**目标已达成**，建议结束。把该子聊天会话的生命周期置为 `ready`（**建议态，不等于结束**）——它只是「你认为可以收尾了」，**是否结束由用户拍板**（用户点「结束」才进 `finished`）。不关窗、不回流、不结束会话；用户若继续聊，新一轮开始即回到 `chatting`。仅在子聊天（`sub_chat`）会话内有意义；主会话 / 群聊 / 子会话不会下发本工具，直接调用也会明确失败。

# 参数
```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}
```

# 返回值
成功（`isError=false`）：

```json
{ "ok": true, "session_id": "subchat-…", "lifecycle": "ready" }
```

- 副作用：写该会话 `lifecycle=ready`，并广播 `subchat-state`（前端据此点亮「可以结束了」建议）。
- 不关窗、不结束、不回流；用户点「结束」才进 `finished`，继续聊则下一轮回到 `chatting`。
- 重复调用幂等（再次置 ready + 再次广播）。

失败（`isError=true`，文本为 JSON）：

```json
{ "ok": false, "error": "subchat_finish 仅可在子聊天（sub_chat）会话内调用；当前会话类型: group_chat" }
```

常见错误：非 `sub_chat` 会话、会话不可用、写 lifecycle 失败。
