---
plugin: crontab
tool: crontab_add
contract: src/tools/crontab/crontab_add.tool.md
handler: src/main.go（crontabPlugin.add）
reviewed_at: 2026-11-10
status: verified
---

# crontab_add 审查

> 2026-11-08 首审；2026-11-10 按 `once/at/delay + agent` 重构后的实现复核并修复。§6 的“已修”项保留原问题描述，便于回归对照。

## 1. 一句话结论

可用。调度三选一、动作三选一、失败返回、任务上限、落盘回滚、`work_dir` 前置校验、系统通知异步化均已成立；剩余只有产品级取舍（非幂等重试、进程本地时区、`@every` 高频下限）。

## 2. 调用链

1. LLM `crontab_add` tool_call → gateway `doCall`：剥离 `tool_call_display_name` / 调用级 `async|timeout` 后转发插件；本工具同步执行（契约 `async: never`→`sync`）。
2. 插件 `crontabPlugin.add`（`src/main.go`）：
   - `rejectUnknownArgs`（顶层只允许 `cron/at/delay/action/sandbox`）→
   - `parseScheduleInput`（`cron` / `at` / `delay` 三选一）→
   - `ParseActionInput`（`run` / `notify` / `agent`，各层拒绝未声明字段）→
   - `parseSandboxArg`（三态）→ `validateWorkDirForAction`（动作相关）→
   - `Action.Canonical` → `nextRun` → `Store.AddLimited`（锁内上限 + 落盘回滚）→
   - `notifyChanged`（UI 事件）→ 创建成功通知（**goroutine**）→ 返回 `{ok:true,task}`。
3. 错误统一由 `withJSONError` 包装：MCP `isError=true`，正文 `{"ok":false,"error":"…"}`。

## 3. 契约面

- YAML：`async: never`（解析为 `sync`）、`timeout: 30`、`idempotent: false`、`hot: true`。`contract.Parse` 已解析 `async/timeout/on_timeout` 并经 `contract.MetaFor` 下发，不再是纯装饰。
- 摘要：已覆盖 5 段/宏、`@yearly/@annually/@midnight`、`@every ≥1s`、一次性自动停用、动作三选一、`display_name`、本地时区、`run/agent` 的 `work_dir` 前置要求、`notify` 无项目可用、非幂等提示。
- schema：顶层与 `action` 均 `additionalProperties:false`；`sandbox` 为 `[boolean,string]` + `enum [true,false,"inherit"]`；返回值含 `run_count/created_at/work_dir/timezone/next_run_at_local/run_at_local`。
- 失败：契约明确 `isError=true` + JSON `{ok:false,error}`，与实现一致。

## 4. 实现面

- **调度**：`parseScheduleInput` 拒绝 `cron+at/delay`、`at+delay`、三者皆空；`at` 必须晚于 now；`delay > 0`。`parseCron` 只接受 5 段或宏，6 段（含秒字段）拒绝；`@every` 下限 1s。
- **时区**：按插件进程本地时区解释；`taskView` 同时返回 `next_run_at_local` / `run_at_local`（RFC3339 带偏移）与 `timezone`，不再要求 LLM 自行换算 epoch。任务级 `TZ=`/`CRON_TZ=` 仍不支持（见 §6）。
- **工作目录**：
  - `run` / `agent`：必须非空、存在且为目录、命中 `allowed-work-dirs`；否则在**写入前**返回明确错误，不再生成“创建成功、首次触发即 `work_dir_missing` 停用”的死任务。
  - `notify`：不访问工作目录，允许无项目；调度器 `execute` 对 `notify` 跳过 `work_dir_missing` 检查。
  - `crontab_update` 把动作改成 `run` / `agent` 时复用任务创建时的 `work_dir`，同样前置校验。
