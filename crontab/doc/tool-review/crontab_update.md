---
plugin: crontab
tool: crontab_update
contract: src/tools/crontab/crontab_update.tool.md
handler: src/main.go（crontabPlugin.update）
reviewed_at: 2026-11-10
status: verified
---

# crontab_update 审查

> 2026-11-08 首审；2026-11-10 按 `once/at/delay + agent` 重构后的实现复核并修复。

## 1. 一句话结论

可用。部分更新语义已明确：显式空调度字段报错、`null` = 省略、成功返回 `changed`（空数组 = no-op）、仅改 action/sandbox 不推进 `next_run_at`；写入失败回滚内存。剩余为无任务名、无并发版本控制、无任务级时区。

## 2. 调用链

1. LLM `crontab_update` → gateway `doCall`（剥离 `tool_call_display_name`，同步执行）。
2. 插件 `crontabPlugin.update`：
   - `rejectUnknownArgs`（只允许 `task_id/cron/at/delay/action/sandbox`）→
   - `optionalTextArg` 读取调度字段（空串报错；null/缺失 = 不变；`at` 接受 unix 秒）→
   - `parseScheduleInput`（三选一，互斥校验）→
   - `ParseActionInput`（严格字段）→ `parseSandboxArg` → `validateWorkDirForAction` →
   - `Store.UpdateTx`（锁内 mutate + 落盘；任一失败回滚）→ `notifyChanged`。
3. 错误由 `withJSONError` 统一为 `isError=true` + `{"ok":false,"error":"…"}`。

## 3. 契约面

- YAML：`async: never`（→sync）、`timeout: 30`、`idempotent: false`。
- 摘要：明确“只改传入字段”“显式空串报错”“null=省略”“只有调度变化才重算 next_run_at”“停用任务保持 0”“`changed:[]` = no-op”。
- schema：顶层与 `action` `additionalProperties:false`；`sandbox` 三态完整；`action` 省略或 null = 不变。
- 返回：`{ok, changed, task}`；失败为 `isError=true` + JSON。

## 4. 实现面

- **task_id**：TrimSpace 后非空；先 `Get` 预检存在，再在 `UpdateTx` 锁内二次确认。
- **调度字段**：
  - 键缺失 / `null` → 未提供（保持不变）；
  - 显式空串 / 全空白 → `cron|at|delay 不能为空（保持不变请省略该字段）`；
  - 非字符串（`at` 除外）→ 明确报错，不再被 `argString` 静默吞掉；
  - 提供任意一个即整体替换调度，互斥/时间校验复用 `parseScheduleInput`。
- **action**：省略/null 不变；其余严格解析并重写 `Action` + `Description`。
- **sandbox**：三态覆盖；`"inherit"` → `nil`。
- **work_dir**：最终动作是 `run`/`agent` 时，复用任务创建时的目录并校验非空/存在/白名单；`notify` 跳过。
- **next_run_at**：
  - 仅在 `scheduleSet && Enabled` 时重算；
  - 仅改 `action`/`sandbox` **不推进**（修复“到期未认领的触发被 update 丢掉”）；
  - 停用任务置 0；
  - 一次性 `completed_once`/`once_missed` 显式改期 → 恢复启用并回显 `enabled`。
- **落盘一致性**：`Store.UpdateTx` 保存修改前快照；`mutate` 或 `persistLocked` 失败时恢复内存与磁盘一致。调度器内部用于跳过标记的 `Store.Update` 保持原语义（失败保留内存标记）。
- **在飞执行**：update 不取消已认领的执行；旧执行继续按旧定义跑完，`task.running=true`。旧的一次性执行收口只在“当前定义仍是同一次 `once+run_at`”时自动停用，避免覆盖运行期间改期/改成 cron 的新定义。
- **返回 changed**：`cron/at/delay/action/sandbox` 按请求字段记录；重新武装追加 `enabled`；无修改为空数组。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 成功（有修改） | false | `{ok:true,changed:["cron"],task:{...}}` |
| 成功（no-op，仅 task_id / 全 null） | false | `{ok:true,changed:[],task:{...}}`，`next_run_at` 不变 |
| 停用任务改调度 | false | `enabled:false,next_run_at:0`（只改定义） |
| 任务不存在 | true | `{"ok":false,"error":"任务不存在"}` |
| 调度非法/互斥/空串 | true | `{"ok":false,"error":"…"}` |
| action 非法/未声明字段 | true | `{"ok":false,"error":"…"}` |
| run/agent 的 work_dir 不可用 | true | `{"ok":false,"error":"work_dir …"}` |
| 落盘失败 | true | `{"ok":false,"error":"…"}`，内存已回滚 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 无并发版本控制 | 两个会话同时 update 同一任务仍是 last-write-wins；返回 `changed` 只能证明本次请求字段被接受，不能证明未被覆盖。需要时再加 `version/expected_next_run_at`。 |
| P2 | 无自定义任务名 / 无法改 `work_dir` | `description` 由 action 派生；任务改名、迁移项目无工具入口。 |
| P2 | `action:null` / `sandbox:null` 被当作省略 | 契约已写明 null=不变，`changed` 可判断是否 no-op；若要更严格，可在后续拒绝显式 null（schema 未声明 null）。 |
| P3 | 无 `dry_run` | 高风险改动无法先预览；当前只能改后看返回。 |
| P3 | 在飞执行按旧定义跑完 | update 不取消在飞任务；`task.running` 已暴露，契约已说明。若产品要求改动作立即生效，需要取消信号。 |
| P3 | 时区为进程本地 | 已返回本地 RFC3339，但无法指定 IANA 时区。 |
| P3 | 无 operation id | 重试同一 update 通常幂等（同值覆盖），但 `next_run_at` 会按第二次的 now 重算；调度改期重试可能轻微偏移。 |

已修（2026-11-10）：`cron:""` 静默 no-op、成功无 `changed`、仅改 action/sandbox 推进 `next_run_at`、`mutate`/落盘失败内存残留、失败返回形态、`action`/顶层未知字段静默丢弃、改 run/agent 绕过 work_dir 校验、摘要“立即重算”与停用任务不符。

## 7. 测试/验收

- `TestUpdateRejectsRunActionWithoutWorkDir`：改 run 时校验 work_dir，失败不改动作。
- `TestUpdateNoopReturnsEmptyChanged`：仅 task_id → `changed:[]` 且 `next_run_at` 不变。
- `TestUpdateRejectsEmptyScheduleArgs`：`cron/at/delay` 空串/空白报错；null 视为省略；非字符串报错。
- `TestUpdateActionOnlyKeepsDueNextRun`：仅改 action 不推进已到期的 `next_run_at`。
- `TestUpdateWhileRunningKeepsExecution`：在飞执行不被 update 取消，返回 `task.running=true`。
- `TestExecuteOnceCompletionDoesNotClobberUpdatedTask` / `...RearmedOnce`：旧一次性执行收口不覆盖改期/改成 cron 的新定义。
- `TestUpdateRearmsCompletedOnce`：`completed_once` 改期恢复启用、`next_run_at=run_at`、`changed` 含 `enabled`。
- `TestStoreUpdateTxRollsBackOnPersistFailure`：落盘失败回滚内存。
- 回归：crontab 插件 `go test ./...` 全量通过。