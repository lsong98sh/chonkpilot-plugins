# 基本信息
```yaml
name: session_send
title: 向会话追发消息
args: session_send --input={RAW-INPUT-FILE}
async: auto
category: session
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 600
touch_files: true
```

# 摘要
向一个**既有会话**追发一条消息并启动一轮。目标会话通常是 `session_create` 或 `session_list`
返回的顶层对话。默认异步（返回 `state=running` + `task_id`）；`wait_sec>0` 同步等待。

**不要用它向用户正在使用的会话抢写**：该会话已有运行轮次时返回 `session_busy`。
也不要用它做委派（用 `task_llm`）。

# 参数
```json
{
  "type": "object",
  "required": ["prompt"],
  "properties": {
    "session_id": {
      "type": "string",
      "description": "目标会话 id（来自 session_create / session_list）；省略或 \"@top\" = 当前顶层对话"
    },
    "prompt": {
      "type": "string",
      "description": "要发送给该会话的消息（必填）"
    },
    "wait_sec": {
      "type": "integer",
      "minimum": 0,
      "maximum": 600,
      "description": "0/缺省 = 立即返回 running；>0 = 最多等该秒数后返回终态"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "state": "running",
  "session_id": "s-…",
  "dialog_id": "…",
  "turn_id": "…",
  "task_id": "tk-…",
  "message": "会话已启动；结果请用 task_status(task_id=…) 获取"
}
```

`state=done` 时另带 `text`。`session_busy` = 该会话已有轮次在跑（等它结束或换会话）；
`not_found` = 会话不存在（或不属于当前用户）。

# 注意
- 只支持**当前用户**的会话；跨用户追写不在能力面内。
- `@top` 指向用户当前正在看的对话；它通常正被用户轮次占用 → 大概率 `session_busy`。