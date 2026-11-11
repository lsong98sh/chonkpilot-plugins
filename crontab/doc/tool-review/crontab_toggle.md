---
plugin: crontab
tool: crontab_toggle
contract: src/tools/crontab/crontab_toggle.tool.md
handler: src/main.go（crontabPlugin.toggle → setEnabled）
reviewed_at: 2026-11-10
status: verified
---

# crontab_toggle 审查

> 2026-11-10 由 `crontab_enable` / `crontab_disable` 合并而来；合并时保留 enable 的 work_dir 校验、幂等语义与 disable 的“不中止在飞执行”语义。

## 1. 一句话结论

可用。一个工具覆盖两个方向：`enabled:true` 启用（幂等、校验 work_dir、拒绝过期一次性任务），`enabled:false` 停用（清 next、保留定义、不中止在飞执行）；返回 `changed` 区分 no-op，失败为 JSON。

## 2. 调用链

1. LLM `crontab_toggle` → gateway 同步转发插件。
2. `crontabPlugin.toggle`：
   - `rejectUnknownArgs`（只允许 `task_id/enabled`）→
   - `optionalBoolArg` 解析必填 `enabled`（bool / true|false|1|0|yes|no|on|off）→
   - `setEnabled(args, enabled)`：`Store.Get` 预检 + 启用方向 `validateWorkDirForAction` → `Store.UpdateTx`（锁内 mutate + 落盘；失败回滚）→ `notifyChanged`。
3. UI 的 `toggle` RPC 复用 `setEnabled`，不经过工具参数校验。
4. 错误统一为 `isError=true` + `{"ok":false,"error":"…"}`。

## 3. 契约面

- YAML：`async: never`（→sync）、`idempotent: true`。
- schema：`task_id` + `enabled` 均 required，`additionalProperties:false`。
- 摘要：覆盖启用/停用两个方向、幂等语义、过期一次性任务拒绝、run/agent 的 work_dir 校验、notify 放行、停用不中止在飞执行、`changed` 含义。
- 返回：`{ok,task_id,enabled,changed,task}`；失败带 JSON error。

## 4. 实现面

### 启用（enabled=true）
- 已启用且 `next_run_at>0` → 不修改（即使已到期未认领也保留，避免丢触发）；`changed:false`。
- 已启用但 `next_run_at==0` → 补算一次。
- 停用任务 → 置启用并按调度重算；一次性任务用 `run_at`。
- 一次性任务停用且 `run_at<=now` → 明确拒绝，提示先 `crontab_update` 改期；`run_at<=0` → 拒绝脏数据。
- `run`/`agent` 启用前校验 `work_dir` 非空、存在、目录、白名单；失败不改状态。`notify` 跳过。
- 清空 `disabled_reason`。

### 停用（enabled=false）
- 置 `enabled=false`、`next_run_at=0`，保留 `Cron/RunAt/Action/Description/CreatedBy/RunCount/LastRun`。
- 不写 `RunningSince`、不发取消信号：已启动的执行继续跑完并更新 `last_run`。
- 重复停用 `changed:false`。
- 不清理 `disabled_reason`（保留自动停用原因，见 §6）。

### 共用
- `Store.UpdateTx` 在 `nextRun` 失败或落盘失败时回滚内存。
- `changed` = 修改前后的 `enabled/next_run_at/disabled_reason` 是否有差异。
- 不重置 `RunCount/LastRun/SessionID/DialogID/CreatedBy`。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 停用→启用 | false | `enabled:true, changed:true, task:{next_run_at>now, disabled_reason 清除}` |
| 重复启用（next 未变） | false | `enabled:true, changed:false` |
| 已启用但 next=0（补算） | false | `changed:true`、新 next |
| 启用→停用 | false | `enabled:false, changed:true, running`（在飞时为 true），`next_run_at:0`，`running_since` 保留 |
| 重复停用 | false | `enabled:false, changed:false` |
| 一次性任务已过期 | true | `{"ok":false,"error":"一次性任务已过期…"}` |
| run/agent 的 work_dir 不可用 | true | `{"ok":false,"error":"work_dir …"}` |
| 历史 cron 非法 | true | `{"ok":false,"error":"cron …"}`，状态回滚 |
| `task_id`/`enabled` 缺失或非法 / 未知参数 | true | `{"ok":false,"error":"…"}` |
| 任务不存在 / 落盘失败 | true | JSON error |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 停用不中止在飞执行 | 契约已明确；若产品需要“立即停”，需给 scheduler 加取消信号（当前只有进程退出/超时）。 |
| P2 | 停用保留 `disabled_reason` | 若任务此前是 `work_dir_missing`/`once_missed`，手动停用后 `list` 仍显示原自动原因；语义上仍属实，但可能混淆。可后续加 `disabled_by=user` 或清空原因。 |
| P2 | 无批量 toggle | 200 个任务需 N 次调用；可后续加 `task_ids`。 |
| P3 | 时区为进程本地 | 已返回 `next_run_at_local/timezone`，无法指定 IANA 时区。 |

已修（2026-11-10）：重复 enable 推进 `next_run_at` 丢触发、启用 `work_dir_missing` 任务假成功、`nextRun`/落盘失败内存残留、契约缺失败面/`changed`/完整返回、unknown args 被忽略、enable/disable 返回不对称。

## 7. 测试/验收

- `TestToggleEnableClearsReasonAndComputesNext`：停用→启用、清 reason、next 未来。
- `TestToggleEnableIsIdempotentAndKeepsDueTime`：重复启用 `changed:false` 且不推进已到期时间。
- `TestToggleEnableRejectsRunWithoutWorkDir` / `TestToggleEnableNotifyWithoutWorkDir`：run 拒绝、notify 放行。
- `TestToggleEnableRejectsExpiredOnce`：过期一次性任务拒绝且状态不变。
- `TestToggleEnableInvalidCronRollsBack`：非法 cron 失败后内存回滚。
- `TestToggleDisableIsIdempotentAndKeepsRunning`：停用清 next、保留定义与 `running_since`、返回 `running:true`、重复停用 `changed:false`。
- `TestToggleRejectsMissingOrInvalidEnabled`：缺 `enabled`、非法类型、数字、未知参数均报错。
- 回归：crontab 插件 `go test -count=1 ./...` 全量通过。