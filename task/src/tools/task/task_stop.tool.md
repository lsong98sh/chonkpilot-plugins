# 基本信息
```yaml
name: task_stop
title: 终止任务
args: task_stop --input={RAW-INPUT-FILE}
async: never
category: task
hot: true
runtime: plugin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
终止一个任务（按 `task_id`）；`cascade=true` 连带子任务。数据面 `task.stop`。

# 参数
```json
{
  "type": "object",
  "required": ["task_id"],
  "properties": {
    "task_id": {
      "type": "string",
      "description": "要终止的任务 id"
    },
    "cascade": {
      "type": "boolean",
      "description": "是否连带子任务"
    }
  }
}
```

# 返回值
```json
{ "ok": true, "result": {} }
```
