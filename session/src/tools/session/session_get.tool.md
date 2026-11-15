# 基本信息
```yaml
name: session_get
title: 会话详情
args: session_get --input={RAW-INPUT-FILE}
async: sync
category: session
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 30
touch_files: false
```

# 摘要
读单个会话的**元数据**（标题 / 类型 / 父会话 / 轮次数 / 时间），不含正文。`session_id`
省略 = **本会话**；`"@top"` = 用户当前正在看的顶层对话。要正文用 `session_history`。

# 参数
```json
{
  "type": "object",
  "properties": {
    "session_id": {
      "type": "string",
      "description": "会话 id；省略 = 本会话（当前轮次所属会话）；\"@top\" = 当前顶层对话"
    },
    "targets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选：目标用户（至多 1 个；省略 = 当前用户）。受跨用户模式门控"
    }
  }
}
```

# 返回值
```json
{
  "user": "alice",
  "session": {
    "user": "alice",
    "session_id": "s-…",
    "dialog_id": "…",
    "title": "登录模块重构",
    "kind": "",
    "parent_id": "",
    "created_at": "2026-11-01T10:00:00.000000000Z",
    "updated_at": "2026-11-01T10:20:00.000000000Z",
    "turn_count": 4,
    "self": true,
    "top": true,
    "cross_user": false
  }
}
```

`not_found` = 会话不存在（或不在该用户下）。