# 基本信息
```yaml
name: crontab_update
title: 修改定时任务
args: crontab_update --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: false
output: json
timeout: 30
```

# 摘要
修改一条已存在的定时任务的 `cron` 和/或 `action`（省略 = 保持不变）。`action` 语义与 `crontab_add` 一致（`{type:"run", param:{command, notify?}}` 或 `{type:"notify", param:HTML}`）。修改后立即按新 cron 重算下次执行时间；任务保持原有的启用 / 停用状态。`sandbox` 可单独修改任务的写隔离开关（省略 = 不变；`"inherit"` = 恢复继承插件配置）。

# 参数
```json
{
  "type": "object",
  "required": ["task_id"],
  "properties": {
    "task_id": { "type": "string", "description": "任务 id（ct_ 开头）" },
    "cron": { "type": "string", "description": "新的 cron 表达式（省略 = 不变）" },
    "action": {
      "type": "object",
      "required": ["type", "param"],
      "description": "新的动作（省略 = 不变）",
      "properties": {
        "type": { "type": "string", "enum": ["run", "notify"] },
        "param": { "type": ["object", "string"] }
      }
    },
    "sandbox": {
      "type": ["boolean", "string"],
      "enum": [true, false, "inherit"],
      "description": "run 任务的写隔离开关：true = 以 Low IL + Job 执行（只允许写工作目录 / 项目白名单）；false = 不隔离；\"inherit\" = 恢复继承插件配置；省略 = 不变"
    }
  }
}
```

# 返回值
```json
{ "ok": true, "task": { "id": "ct_7f3a91c2", "description": "通知：该喝水了", "cron": "0 */2 * * *", "next_run_at": 1761888000, "enabled": true, "type": "notify", "sandbox": null } }
```

失败：`{ ok:false, error }`（任务不存在 / cron 非法 / action 非法 / sandbox 非法）。
