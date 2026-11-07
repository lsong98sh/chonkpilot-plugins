# 基本信息
```yaml
name: task_status
title: 查询任务
args: task_status --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 120
```

# 摘要
查询任务状态与结果（只读）。给 `task_id` = 查单个任务：终态返回结果，仍在运行返回
`state=running` + **已产出的部分输出**；`waitSec` 可等待至多该秒数。不给 `task_id` =
返回当前会话的任务树 + 任务列表（`limit` 截断）。

`task_id` 支持：任务节点 id（`tk-…`）、gateway 执行 id、委派子代理的 turn id。

# 参数
```json
{
  "type": "object",
  "properties": {
    "task_id": {
      "type": "string",
      "description": "可选：单任务查询目标（任务节点 / gateway 执行 id / 子轮次 turn id）"
    },
    "waitSec": {
      "type": "integer",
      "description": "可选：等待秒数（仅单任务查询；0/缺省 = 立即返回当前状态）"
    },
    "limit": {
      "type": "integer",
      "description": "可选：任务树模式最多返回的任务条数（缺省用配置）"
    }
  }
}
```

# 返回值
单任务：
```json
{
  "ok": true,
  "task_id": "tk-…",
  "state": "running | done | error | cancelled",
  "result": "终态结果（终态时）",
  "output": "已产出的部分输出（运行中）"
}
```

任务树：
```json
{
  "ok": true,
  "limit": 50,
  "tree": {},
  "tasks": []
}
```