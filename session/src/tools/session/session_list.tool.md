# 基本信息
```yaml
name: session_list
title: 列出会话
args: session_list --input={RAW-INPUT-FILE}
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
列出会话（默认只列**顶层对话**，按最近活动降序）。用于："我之前开过哪些会话""那三个任务
分别叫什么"之类。每条带 `session_id` 与 `self` / `top` 锚点：`top=true` 的那条就是用户当前
正在看的对话。

默认范围由用户设置（`plugins.session.cross-user`）决定：`private` / `llm` 只列**当前用户**，
`public` 列**全项目**（自己优先）。`targets` 可显式点名其他用户。
需要看会话内容用 `session_history`，需要按内容找会话说 `session_search`。

# 参数
```json
{
  "type": "object",
  "properties": {
    "targets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选：额外列哪些用户（[\"*\"] = 全项目；[\"alice\"] = 指定用户）。省略 = 按服务端模式（private/llm 仅自己；public 全项目，自己优先）"
    },
    "include_sub": {
      "type": "boolean",
      "description": "true = 连子会话（子聊天 / 委派子会话）一起列出；缺省 false"
    },
    "kind": {
      "type": "string",
      "description": "可选：按会话类型过滤（如 sub_chat / group_chat / crontab_task；空 = 不过滤）"
    },
    "since": {
      "type": "string",
      "description": "可选：创建时间下界（RFC3339）"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 200,
      "description": "最多返回条数（缺省 50，上限 200）"
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "description": "可选：分页偏移"
    }
  }
}
```

# 返回值
```json
{
  "sessions": [
    {
      "user": "alice",
      "session_id": "s-…",
      "dialog_id": "…",
      "title": "登录模块重构",
      "kind": "",
      "origin": "",
      "parent_id": "",
      "created_at": "2026-11-01T10:00:00.000000000Z",
      "updated_at": "2026-11-01T10:20:00.000000000Z",
      "turn_count": 4,
      "self": false,
      "top": true,
      "cross_user": false
    }
  ],
  "total": 3,
  "scanned_users": 1,
  "truncated": false
}
```

`total` = 过滤后的总数（分页前）；`truncated=true` = 扫描预算截断（结果可能不全）。
`permission_denied` = 跨用户模式不允许。