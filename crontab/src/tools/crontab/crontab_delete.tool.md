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
on_timeout: background
```

# 摘要
永久删除一条定时任务（定义从 tasks.json 移除；历史运行记录保留在 runs.jsonl 中供排查）。删除后状态栏徽标即时更新。

- 返回 `remaining` = 删除后剩余任务总数（`count` 是同一值的旧别名）。
- 删除**不会中止**已经启动的执行：若 `running:true`，命令仍会在后台跑完并写入 runs 历史。
- 任务不存在返回错误（不是幂等成功）；失败/超时后请先 `crontab_list` 确认是否已被删除。
- 删除任务不会删除其历史运行记录或已创建的任务子会话。

# 参数
```json
{
  "type": "object",
  "required": ["task_id"],
  "additionalProperties": false,
  "properties": {
    "task_id": { "type": "string", "description": "任务 id（ct_ 开头）" }
  }
}
```

# 返回值
```json
{ "ok": true, "task_id": "ct_7f3a91c2", "deleted": true, "remaining": 4, "count": 4, "running": false }
```

失败：`isError=true`，正文为 JSON `{ "ok":false, "error": "<原因>" }`（`task_id` 为空 / 任务不存在 / 未知参数 / 落盘失败）。落盘失败会回滚内存，任务仍存在。
