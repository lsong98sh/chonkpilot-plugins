# 基本信息
```yaml
name: crontab_delete
title: 删除定时任务
args: crontab_delete --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: false
output: json
timeout: 30
```

# 摘要
永久删除一条定时任务（定义从 tasks.json 移除；历史运行记录保留在 runs.jsonl 中供排查）。删除后状态栏徽标即时更新。

# 参数
```json
{
  "type": "object",
  "required": ["task_id"],
  "properties": {
    "task_id": { "type": "string", "description": "任务 id（ct_ 开头）" }
  }
}
```

# 返回值
```json
{ "ok": true, "task_id": "ct_7f3a91c2", "count": 0 }
```

失败：`{ ok:false, error }`（任务不存在）。
