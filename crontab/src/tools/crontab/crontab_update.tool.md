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
on_timeout: background
```

# 摘要
修改一条已存在的定时任务的调度和/或 `action`（省略 = 保持不变）。本工具是**部分更新**：只改传入的字段。

调度三选一（与 `crontab_add` 一致）：`cron`（周期）、`delay`（一次性相对时长）、`at`（一次性绝对时间）；提供任意一个即整体替换调度。把周期任务改成一次性、或反过来，都在这里完成。**显式空串/全空白会报错**（空值没有合法调度语义）；保持不变请省略该键。`null` 与省略同义。一次性任务执行完 / 错过后会终态停用（`completed_once` / `once_missed`）；显式改期会**重新武装**（恢复启用）。

`action` 语义与 `crontab_add` 一致：`{type:"run", param:{command, notify?}}` / `{type:"notify", param:HTML}` / `{type:"agent", param:{prompt, agent?, title?, display_name?, timeout_sec?, notify?}}`。`action` 省略或 `null` = 不修改。`sandbox` 可单独修改 `run` 任务的写隔离开关（省略 = 不变；`"inherit"` = 恢复继承插件配置）。

**next_run_at 口径**：只有传入调度参数且任务处于启用状态时才重算；仅改 `action`/`sandbox` **不会**推进下次执行时间（避免丢掉已到期但尚未认领的触发）。停用任务的 `next_run_at` 保持 0。时间按插件进程本地时区解释。

**在飞执行**：update 只影响后续触发；如果 `task.running=true`，已启动的那一次仍按旧定义跑完，不会被取消，也不会因旧的一次性执行收口而覆盖刚改好的新调度。

把动作改成 `run` / `agent` 时，复用任务创建时的 `work_dir`；目录缺失、不是目录或不在白名单时**直接拒绝**（避免改成首次触发即停用的死任务）。`notify` 不校验工作目录。

成功返回含 `changed` 列表（`cron/at/delay/action/sandbox/enabled`）；`changed:[]` 表示本次调用没有实际修改，可用于判断 no-op。

# 参数
```json
{
  "type": "object",
  "required": ["task_id"],
  "additionalProperties": false,
  "properties": {
    "task_id": { "type": "string", "description": "任务 id（ct_ 开头）" },
    "cron": { "type": "string", "description": "新的周期 cron（5 段或 @宏；省略 = 不变；显式空串报错；与 at/delay 互斥）" },
    "delay": { "type": "string", "description": "改为一次性：相对当前时间的时长（如 50m / 2h；省略 = 不变；显式空串报错；与 cron/at 互斥）" },
    "at": { "type": ["string", "integer"], "description": "改为一次性：绝对时间（RFC3339 / 2026-11-08 18:30 / unix 秒；省略 = 不变；显式空串报错；与 cron/delay 互斥；无偏移按本地时区）" },
    "action": {
      "type": "object",
      "required": ["type", "param"],
      "additionalProperties": false,
      "description": "新的动作（省略或 null = 不变）",
      "properties": {
        "type": { "type": "string", "enum": ["run", "notify", "agent"] },
        "param": { "type": ["object", "string"], "description": "run → {command, notify?}；notify → HTML 字符串或 {html, click_url?}；agent → {prompt, agent?, title?, display_name?, timeout_sec?, notify?}；未声明字段会被拒绝" }
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
{ "ok": true, "changed": ["cron"], "task": { "id": "ct_7f3a91c2", "description": "通知：该喝水了", "cron": "0 */2 * * *", "once": false, "schedule_type": "cron", "next_run_at": 1761888000, "next_run_at_local": "2026-10-31T10:00:00+08:00", "timezone": "CST", "enabled": true, "type": "notify", "run_count": 0, "running": false, "created_at": 1761878400, "work_dir": "", "sandbox": null } }
```

一次性任务返回含 `"once": true, "schedule_type": "once", "run_at": <unix秒>, "run_at_local": "<本地 RFC3339>"`。仅传 `task_id` 时返回 `"changed": []` 与当前任务快照。

失败：`isError=true`，正文为 JSON `{ "ok":false, "error": "<原因>" }`（任务不存在 / 调度参数非法、互斥或显式空串 / 一次性时间已过 / action 非法或含未声明字段 / run·agent 的 work_dir 不可用 / sandbox 非法）。
