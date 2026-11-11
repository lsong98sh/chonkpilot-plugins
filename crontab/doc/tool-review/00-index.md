# crontab 工具审查索引

审查对象：`E:/BizWorks/chonkpilot-plugins/crontab`（插件源码）＋ 宿主侧只读核对（contract/gateway/manifest）。
审查日期：2026-11-08（2026-11-10 复核；enable/disable 已合并为 toggle）。严重级别定义见 `E:/BizWorks/chonkpilot-plugins/工具审查.md:216-220`。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| crontab_add | 可用 / 有风险 | 0 | 6 | 6 | [crontab_add.md](crontab_add.md) |
| crontab_update | 有风险 | 0 | 3 | 6 | [crontab_update.md](crontab_update.md) |
| crontab_list | 有风险 | 0 | 3 | 5 | [crontab_list.md](crontab_list.md) |
| crontab_toggle（enable+disable 合并） | 有风险 | 0 | 4 | 10 | [crontab_toggle.md](crontab_toggle.md) |
| crontab_delete | 有风险 | 0 | 2 | 5 | [crontab_delete.md](crontab_delete.md) |
| **合计** | — | **0** | **18** | **32** | — |

> 计数按「每个工具文档的问题行」统计；同一根因（如错误返回形态）会在多个工具重复计入，见下方「跨工具根因」。本次未发现满足 P0 定义（必然失败/误用、安全或数据损坏、返回不可解析、沙箱绕过）的问题。

> **2026-11-10 修复说明（crontab_add）**：失败统一为 `isError=true` + `{"ok":false,"error"}` 文本；`run/agent` 创建/改动作前校验 `work_dir` 非空、存在、命中白名单（`notify` 允许无项目，调度器不再对 notify 做 `work_dir_missing`）；`Store.AddLimited` 锁内上限 + 落盘失败回滚（`Delete` 同步回滚）；创建 toast 改异步；`action`/`param`/顶层未知字段显式拒绝；`@yearly/@annually/@midnight`、`sandbox:"inherit"`、`display_name` 补入契约；返回补 `timezone`/`next_run_at_local`/`run_at_local`。上表 P1/P2 计数仍为 2026-11-08 审查基线，未随修复回改。

> **2026-11-10 修复说明（crontab_update）**：`cron/at/delay` 显式空串改为报错（null 仍=省略），非字符串类型不再被静默忽略；成功返回增加 `changed`（空数组 = no-op）；仅改 `action`/`sandbox` 不再推进 `next_run_at`（避免丢掉已到期待认领触发），停用任务保持 0；`Store.UpdateTx` 在 mutate/落盘失败时回滚内存；改 `run/agent` 时校验复用任务的 `work_dir`。

> **2026-11-10 修复说明（crontab_list）**：新增 `task_id` 精确查询、`order`（默认 desc）、`offset` 翻页；`enabled/limit/offset/output_tail_bytes/order` 类型或范围错误改为显式报错；返回 `count/total/truncated/offset/order/output_tail_bytes`，并随 `taskView` 提供 `timezone/next_run_at_local/run_at_local`；契约补 `last_run` 字段与跳过事件语义。

> **2026-11-10 合并说明（crontab_enable / crontab_disable → crontab_toggle）**：两个方向合并为 `crontab_toggle{task_id,enabled}`；启用保持已有 `next_run_at` 实现真幂等、校验 `work_dir`、拒绝过期一次性任务；停用清 next、保留定义、不中止在飞执行；共用 `Store.UpdateTx` 回滚与 `changed` 返回。上表 toggle 行计数为原 enable/disable 两行之和（审查时基线）。

> **2026-11-10 修复说明（crontab_delete）**：返回改为 `{deleted,remaining,count,running}`（`remaining`=删除后总数，`count` 为旧别名）；落盘失败恢复被删项；未知参数/空 id 明确报错；契约补失败面、`additionalProperties:false`、在飞执行与历史/子会话保留说明。

> **2026-11-10 在飞执行口径**：delete / update / toggle 均允许作用于 `running=true` 的任务，但都只影响后续触发、不取消当前执行；`taskView.running` 统一暴露 `RunningSince>0`。旧一次性执行收口仅在当前定义仍是同一次 `once+run_at` 时自动停用，避免覆盖运行期间改期/改成 cron 的新定义。

## 覆盖范围

- 5 个工具全部覆盖：`crontab_add / update / list / toggle / delete`（enable/disable 于 2026-11-10 合并为 toggle）。
- 契约：5 个 `.tool.md` 齐全（`src/tools/crontab/`），均通过 `loadContracts` + `registerTools` 注册；`registerTools` 对「契约缺少实现」会 `os.Exit(1)`。
- manifest：hot_tools = 5（`src/initdata/capability/plugins/crontab/manifest.json`），`timeout_sec: 60`，`config.scope=plugin`。

## 范围差异（与分配说明不符处）

