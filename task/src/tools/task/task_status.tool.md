# 基本信息
```yaml
name: task_status
title: 查询任务树
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
查询当前会话的任务树 / 任务列表（只读）。数据来自数据面 `task.tree` / `task.tasks`。

# 参数
```json
{
  "type": "object",
  "properties": {
    "limit": {
      "type": "integer",
      "description": "最多返回的节点/任务条数（缺省用配置）"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "limit": 50,
  "tree": {},
  "tasks": []
}
```
