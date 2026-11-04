# 基本信息
```yaml
name: crontab_list
title: 列出定时任务
args: crontab_list --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: true
output: json
timeout: 30
```

# 摘要
列出本机全部定时任务（可按启用状态过滤）。返回 `id / 简述 / cron / 下次执行时间 / 是否启用 / 动作类型 / 最近一次结果`，另含 `work_dir`、`sandbox`（任务级开关，`null` = 继承插件配置）与 `disabled_reason`（因 `work_dir` 缺失被自动停用时为 `work_dir_missing`）。任务全局共享，不区分项目。

# 参数
```json
{
  "type": "object",
  "properties": {
    "enabled": { "type": "boolean", "description": "只返回启用（true）或停用（false）的任务；省略 = 全部" }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "count": 1,
  "tasks": [
    { "id": "ct_7f3a91c2", "description": "执行：npm run build", "cron": "*/5 * * * *",
      "next_run_at": 1761878700, "enabled": true, "type": "run", "run_count": 12,
      "work_dir": "E:\\proj\\a", "sandbox": null,
      "last_run": { "at": 1761878400, "ok": true, "exit_code": 0, "ms": 1834 } }
  ]
}
```