- **上限与落盘**：`Store.AddLimited` 在锁内检查 `MaxTasks` 并 append；`persistLocked` 失败时回滚内存。`Delete` 同样在落盘失败时恢复被删项。
- **通知**：创建成功 toast 改为 goroutine（PowerShell 阻塞不再拉长 tool 调用）；失败只记插件日志。
- **参数封闭**：顶层未知参数、`action`/`param` 内未声明字段均显式报错，不再静默丢弃 `name/description` 等键。
- **幂等**：非幂等；契约提示失败/超时后先 `crontab_list` 再重试。

## 5. 返回面

| 形态 | 引擎 isError | LLM 可见 |
|---|---|---|
| 成功（周期） | false | `{ok:true,task:{id,description,cron,once:false,schedule_type:"cron",next_run_at,next_run_at_local,timezone,enabled,type,run_count,created_at,work_dir,sandbox}}` |
| 成功（一次性） | false | 额外 `run_at/run_at_local`；`next_run_at` = `run_at`，执行后 0 |
| 调度非法/互斥 | true | `{"ok":false,"error":"…"}` |
| action 非法/未声明字段 | true | `{"ok":false,"error":"…"}` |
| run/agent 无可用 work_dir | true | `{"ok":false,"error":"work_dir …"}` |
| 任务数上限 | true | `{"ok":false,"error":"任务数已达上限（N）"}` |
| 落盘失败 | true | `{"ok":false,"error":"…"}`，且任务不会留在内存 |

成功返回不含 `last_run/session_id/dialog_id`（未执行过）。

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | `@every 1s` 仍允许 | 契约已明确下限 1s；1s 频率会快速滚动 `runs.jsonl`（1MB×3）并持续拉起进程。若要限流，需产品决定把新任务下限提到 1m，并处理已有 1s 任务迁移。 |
| P2 | 无任务级时区 | 全部按插件进程本地时区；现已返回 `timezone` / 本地 RFC3339，可被模型正确解读，但跨时区用户无法指定 `Asia/Shanghai` 等。 |
| P2 | 非幂等 | gateway 超时/调用方重试仍可能重复创建；已异步化 toast 消除主要超时源，并在契约中要求先 `list` 再重试。彻底解决需 `idempotency_key` + 持久去重。 |
| P3 | 无自定义任务名 | 简述由 action 派生；未知 `name/description` 现在会明确报错，不再静默丢弃。 |
| P3 | 白名单拒绝无自愈 | 模型只能转告用户调整目录/配置。 |
| P3 | 失败用 `isError` + JSON 文本而非 MCP `structuredContent` | SDK 对非 nil error 统一 `mcpError`，当前无结构化错误通道；正文已可稳定 JSON 解析。 |

已修（2026-11-10）：失败形态与契约不符、空/不存在 `work_dir` 建死任务、`Store.Add` 落盘失败后内存残留、上限检查非原子、同步 toast 造成超时重试、`sandbox:"inherit"` 未声明、返回字段不全、`action`/顶层未知字段静默丢弃、时区不可读、`update` 改动作绕过 work_dir 校验。

## 7. 测试/验收

- `TestAddRejectsUnknownArgs`：顶层未知参数明确报错。
- `TestAddRequiresWorkDirForRunAndAgent` / `TestAddRunRejectsMissingWorkDir`：run/agent 空/不存在目录拒绝且不落库。
- `TestAddNotifyWithoutWorkDirSucceeds`：notify 可在无项目会话创建，返回本地时间字段。
- `TestAddRunWithExistingWorkDirSucceeds`：正常 run 创建。
- `TestExecuteNotifyWithoutWorkDir` / `TestExecuteRunDisablesOnMissingWorkDir`：调度器只对 run/agent 做 `work_dir_missing`。
- `TestStoreAddLimited` / `TestStoreAddRollsBackOnPersistFailure`：上限同锁、落盘失败回滚。
- `TestWithJSONError`：错误正文为 `{ok:false,error}` JSON。
- `TestParseActionErrors` / `TestParseActionAgentDisplayName`：动作各层未知字段拒绝，`display_name` 可用。
- `TestTaskViewLocalFields`：`next_run_at_local` 与本地 RFC3339 一致。
- 回归：crontab 插件 `go test ./...` 全量通过。