1. **无 `src/lib` 目录**：handler 全部在 `src/main.go`，辅助逻辑为同目录平铺文件（`action.go / schedule.go / store.go / run.go / sandbox.go / workdir.go / session.go / config.go`）。
2. **执行侧不在宿主调度器**：定时触发由插件自身完成——`main.go:128` 启动 `scheduler`，`schedule.go:74-94` 起 1s ticker 单 goroutine 扫描，任务用 goroutine 执行（`schedule.go:220-224`）。宿主只提供三个能力面：`plugin/v1/workdir.ensure`（`workdir.go:20`）、`plugin/v1/sandbox.resolve`（`sandbox.go:26`）、`plugin/v1/session.*`（`session.go:33-39`）。因此 misfire/overlap/auto-disable 的根因在插件内，不在 llm-server。
3. **契约注册不是 `contract.Register`**：走 SDK 的 `p.Tool`（`main.go:181`）；`contract.Parse` 只解析 `name/title/hot`（`contract.go:73-79`），YAML 里的 `async/timeout/idempotent/category/runtime/output` **一律不参与运行时**。
4. **运行时的 async/timeout 由 gateway 决定**：本插件工具 `_meta` 只有 `hot` → 默认 `mode=auto`；超时取 server 级 `manifest.timeout_sec=60`（`E:/BizWorks/chonkpilot-agent/gateway/src/mcpgateway.go:889-958`）。契约里的 `async: never`、`timeout: 30` 是装饰性的。

## 跨工具根因（在多个工具重复计分）

| 根因 | 证据 | 影响面 |
|---|---|---|
| **失败一律 `IsError=true` + 裸文本，从不返回契约承诺的 `{ok:false,error}`** | SDK `plugin/src/sdk/plugin.go:552-554`、`:624-626`；契约 add/update/delete | 5 个工具；`error`/`code` 字段不存在，错误不可结构化解析 |
| **插件侧契约元数据不生效** | `plugin/src/contract/contract.go:73-79`（只读 name/title/hot）、`main.go:181` | `async/timeout/idempotent` 全部是文档；实际 auto + 60s |
| **`Store` 的「先改内存、后落盘」在 persist 失败时造成内存/磁盘分歧** | `store.go:169-178`（Add）、`:191-195`（Update）、`:206-210`（Delete） | add/update/enable/disable/delete：报告失败但状态已变，或反之 |
| **无任务级时区**：`TZ=`/`CRON_TZ=` 前缀被「必须 5 段」检查拒绝，调度按进程本地时区 | `schedule.go:18`、`:21-36`、`:39-49`；robfig `parser.go:95-110` | add/update 创建语义、list 的 `next_run_at` 解读 |
| **misfire / 重入跳过不进 `Task.LastRun`**，`runs.jsonl` 无 LLM 工具可读 | `schedule.go:135-143`、`:212-215`；`main.go:409-416`（仅 UI 动作） | list/enable/delete 的诊断能力 |
| **单次返回无上限**（list 每条任务 ≤4KB `output_tail`，任务上限 200） | `main.go:322-333`、`run.go:14`/`:75`、`config.go:70` | list：token 爆量风险 |
| **`next_run_at` 只有 epoch 秒**，契约与返回均无时区/本地时间 | `main.go:553`、`schedule.go:39-49` | add/update/list/enable/disable 的可读性 |

## Top 3 问题（带证据）

1. **P1｜list 可把数百 KB 拉进上下文**：`crontab_list` 无 `limit`，每条任务无条件下挂 `last_run`，`last_run.output_tail` 截到 4KB（`main.go:322-333`、`:572-574`、`run.go:14`/`:75`），任务上限 200（`config.go:70`）→ 最坏约 800KB。契约示例完全没提示体积。
2. **P1｜add 可造出「创建成功但永不执行」的任务，且非幂等会因通知阻塞产生重复任务**：`work_dir` 不是入参（来自 `_meta`），空白名单放行空串（`main.go:606-619`），首次触发即 `work_dir_missing` 自动停用（`schedule.go:232-236`、`:304-311`）；同时 `store.Add` 先落盘、再**同步** PowerShell 通知（`main.go:243-252`、`notify_windows.go:89-96`），`notify-fallback=window` 时 MessageBox 会阻塞到用户点击（`notify_windows.go:57-63`），超过 gateway auto 的 60s 阈值后转后台/超时，模型重试即产生第二条同命令任务。
3. **P1｜misfire 与重入跳过对 LLM 完全不可见**：`Recover` 的 `skipped:missed` 与 `tick` 的 `skipped:overlap` 只写 `runs.jsonl`，不写 `Task.LastRun`（`schedule.go:135-143`、`:212-215`），而 `crontab_list` 的「最近一次结果」只读 `Task.LastRun`（`main.go:572-574`），`runs`/`detail` 仅 UI 动作（`main.go:409-459`）→ 模型看到的是更早的成功记录。

## 其他观察（非工具面，仅记录）

- **产物漂移**：`src/chonkpilot-crontab-plugin.exe`（Oct 31，文件名与 manifest 不一致）与源码（Nov 3/4 更新）不同步；实际分发目录 `dist/plugins/crontab/` 使用 `chonkpilot-plugin-crontab.exe`，与 `manifest.runtime.exe` 一致。审查以源码为准，未改动任何产物。
- `main.go:120` 注册了 UI 动作 `detail`，但 `manifest.ui.rpc`（`:73`）未列出；宿主 `pluginActionHandler` 按路径直接转发、不校验该清单（`E:/BizWorks/chonkpilot-framework/host/src/gui/pluginaction.go:67-95`），因此当前不影响功能。[待核实] 是否有其它消费方读取 `ui.rpc`。
- 调度器 `execute` 的完成通知与 `disableForMissingWorkDir` 通知均为同步 PowerShell（`run.go:82-90`、`schedule.go:298-300`）；Windows 下若走 `window` 兜底会长时间占用该任务的执行 goroutine，间接放大 `overlap` 跳过。
- `RunRecord.ExitCode/MS` 都是 `omitempty`（`store.go:71-72`），成功且耗时为 0 时字段消失；契约示例按「字段恒在」呈现。