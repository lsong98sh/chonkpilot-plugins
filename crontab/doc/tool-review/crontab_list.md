---
plugin: crontab
tool: crontab_list
contract: src/tools/crontab/crontab_list.tool.md
handler: src/main.go（crontabPlugin.list）
reviewed_at: 2026-11-10
status: verified
---

# crontab_list 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的“无分页、跳过不可见、epoch 无时区、示例 exit_code”等问题已在当前实现修复。

## 1. 一句话结论

可用。只读、并发安全，已具备 `task_id` 精确查询、`enabled` 过滤、`order`、`limit/offset`、`output_tail_bytes` 体积控制，返回 `total/truncated` 与本地时间字段；剩余只是诊断深度与极端体积取舍。

## 2. 调用链

1. LLM `crontab_list` → gateway 同步转发插件。
2. `crontabPlugin.list`：
   - `rejectUnknownArgs`（只允许 `task_id/enabled/order/limit/offset/output_tail_bytes`）→
   - `optionalBoolArg` / `optionalTextArg` / `optionalIntArg` 严格解析 →
   - `Store.List()` 锁内快照（按 `CreatedAt, ID` 升序）→ `desc` 时在本地切片反转 →
   - 按 `task_id` / `enabled` 过滤 → `offset` 跳过、`limit` 截断 → `taskViewWithTail` 裁剪 `output_tail`。
3. 参数错误由 `withJSONError` 统一为 `isError=true` + `{"ok":false,"error":"…"}`。

## 3. 契约面

- YAML：`async: never`（→sync）、`timeout: 30`、`idempotent: true`。
- schema：`additionalProperties:false`；`order enum [asc,desc]`；`limit 1..200`；`offset >=0`；`output_tail_bytes 0..4096`。
- 摘要：默认 `order=desc`（新任务优先）、`count/total/truncated` 语义、`last_run` 字段与 `skipped missed/overlap/workdir-missing`、`exit_code` 仅非 0 出现、参数类型错误会失败。
- 返回示例含 `next_run_at_local/run_at_local/timezone` 与真实 `last_run` 形态。

## 4. 实现面

- **过滤**：`enabled` 仅接受 bool / `true|false|1|0|yes|no|on|off`；非法类型或值显式报错，不再静默返回全部。
- **精确查询**：`task_id` 非空精确匹配；未命中返回 `ok:true,tasks:[],count:0,total:0`（不是错误）；显式空串报错。
- **排序**：默认 `desc`（创建时间新→旧），`asc` 为旧→新；排序后再过滤/分页。
- **分页**：`limit` 默认 50、上限 200；`offset` 在过滤/排序后跳过。`count` = 本次返回，`total` = 过滤后总数，`truncated = total > offset+count`。
- **体积**：`output_tail` 默认 512 **字节**，`0` 不返回，上限 4096。`limit=200 + tail=4096` 最坏约 800KB，需调用方显式请求；默认约 25KB 尾部 + 任务体。
- **跳过可见性**：调度器已把 `missed` / `overlap` / `workdir-missing` 写入 `Task.LastRun`，list 直接可见（`kind=skipped`）。
- **时区**：`taskView` 返回 `timezone` 与本地 RFC3339；`next_run_at/run_at` 保留 epoch 秒。
- **在飞执行**：`taskView.running` 暴露 `RunningSince>0`；delete/toggle/update 均不会取消它。
- **并发/副作用**：`Store.List` 持锁快照；只读、无 goroutine、无文件写入。
- **参数类型**：`limit/offset/output_tail_bytes` 接受整数/整值浮点/数字字符串，其他类型报错；越界 `limit` 截到 200、`output_tail_bytes` 截到 4096；负数报错。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有任务 | false | `{ok:true, count,total,truncated,limit,offset,order,output_tail_bytes,tasks:[…]}` |
| 空列表 / task_id 未命中 | false | `tasks:[]`, `count:0`, `total:0` |
| `task_id` 查询 | false | 额外回显 `task_id` |
| `enabled`/`order`/`limit`/`offset`/`output_tail_bytes` 非法 | true | `{"ok":false,"error":"…"}` |
| 未知参数 | true | `{"ok":false,"error":"不支持的参数: …"}` |

每条任务字段：`id/description/cron/once/schedule_type/run_at?/next_run_at/next_run_at_local?/run_at_local?/timezone/enabled/type/run_count/running/created_at/work_dir/sandbox/disabled_reason?/session_id?/dialog_id?/last_run?`。

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 显式 `limit=200 & output_tail_bytes=4096` 最坏约 800KB | 默认值已安全（50×512B）；这是调用方主动请求的上限。若仍担心，可再加服务端总输出预算或 `include_last_run` 开关。 |
| P2 | `last_run.at` 仍是 epoch | 任务级 `next_run_at_local` 已有；`last_run.at` 需要模型结合 `timezone` 换算。可后续加 `at_local`。 |
| P3 | 排序仅按创建时间 | 无法按 `next_run_at` 排序回答“接下来跑什么”；可用 `order/limit` + 模型自行比较。 |
| P3 | `last_run.error/detail` 未单独截断 | 正常错误较短；若 Agent 返回超长错误文本，会进入 `last_run.error`。可加独立上限。 |
| P3 | `offset` 不是游标 | 任务数变化时翻页可能重复/漏项；当前任务上限通常 200，影响有限。 |

已修（2026-11-10）：参数类型静默忽略、无 `task_id`/`order`/`offset`、默认顺序不适合 limit 截断、`enabled` 非法静默返回全部、`next_run_at` 无本地时间、`last_run` 字段未文档化、示例 `exit_code:0` 与 `omitempty` 不符；跳过事件写入 `LastRun` 为更早批次修复。

## 7. 测试/验收

- `TestListLimitAndOutputTail`：limit/truncated/total 与 tail 截断、`0` 不返回 tail。
- `TestListOrderAndTaskID`：默认 desc、asc+limit、`task_id` 命中/未命中、offset 生效与越界。
- `TestListRejectsInvalidArgs`：`enabled` 非布尔、`limit` 非整数/小于 1、负 offset、负 tail、`order` 非法、空 `task_id`、未知参数均报错。
- `TestListClampsAndReportsEffectiveLimits`：`limit` 截到 200、`output_tail_bytes` 截到 4096 并回显。
- 回归：crontab 插件 `go test -count=1 ./...` 全量通过。