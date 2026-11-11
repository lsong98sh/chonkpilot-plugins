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
终止一个任务（按 `task_id`）。`cascade=true`（缺省）连带子任务/子代理；`false` **只停目标任务**。
终止不可逆；停止后用 `task_status(task_id=…)` 查询终态。

若目标任务或其（级联）子树包含 **`ask_user` 提问**：提问会随任务终止一并撤销——Ask 中心移除、该提问的历史工具结果标记 `cancelled`，用户之后再作答不会再送回模型。`task_stop` 由 LLM 发起时同样适用。

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
      "description": "true（缺省）= 连带子任务/子代理；false = 只停目标任务"
    }
  }
}
```

# 返回值
```json
{ "ok": true, "cancelled": true, "task_id": "tk-…", "state": "cancelled" }
```
失败（目标不存在 / 已结束 / 执行池不可用）返回明确错误文本，不返回 `ok:true`。
