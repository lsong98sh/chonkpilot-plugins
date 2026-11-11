# 基本信息
```yaml
name: crontab_toggle
title: 启用/停用定时任务
args: crontab_toggle --input={RAW-INPUT-FILE}
async: never
category: crontab
hot: true
runtime: plugin
idempotent: true
output: json
timeout: 30
on_timeout: background
```

# 摘要
启用或停用一条定时任务（`enabled:true` 启用 / `false` 停用）。任务定义始终保留。

**启用**：
- 原本停用：置 `enabled=true`、清 `disabled_reason`、按调度重算 `next_run_at`（一次性任务为 `run_at`；已过期的一次性任务直接报错，需先 `crontab_update` 改期）。
- 原本已启用且 `next_run_at>0`：**保持原值**，不推进已到期触发；返回 `changed:false`。重复启用因此是真幂等。
- 已启用但 `next_run_at=0`（异常/脏数据）：补算一次。
- `run` / `agent` 启用前校验创建时的 `work_dir`：缺失、不是目录或不在白名单时直接拒绝（避免“启用成功、下一轮又被 work_dir_missing 停用”）；`notify` 不依赖工作目录。

**停用**：
- 置 `enabled=false`、`next_run_at=0`，保留定义与历史。
- 不中止已经启动的执行；命令会继续跑完并写入 `last_run`。
- 重复停用是幂等的，第二次返回 `changed:false`。

`changed` 表示本次是否实际修改了 `enabled/next_run_at/disabled_reason`。`running` 表示当前是否有已启动、尚未收口的执行（停用不会中止它）。

# 参数
```json
{
  "type": "object",
  "required": ["task_id", "enabled"],
  "additionalProperties": false,
  "properties": {
    "task_id": { "type": "string", "description": "任务 id（ct_ 开头）" },
    "enabled": { "type": "boolean", "description": "true = 启用；false = 停用" }
  }
}
```

# 返回值
```json
{ "ok": true, "task_id": "ct_7f3a91c2", "enabled": true, "changed": true, "running": false, "task": { "id": "ct_7f3a91c2", "description": "执行：npm run build", "cron": "*/5 * * * *", "once": false, "schedule_type": "cron", "next_run_at": 1761878700, "next_run_at_local": "2026-10-31T09:05:00+08:00", "timezone": "CST", "enabled": true, "type": "run", "run_count": 12, "running": false, "created_at": 1761878400, "work_dir": "E:\\proj\\a", "sandbox": null } }
```

停用时 `enabled:false`、`next_run_at:0`；重复调用对应方向为 `changed:false`。

失败：`isError=true`，正文为 JSON `{ "ok":false, "error": "<原因>" }`（`task_id`/`enabled` 缺失或类型非法 / 未知参数 / 任务不存在 / 一次性任务已过期 / 历史 cron 非法 / 启用 run·agent 时 work_dir 不可用 / 落盘失败）。