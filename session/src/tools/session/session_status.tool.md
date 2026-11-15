# 基本信息
```yaml
name: session_status
title: 查询会话状态
args: session_status --input={RAW-INPUT-FILE}
async: sync
category: session
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 600
touch_files: false
```

# 摘要
查询某个会话（或其中某轮）的**持久化状态**与**最终回复文本**。

用于 `session_create` / `session_send` 以 `wait_sec=0` 返回 `running` 之后：父轮很快结束，此时
**没有执行中的工具**可承载结果、也没有 inbox 通知；`task_status` 依赖发起轮次仍存活，父轮结束后
不可用。本工具直接读库，因此**在任何轮次、任何会话里都能查**，并可 `wait_sec` 等待到终态。

典型用法：
1. `session_create(prompt=…, wait_sec=0)` → 得到 `session_id` + `state=running`；
2. 稍后 `session_status(session_id=…, wait_sec=60)` → `state=done` + `text` = 最终回复。

# 参数
```json
{
  "type": "object",
  "properties": {
    "session_id": {
      "type": "string",
      "description": "目标会话 id；省略 = 本会话；\"@top\" = 当前顶层对话"
    },
    "turn_id": {
      "type": "string",
      "description": "可选：指定轮次；省略 = 最新轮"
    },
    "wait_sec": {
      "type": "integer",
      "minimum": 0,
      "maximum": 600,
      "description": "可选：轮次仍 running 时最多等待多少秒（缺省 0 = 立即返回）。不要在单次调用里长时间阻塞，建议 30–60 秒分段轮询"
    },
    "targets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选：跨用户查询（语义同 session_search；private 模式点名他人 → permission_denied）"
    }
  }
}
```

# 返回值
```json
{
  "user": "alice",
  "session": { "session_id": "split-…", "dialog_id": "…", "title": "交付讨论", "turn_count": 2 },
  "state": "done",
  "turn_status": "done",
  "turn_id": "t-…",
  "text": "最终回复正文…",
  "updated_at": "2026-11-15T10:00:00.000000000Z",
  "waited_ms": 420
}
```

字段口径：

- `state`：`running` / `done` / `incomplete` / `error` / `interrupted` / `cancelled` / `not_started`
  （`turn_status` 是库内原始值，便于诊断）。
- `text`：该轮最后一条 assistant 正文（终态 = 最终回复）；无正文时为空。
- `output`：仅 `state=running` 时出现，= 已产出的部分文本（可能为空）。
- `waited_ms`：本次调用实际等待时长（含首次读取）。

`not_found` = 会话 / 指定轮次不存在；`permission_denied` = 跨用户模式不允许。查询不会修改会话。