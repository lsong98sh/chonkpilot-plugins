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
```

# 摘要
新建一条**本机全局**定时任务（默认立即生效，无需确认）。`cron` 为 5 段（分 时 日 月 周）或 `@daily` / `@hourly` / `@weekly` / `@monthly`；不支持秒级。`action` 是二选一联合类型：`{type:"run", param:{command, notify?}}` 执行命令（`notify` = 完成后是否弹系统通知，可传 `true` 或自定义文案）；`{type:"notify", param:"<b>HTML</b>"}`（或 `{html, click_url?}`）在时间点弹系统通知。`run` 的默认工作目录 = 创建任务时的会话 work_dir；可选 `sandbox` 控制该任务是否以进程沙箱（Low IL + Job，只限写）执行（省略 = 继承插件配置）。创建成功会弹系统通知并在状态栏徽标即时计数。任务全局共享，不区分项目。

# 参数
```json
{
  "type": "object",
  "required": ["cron", "action"],
  "properties": {
    "cron": {
      "type": "string",
      "description": "cron 表达式（5 段：分 时 日 月 周；或 @daily/@hourly/@weekly/@monthly）"
    },
    "action": {
      "type": "object",
      "required": ["type", "param"],
      "description": "动作（run 执行命令 / notify 系统通知）",
      "properties": {
        "type": { "type": "string", "enum": ["run", "notify"] },
        "param": {
          "type": ["object", "string"],
          "description": "run → {command, notify?}；notify → HTML 字符串或 {html, click_url?}"
        }
      }
    },
    "sandbox": {
      "type": "boolean",
      "description": "run 任务的写隔离（可选；省略 = 继承插件配置 plugins.crontab.sandbox）：true = 以 Low IL + Job 执行，只允许写工作目录 / 项目可写白名单；false = 不隔离"
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
    "next_run_at": 1761878700,
    "enabled": true,
    "type": "run",
    "sandbox": null
  }
}
```

失败时 `ok:false` 并带 `error`（cron 非法 / action 非法 / 任务数超上限 / work_dir 不在白名单 / sandbox 非法）。
