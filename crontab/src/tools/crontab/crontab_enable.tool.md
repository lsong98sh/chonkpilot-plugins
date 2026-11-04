# 基本信息
```yaml
name: crontab_enable
title: 启用定时任务
args: crontab_enable --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: true
output: json
timeout: 30
```

# 摘要
启用一条已停用的定时任务，并立即按 cron 计算下次执行时间。重复启用是幂等的。

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
{ "ok": true, "task_id": "ct_7f3a91c2", "enabled": true, "task": { "id": "ct_7f3a91c2", "cron": "*/5 * * * *", "next_run_at": 1761878700, "enabled": true } }
```
