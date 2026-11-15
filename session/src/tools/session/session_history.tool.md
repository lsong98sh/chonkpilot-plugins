# 基本信息
```yaml
name: session_history
title: 读取会话历史
args: session_history --input={RAW-INPUT-FILE}
async: sync
category: session
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 60
touch_files: false
```

# 摘要
分页读取某个会话的**原文**（轮次 + 消息）。`session_id` 省略 = **本会话**；`"@top"` =
用户正在看的顶层对话。默认从最新一轮往回取 50 条消息 / 200KB，`has_more=true` 时用
`before_turn_id` 继续往前翻。

这是拿"原话"的入口：先 `session_search` 找到 `turn_id`，再用 `before_turn_id=<该轮之后的一轮>`
定位；或直接指定 `session_id` 从最新往回读。**不要一次拉全会话**——按需分页。

# 参数
```json
{
  "type": "object",
  "properties": {
    "session_id": {
      "type": "string",
      "description": "会话 id；省略 = 本会话；\"@top\" = 当前顶层对话"
    },
    "targets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选：目标用户（至多 1 个；省略 = 当前用户）。受跨用户模式门控"
    },
    "before_turn_id": {
      "type": "string",
      "description": "可选：只取该轮**之前**的轮次（不含该轮）；用于向前翻页"
    },
    "target_messages": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "本次最多返回的消息条数（缺省 50）"
    },
    "target_bytes": {
      "type": "integer",
      "minimum": 1024,
      "maximum": 1048576,
      "description": "本次最多返回的正文字节数（缺省 200KB）"
    }
  }
}
```

# 返回值
```json
{
  "user": "alice",
  "session": { "session_id": "s-…", "dialog_id": "…", "title": "…", "turn_count": 4 },
  "turns": [
    {
      "turn_id": "t-…",
      "status": "done",
      "summary": "",
      "created_at": "2026-11-01T10:00:00.000000000Z",
      "messages": [
        {
          "message_id": "M…",
          "turn_id": "t-…",
          "role": "user",
          "content": "交付时间定在周五",
          "created_at": "2026-11-01T10:00:00.000000000Z",
          "truncated": false
        }
      ]
    }
  ],
  "has_more": true,
  "truncated": false
}
```

`has_more=true` = 还有更早的轮次未返回（用最早一条的 `turn_id` 作为下一轮 `before_turn_id`）。
`truncated=true` = 单条消息正文超长被截断（`truncated=true` 标记在消息上）。