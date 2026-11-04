# 基本信息
```yaml
name: crontab_disable
title: 停用定时任务
args: crontab_disable --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: true
output: json
timeout: 30
```

# 摘要
停用一条定时任务（保留定义，不再触发；下次执行时间清空）。重复停用是幂等的。

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
{ "ok": true, "task_id": "ct_7f3a91c2", "enabled": false }
```
