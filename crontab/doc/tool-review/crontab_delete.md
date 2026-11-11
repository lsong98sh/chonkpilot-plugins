---
plugin: crontab
tool: crontab_delete
contract: src/tools/crontab/crontab_delete.tool.md
handler: src/main.go（crontabPlugin.delete）
reviewed_at: 2026-11-10
status: verified
---

# crontab_delete 审查

> 2026-11-08 首审；2026-11-10 复核并修复返回语义、失败回滚与文档。

## 1. 一句话结论

可用。删除定义、保留 runs 历史、`remaining` 明确剩余总数、`running` 提示在飞执行；落盘失败回滚内存；未知参数与空 id 明确报错。剩余是非幂等重试与历史/子会话清理口径。

## 2. 调用链

1. LLM `crontab_delete` → gateway 同步转发插件。
2. `crontabPlugin.delete`：
   - `rejectUnknownArgs`（只允许 `task_id`）→ TrimSpace 非空 →
   - `Store.Delete`（锁内移除 + `persistLocked`；失败恢复被删项）→ `notifyChanged` → 返回。
3. UI 的 `delete` RPC 复用 `t.delete`（只带 `task_id`）。
4. 错误统一为 `isError=true` + `{"ok":false,"error":"…"}`。

## 3. 契约面

- YAML：`async: never`（→sync）、`idempotent: false`（任务不存在仍报错）。
- schema：`task_id` required，`additionalProperties:false`。
- 摘要：明确 `remaining` = 删除后总数、`count` 是旧别名、`running:true` 表示命令仍在后台、任务不存在是错误、失败后先 `list` 确认、不删历史 runs/子会话。
- 返回：`{ok,task_id,deleted,remaining,count,running}`；失败 JSON。

## 4. 实现面

- `task_id` TrimSpace 后非空；不存在 → `ErrTaskNotFound`。
- `Store.Delete`：查找、构造新切片、`persistLocked`；失败时恢复原切片（`s.data.Tasks = old`），不会出现“报失败但当前进程已看不到、重启复活”。
- 返回 `remaining = len(store.List())`；`count` 保留同值别名；`running = deletedTask.RunningSince > 0`。
- 不中止在飞执行：调度 goroutine 已持有 `Task` 快照；删除后其收口 `store.Update` 返回 `ErrTaskNotFound`（忽略），命令仍跑完并 `AppendRun` 写 runs 历史。
- 不清理 `runs.jsonl`，也不清理宿主侧的 `crontab-<task_id>` 子会话。
- 副作用：重写 `tasks.json`、推 UI 事件。无资源分配。
- 非幂等：重复删除/错误 id 返回 `任务不存在`；契约要求先 `list` 确认。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 成功删除 | false | `{ok:true,task_id,deleted:true,remaining:N,count:N,running:false}` |
| 删除时仍在执行 | false | 同上，`running:true`；命令继续跑完 |
| 最后一个任务 | false | `remaining:0,count:0` |
| `task_id` 为空 / 缺失 / 未知参数 | true | `{"ok":false,"error":"…"}` |
| 任务不存在（含重复删除） | true | `{"ok":false,"error":"任务不存在"}` |
| 落盘失败 | true | JSON error，内存已回滚，任务仍存在 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 非幂等 | 重试/错误 id 返回“任务不存在”；契约已要求先 `list`。若希望删除幂等，可改为 `ok:true,deleted:false`（产品取舍）。 |
| P2 | 不清理历史 runs 与任务子会话 | runs 保留供排查，但 LLM 没有工具可读；子会话成为孤儿。可后续加 `delete_runs` 或会话清理。 |
| P2 | 无批量删除 | 多任务需 N 次调用；可后续加 `task_ids`。 |
| P3 | 删除不中止在飞执行 | 已返回 `running` 并写入契约；若产品要求“立即停”，需 scheduler 取消信号。 |
| P3 | `count` 别名保留 | 为兼容旧解析保留，已文档化为 `remaining` 同值。 |

已修（2026-11-10）：`count` 歧义、缺少 `deleted/remaining/running`、落盘失败内存已删、失败返回形态、unknown args 静默忽略、契约缺失败面/`additionalProperties`、顶部注释旧工具名（enable/disable→toggle）。

## 7. 测试/验收

- `TestDeleteReturnsRemainingAndRunning`：删除运行中的任务 → `deleted:true`、`remaining/count` 正确、`running:true`、其它任务保留。
- `TestDeleteMissingAndInvalidArgs`：不存在、空 id、缺 id、未知参数均报错。
- `TestStoreDeleteRollsBackOnPersistFailure`：落盘失败后任务仍在内存。
- 回归：crontab 插件 `go test -count=1 ./...` 全量通过。