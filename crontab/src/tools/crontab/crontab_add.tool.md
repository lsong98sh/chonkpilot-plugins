# 基本信息
```yaml
name: crontab_add
title: 新建定时任务
args: crontab_add --input={RAW-INPUT-FILE}
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
新建一条**本机全局**定时任务（默认立即生效，无需确认）。本工具**非幂等**：调用失败或超时后先 `crontab_list` 确认，避免重复创建。

**调度二选一**（必须且只能提供一组）：
- 周期：`cron` 为 5 段（分 时 日 月 周）或 `@daily` / `@hourly` / `@weekly` / `@monthly` / `@yearly`（`@annually` / `@midnight` 同义） / `@every <duration>`（时长 ≥ 1s）；不支持 6 段秒级字段。`@every` 是高频入口，如 `1s` 会快速滚动运行日志并持续拉起进程，非测试场景建议 ≥ 1m。
- 一次性：`delay`（相对当前时间，如 `50m` / `2h`）或 `at`（绝对时间：RFC3339 / `2026-11-08 18:30` / unix 秒）。用户说「50 分钟后做 X」→ 用 `delay:"50m"`；说「明天 9 点做 X」→ 用 `at:"2026-11-09 09:00"`。一次性任务执行一次后自动停用（`disabled_reason=completed_once`），保留历史；错过补偿窗口则标记 `once_missed` 停用。

**时间口径**：`cron` / `at` 与 `next_run_at` 均按**插件进程本地时区**解释；返回同时给出 `timezone` 与带偏移的 `next_run_at_local` / `run_at_local`（RFC3339），无需自行换算 epoch。`at` 不带偏移时按本地时区。

`action` 是三选一联合类型（各层对象均拒绝未声明字段）：
- `{type:"run", param:{command, notify?}}`：执行命令（`notify` = 完成后是否弹系统通知，可传 `true` 或自定义文案）；
- `{type:"notify", param:"<b>HTML</b>"}`（或 `{html, click_url?}`）：在时间点弹系统通知；
- `{type:"agent", param:{prompt, agent?, title?, display_name?, timeout_sec?, notify?}}`：以**完整智能体（全工具面）**执行 prompt，可读项目文件，适合「定时代码审查」；`agent` 指定场景智能体（省略 = 默认），`timeout_sec` 省略 / 0 = 插件默认超时，`notify` 省略 = true（完成后通知并附结果摘要）。执行结果写入任务运行记录（`output_tail`），并可在任务子会话查看完整对话。

**工作目录**：`run` / `agent` 必须由项目会话创建（`work_dir` 必须存在、是目录且命中配置白名单），否则直接失败；`notify` 不访问工作目录，可在无项目的全局会话创建。可选 `sandbox` 控制 `run` 是否以进程沙箱（Low IL + Job，只限写）执行（省略 / `"inherit"` = 继承插件配置）。创建成功会异步弹系统通知并在状态栏徽标即时计数。任务全局共享，不区分项目。

# 参数
```json
{
  "type": "object",
  "required": ["action"],
  "additionalProperties": false,
  "properties": {
    "cron": {
      "type": "string",
      "description": "周期任务的 cron 表达式（5 段：分 时 日 月 周；或 @daily/@hourly/@weekly/@monthly/@yearly/@every 5m）；与 at/delay 互斥"
    },
    "delay": {
      "type": "string",
      "description": "一次性相对时长（如 50m / 2h，必须 > 0）；与 cron/at 互斥。用户说「50 分钟后」用这个"
    },
    "at": {
      "type": ["string", "integer"],
      "description": "一次性绝对时间（RFC3339 / 2026-11-08 18:30 / unix 秒，必须晚于当前时间；无偏移按本地时区）；与 cron/delay 互斥"
    },
    "action": {
      "type": "object",
      "required": ["type", "param"],
      "additionalProperties": false,
      "description": "动作（run 执行命令 / notify 系统通知 / agent 完整智能体）",
      "properties": {
        "type": { "type": "string", "enum": ["run", "notify", "agent"] },
        "param": {
          "type": ["object", "string"],
          "description": "run → {command, notify?}；notify → HTML 字符串或 {html, click_url?}；agent → {prompt, agent?, title?, display_name?, timeout_sec?, notify?}；未声明字段会被拒绝"
        }
      }
    },
    "sandbox": {
      "type": ["boolean", "string"],
      "enum": [true, false, "inherit"],
      "description": "run 任务的写隔离（可选；省略或 \"inherit\" = 继承插件配置 plugins.crontab.sandbox）：true = 以 Low IL + Job 执行，只允许写工作目录 / 项目可写白名单；false = 不隔离"
    }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "task": {
    "id": "ct_7f3a91c2",
    "description": "执行：npm run build",
    "cron": "*/5 * * * *",
    "once": false,
    "schedule_type": "cron",
    "next_run_at": 1761878700,
    "next_run_at_local": "2026-10-31T09:05:00+08:00",
    "timezone": "CST",
    "enabled": true,
    "type": "run",
    "run_count": 0,
    "running": false,
    "created_at": 1761878400,
    "work_dir": "E:\\proj\\a",
    "sandbox": null
  }
}
```

一次性任务额外返回 `"once": true, "schedule_type": "once", "run_at": <unix秒>, "run_at_local": "<本地 RFC3339>"`；`next_run_at` 与 `run_at` 相同（执行后置 0）。未执行过的任务不含 `last_run/session_id/dialog_id`。

失败：`isError=true`，正文为 JSON `{ "ok": false, "error": "<原因>" }`（调度参数非法 / 互斥冲突 / 一次性时间已过 / action 非法或含未声明字段 / 任务数超上限 / run·agent 的 work_dir 缺失、不存在或不在白名单 / sandbox 非法）。`work_dir` 不合法时请改在项目会话中调用或改用 `notify`。
