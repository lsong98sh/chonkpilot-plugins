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
on_timeout: background
```

# 摘要
只读列出本机定时任务。支持 `task_id` 精确查询、`enabled` 过滤、`order` 排序与 `limit` / `output_tail_bytes` 体积控制。

每条任务返回 `id / 简述 / cron / schedule_type / once / run_at / next_run_at / next_run_at_local / timezone / 是否启用 / 动作类型 / 最近一次结果 / running`，另含 `work_dir`、`sandbox`（任务级开关，`null` = 继承插件配置）与 `disabled_reason`：
- `work_dir_missing`：项目目录不存在被自动停用（可恢复）；
- `completed_once`：一次性任务已执行完成（终态，需改期/新建）；
- `once_missed`：一次性任务在补偿窗口外错过（终态）。

动作类型 `type` 取值 `run / notify / agent`。`schedule_type=once` 时 `cron` 为空、`run_at` 为计划执行时间。任务全局共享，不区分项目。调度时间按插件进程本地时区解释，`next_run_at_local` / `run_at_local` 为带偏移的 RFC3339。

- 默认 `order:"desc"`（新任务优先）；`limit` 缺省 50、硬上限 200；`offset` 用于翻页（过滤/排序后跳过前 N 条）。`count` = 本次返回条数，`total` = 过滤后总数；`truncated=true` 表示还有未返回项。
- `last_run.output_tail` 按 `output_tail_bytes`（缺省 512 **字节**，上限 4096）截断；`0` = 不返回该字段。
- 自动跳过也会写入 `last_run`：`kind=skipped` + `detail=missed|overlap|workdir-missing`，用于诊断漏跑/重入。
- `running=true` 表示已有一次执行被认领且尚未收口；delete / toggle / update 都不会取消它。
- `last_run` 字段：`at/kind/ok/ms?/error?/detail?/output_tail?/session_id?/dialog_id?/turn_id?`；`exit_code` 仅在非 0 时出现。`kind` 可取 `run/notify/agent/skipped/interrupted`。
- 参数类型错误（如 `enabled:1`、`limit:"abc"`、`order:"up"`）会失败，不再静默按默认值处理。

# 参数
```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task_id": { "type": "string", "description": "精确查询单个任务 id（ct_ 开头）；省略 = 全部。显式空串报错" },
    "enabled": { "type": "boolean", "description": "只返回启用（true）或停用（false）的任务；省略 = 全部" },
    "order": { "type": "string", "enum": ["asc", "desc"], "description": "按创建时间排序：desc = 新任务优先（缺省），asc = 最早优先" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 200, "description": "返回任务数上限；缺省 50，硬上限 200" },
    "offset": { "type": "integer", "minimum": 0, "description": "过滤/排序后跳过的任务数；缺省 0，用于翻页" },
    "output_tail_bytes": { "type": "integer", "minimum": 0, "maximum": 4096, "description": "last_run.output_tail 的字节上限；缺省 512，0 = 不返回该字段" }
  }
}
```

# 返回值
```json
{
  "ok": true,
  "count": 1,
  "total": 1,
  "truncated": false,
  "limit": 50,
  "offset": 0,
  "order": "desc",
  "output_tail_bytes": 512,
  "tasks": [
    { "id": "ct_7f3a91c2", "description": "执行：npm run build", "cron": "*/5 * * * *",
      "once": false, "schedule_type": "cron",
      "next_run_at": 1761878700, "next_run_at_local": "2026-10-31T09:05:00+08:00",
      "timezone": "CST", "enabled": true, "type": "run", "run_count": 12,
      "running": false,
      "work_dir": "E:\\proj\\a", "sandbox": null,
      "last_run": { "at": 1761878400, "kind": "run", "ok": true, "ms": 1834,
                    "output_tail": "Build succeeded" } },
    { "id": "ct_8a1b2c3d", "description": "智能体：代码审查", "cron": "",
      "once": true, "schedule_type": "once", "run_at": 1761881700,
      "run_at_local": "2026-10-31T10:15:00+08:00",
      "next_run_at": 0, "enabled": false, "disabled_reason": "completed_once",
      "type": "agent", "run_count": 1,
      "last_run": { "at": 1761881705, "kind": "agent", "ok": true, "ms": 42000,
                    "output_tail": "审查结论：…" } }
  ]
}
```

按 `task_id` 查询时额外返回 `"task_id":"ct_…"`；空结果仍为 `ok:true` + `tasks:[]`。失败（参数非法）为 `isError=true` + JSON `{ "ok":false, "error":"…" }`。
