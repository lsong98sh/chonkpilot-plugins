# 工具调用目标架构与修改方案（v2，按 7 条要求校正）

> 依据：用户 7 条要求 + 2026-11-08 全量工具审查 + 三层一致性 review。
> 性质：方案文档，未改代码。证据沿用 `00-tool-invocation-common-review.md` 的 file:line。
> 路径别名：`FW`=chonkpilot-framework/plugin/src；`HOST`=chonkpilot/src；`GW`=chonkpilot-agent/gateway/src；`FE`=HOST/frontend/src；`PLUG`=chonkpilot-plugins。

## 0. 结论

7 条要求自洽，可以作为目标架构。落地前提只有一个：**把 `.tool.md` 变成工具策略的唯一来源**，并删除 llm-server 的强制 `async=always`。否则界面、md、gateway 仍会互相覆盖。

目标：

> mcp-server 只扫 `<exeDir>/capability`；插件 exe 内嵌 `.tool.md` 并由 SDK 全量解析；界面只做用户覆盖；gateway 按「界面 > md > 默认 auto 30s」裁决；stdio 沙箱开关真实生效；HTTP 仅在开启时下发 `_meta`；展示名统一为 `tool_call_display_name`，工具调用上下文 `purpose` 作废。

## 1. 七条确认（校正后）

| # | 确认 | 与现状的差距 |
|---|---|---|
| 1 | mcp-server 只扫 `<exeDir>/capability`，解析 tool/prompt/skill/resource 并注册 | 扫描范围不改；只需清理 `dist/capability/tools/server` 旧重复契约 |
| 2 | 插件 exe 嵌入 `.xxx.md`，代码注册工具 | `contract.Parse` 只读 name/title/hot，必须补全 async/timeout/on_timeout/category/idempotent/output |
| 3 | 界面按已注册工具配置同步/异步/timeout/超时动作 | 现有 auto/always/never/manual + threshold + hard_timeout + cancel_on_timeout 需收敛 |
| 4 | 不使用 `inProcessTool` 概念；**ask_user 必须异步、task_status 必须同步**，两者都是例外；task_status 按 LLM 传的 `timeout` 等待，到点返回半途中状态与内容；不引入 `waitSec` | 删除 `turn.go:383` 的 `inProcessTool` 及其分支；两个工具 md 均 `policy_lock: true` |
| 5 | 统一注入/下发 `tool_call_display_name`，不再出现 purpose 混淆 | Context.Purpose、`_meta.chonkpilot.purpose`、subjects.Purpose 全部改名；mcp_find `purpose`→`match` |
| 6 | gateway 按界面 > md > 默认裁决；默认 **auto 30s** | 删除 llm-server 强制 always；`manifest.timeout_sec` 退出执行链 |
| 7 | stdio 沙箱开关必须生效；HTTP 仅在开启时注入，未注入不做限制 | `buildConn` 无条件 Prepare 需改成按开关；HTTP 维持“仅开启注入” |
| 8 | YAML 可增加字段，不填用默认 | 新增 `on_timeout`、`policy_lock`；旧值做兼容映射 |
| 9 | `on_timeout` 的 fail/background 由 taskx 支持，不另起炉灶 | 复用 `timeout_arbiter.go` / `taskx.Verdict`，只扩展裁决映射 |

## 2. 目标策略模型

### 2.1 字段

YAML（`.tool.md`）新增/统一：

```yaml
async: auto | sync | async     # 兼容：never→sync, always→async, manual→auto
timeout: 30                    # 秒；0/-1 = 无超时；缺省 = 30
on_timeout: background | fail  # 缺省 background
policy_lock: true              # 可选；true = 界面不可覆盖（ask_user / task_status）
```

界面（usr `tool_policy`，替代 `tool_async`）：

```json
{"file_read": {"mode": "inherit|sync|async|auto", "timeout": null, "on_timeout": "inherit|fail|background"}}
```

解析后的有效策略（gateway 计算并回传）：

| 来源 | mode | timeout | on_timeout |
|---|---|---|---|
| 界面显式 | UI | UI | UI |
| 否则 md | md.async | md.timeout | md.on_timeout |
| 都没有 | **auto** | **30** | **background** |

### 2.2 三种 mode 的语义

| mode | 行为 |
|---|---|
| `sync` | gateway 阻塞等结果；到 `timeout` 按 `on_timeout` 失败或转后台；`timeout=0` 永久等待 |
| `async` | 立即受理，返回 pending；结果经 `mcp-tasks-report` 回喂；到 `timeout` 按 `on_timeout` 标记失败或继续跑 |
| `auto` | 等 `timeout`；未完成则按 `on_timeout`（默认转后台） |

- 默认 `auto / 30 / background`。
- **`background` 就是“转异步”**（同一动作，不新造词）。
- `on_timeout=background` → `taskx.Detach`；`on_timeout=fail` → `taskx.Fail`（交付超时错误）。全部走现有 `timeout_arbiter.go`，不新增计时器或异步通道。

### 2.3 例外工具（由 md 表达，不写代码分类）

| 工具 | md 策略 | 说明 |
|---|---|---|
| `ask_user` | `async: async`、`timeout: 0`、`policy_lock: true` | **必须异步**：调用立即受理并返回“等待回复”占位；答复以新用户消息到达，不回填本次工具结果；答复超时是它自己的 `timeout` 参数，归 ask 子系统 |
| `task_status` | `async: sync`、`timeout: 0`、`policy_lock: true` | **必须同步**：LLM 传 `timeout` 作为内部等待上限（**不引入 `waitSec`**）；到点返回 `state=running` + 已产出内容，`isError=false`；工具自身等待，不依赖 gateway 超时 |

- 删除 `inProcessTool` 概念与函数；不再有“进程内工具类”分支。
- `isAskTool` 仅保留 ask 结果落库/回填，不参与调度决策。

## 3. 展示名与 purpose

- 所有工具 schema 必须 required `tool_call_display_name`（≤20 字）；chat 实时、任务树、DB brief 三处统一。
- 工具调用上下文 `purpose` 作废：
  - `Context.Purpose` → `Context.DisplayName`。
  - `_meta.chonkpilot.purpose` → `_meta.chonkpilot.display_name`（或直接不下发，仅网关内部使用）。
  - `subjects.go` 的 `Purpose` 字段 → `DisplayName`。
- `mcp_find` 的 `purpose` 参数改名为 `match`（检索目的/匹配意图）；同步改 schema、描述、`memory_guide.go:162` 引导、测试。可保留一个版本 `purpose` 别名并标 deprecated。
- `subchat_open` 的子会话标题/目标改用 `tool_call_display_name` 或显式 `goal`，不再复用调用上下文 purpose。

## 4. 沙箱

```text
stdio MCP：
  开关 ON  → sandbox.Prepare + EnsureLowIL(writable_dirs)
  开关 OFF → 不 Prepare、不打标
  未设置   → 默认 ON

HTTP MCP（含插件）：
  开关 ON  → 下发 _meta.chonkpilot.sandbox = {enabled:true, writable_dirs:[当前实例]}
  开关 OFF / 未设置 → 不下发；插件不做限制
```

- `buildConn` 必须读 `sandboxOn(entry)`，修掉当前“无条件 Low IL”。
- `writable_dirs` 只按当前 `work_dir`/实例计算，不再做在线实例并集。
- 插件侧：file/task/web/codegraph/browser/desktop 只在收到 `_meta` 且 `enabled=true` 时按 dirs 校验；没有 meta 不限制。
- UI 沙箱页只保留 stdio MCP 条目，默认开；删除 executor 行与旧文案。

## 5. 分组件修改清单

### 5.1 mcp-server（扫描/注册）

| 改动 | 文件 | 说明 |
|---|---|---|
| 扫描范围 | `HOST/lib/mcp-server/server/contractsource.go` | 保持只扫 `<exeDir>/capability`，不改 |
| `_meta` 全量 | `contract.go:203-266` | 增加 `on_timeout`、`source`；false/0 也要可表达 |
| 旧契约清理 | `dist/capability/tools/server/*` | 删除与 task 插件重复的 ask_user/next_run/subchat_*/tool_result |

### 5.2 plugin framework / SDK

| 改动 | 文件 | 说明 |
|---|---|---|
| 全量解析 | `FW/contract/contract.go:63-105` | 解析 async/timeout/on_timeout/category/idempotent/output/policy_lock |
| `_meta` 写入 | 同上 `Register` | 写全量字段 + `source=md`；不再只写 hot |
| `# 摘要` | `contract.go:174-193` | 改为全文（或与宿主 contractmd 统一）；去掉 `title：` 前缀 |
| HotTool | `FW/sdk/plugin.go:96-102,255-264,558` | 删除“注册即 hot”，按 md hot/HotTool 标记 |
| Tool API | `FW/sdk/plugin.go:247` | 支持传 meta，供程序化工具注册 |

### 5.3 插件仓

| 插件 | 改动 |
|---|---|
| file / task / crontab | 25 个 md 补 `on_timeout`；ask_user 改 `async: async + timeout: 0 + policy_lock`；task_status 改 `async: sync + timeout: 0 + policy_lock`，参数 `waitSec`→`timeout`（**不保留别名**）；其余按实现修正 |
| codegraph（9） | 补 md；workdir 改 `_meta.chonkpilot.work_dir`；9 个工具统一 async/timeout/on_timeout |
| web（3） | 补 md；修 SSRF/maxBytes/4xx |
| mcp（3） | 补 md；`mcp_find.purpose`→`match`；`mcp_invoke` 同步结果/禁递归 |
| browser/desktop（2） | 补 md；screenshot 接 sandbox meta；目标窗口进 schema |
| demo-python（1） | `demo_echo` 补 md 或对齐 required/返回 |

### 5.4 gateway

| 改动 | 文件 | 说明 |
|---|---|---|
| 删除强制 always | `HOST/lib/llm/server/turn.go:342-389` | `gatewayArgs` 不再注入调用级 async |
| 策略解析 | `GW/mcpgateway.go:888-959` | 界面 > md `_meta` > 默认 auto/30/background；回传 source |
| 超时裁决 | `GW/mcpgateway.go:1088-1160` + `timeout_arbiter.go` | fail→`taskx.Fail`，background→`taskx.Detach`；复用现有外壳 |
| manifest timeout | `GW/mcpgateway.go:939-955` | 执行链移除 `entry.TimeoutSec`；如保留仅作连接超时并改名 |
| 展示名 | `GW/registry.go:29-67,265` | 保留注入；内部字段改 DisplayName |
| 沙箱 | `GW/provider.go:646-699,1212-1294` | stdio 按开关 Prepare；HTTP 仅开启注入 |
| 显式异步触发 | `GW/mcpgateway.go` doCall | 仅在 policy.mode=async 或 on_timeout 触发时进入异步；不默认 always |

### 5.5 llm-server

| 改动 | 文件 | 说明 |
|---|---|---|
| 删除 inProcessTool | `HOST/lib/llm/server/turn.go:331-389` | 函数与所有分支删除；所有工具走同一策略 |
| 展示名注入 | `server.go:282-295`、`turn.go` | 工具面组装层统一注入，覆盖所有工具 |
| purpose 清理 | `turn.go`、`tasks.go`、`subjects.go` | Context/Payload/事件字段改 DisplayName |
| ask 落库 | `turn.go:1486-1621` | 保留 ask_id/占位回填；不参与调度分类 |
| mcp_find 引导 | `memory_guide.go:162` | purpose→match |

### 5.6 前端

| 改动 | 文件 | 说明 |
|---|---|---|
| 新策略页 | `FE/views/config/SettingsToolAsyncPage.vue` | 模式（继承/同步/异步/自动）+ timeout + on_timeout；显示生效值与来源 |
| 迁移 | 同页 + composable | `tool_async`→`tool_policy`：never→sync、always→async、auto/manual→auto、cancel_on_timeout→fail、hard_timeout→timeout（仅 executor） |
| 例外灰置 | 同页 | `policy_lock` 工具显示“固定策略”，不可编辑 |
| 沙箱页 | `SettingsToolSandboxPage.vue`、`useToolSandbox.js` | 只留 stdio MCP，默认开；删除 executor 行与“未设置=不隔离” |
| display name | 任务树/chat | 统一读 `tool_call_display_name`，删除 purpose 回退 |
| chat tool-call 状态图标 | `FE/views/chat/MessageItem.vue`、`FE/composables/useTaskStatus.js` | 执行中/转异步/等待裁决/完成/失败/终止/中断统一用图标（复用 `TASK_STATE_META`：loading/arrow-right/warning-filled/circle-check-filled/circle-close-filled/refresh/clock）+ title；文本 badge 可保留作 a11y |

### 5.7 manifest / 构建

- `mcp.timeout_sec` 退出执行链；若保留，改名为 `connect_timeout_sec`。
- `hot_tools` 由 md 汇总生成，或构建时校验与 md 一致。
- 构建校验 `dist/plugins/*/tools`、`build/plugin-apps/*` 与源码逐字节一致。
- 现场 dist 必须重建，否则旧 `purpose/ranges` 契约仍生效。

## 6. 任务重排（按依赖顺序）

| 任务 | 内容 | 依赖 | 验收 |
|---|---|---|---|
| **T0 语义冻结** | 定稿字段、默认 auto/30/background、旧值映射；写决策表测试骨架 | 无 | 纯函数测试覆盖 UI/md/default |
| **T1 SDK 全量契约** | contract.Parse 全字段；Register 写 `_meta`；HotTool 修正；描述全文 | T0 | 单插件 `tools/list._meta` 与 md 一致 |
| **T2 程序化工具补 md** | codegraph 9、web 3、mcp 3、browser/desktop 2、demo 1 | T1 | 18 个工具全部有 md，schema/策略来自 md |
| **T3 gateway 策略与 taskx** | 删强制 always；resolver；默认 auto30；fail/background→taskx；manifest 退出 | T1 | 假时钟测试：sync/async/auto/fail/background |
| **T4 llm-server 清理** | 删 inProcessTool；display name 注入；purpose→DisplayName；mcp_find match | T3 | ask_user/task_status/display name 回归 |
| **T5 前端策略与沙箱** | 三模式+timeout+action；生效值/来源；旧键迁移；沙箱开关语义；chat tool-call 状态图标（执行中/转异步/等待/完成/失败/终止） | T3 | UI 保存后 gateway 实际决策一致；图标与任务树同源 |
| **T6 插件行为修复** | file/task/crontab md 对齐；codegraph workdir；web SSRF；mcp_invoke；browser/desktop sandbox | T3/T4 | 各插件 P0 用例 |
| **T7 产物与构建** | 清旧契约、构建校验、重建 dist | T2/T6 | 部署包契约与源码一致 |
| **T8 e2e** | 决策矩阵、ask_user、task_status 半途、stdio/http 沙箱、三处展示名 | 全部 | 端到端通过 |

建议先做 **T0+T1**（契约与 SDK），再做 **T3**（gateway 裁决）；T2 可以在 T1 后并行。

## 7. Breaking changes

1. 默认从 auto/无阈值改为 **auto/30/background**：30s 后转后台的行为明确化。
2. 删除 llm-server 强制 always：原“立即受理”的工具会按 md/UI 进入 sync/auto，长任务需显式配 async。
3. `manifest.timeout_sec` 退出执行链：现网依赖 300/600/60 的隐含超时需在 md/UI 显式配置。
4. **task_status 改为始终异步**，参数 `waitSec`→`timeout`（不保留别名）：旧提示词/调用会失效，结果经异步回执返回。
5. `purpose` 全链改名：Context、`_meta`、事件、测试、subchat_open 取值。
6. `mcp_find.purpose`→`match`：旧调用需一个版本别名。
7. stdio 沙箱开关生效：关闭后不再 Low IL；开启时按 dirs 打标，未配置的 MCP 可能写失败。
8. `tool_async`→`tool_policy`：需要迁移与回滚。
9. 程序化工具 schema 改由 md 提供：代码内硬编码 schema 需逐个对齐。

## 8. 已确认，无待决项

- 例外工具：`ask_user`（必须异步）、`task_status`（必须同步、`timeout` 参数、到点返回半途状态）；两者 `policy_lock: true`。
- 不引入 `waitSec`；`task_status` 参数直接用 `timeout`。
- `background` = 转异步。
- 默认策略：`auto / 30s / background`。

下一步从 **T0 + T1** 开始：先冻结字段与决策表，再改 SDK 契约解析；不改业务 handler。

## 9. 实施进度（2026-11-08）

已完成并通过测试：

- **T0/T1（SDK）**：`contract.Parse` 全量解析 async/timeout/on_timeout/policy_lock/category/idempotent/output；`MetaFor` 写入 `_meta`（含 `source=md` 与 `*_set`）；`ToolWithMeta` 注册入口；`HotTool` 不再“注册即 hot”；摘要改为全文。`contract` 包新增表驱动测试。
- **例外工具**：`ask_user` → `async: async + policy_lock`；`task_status` → `async: sync + timeout: 0 + policy_lock`，参数 `waitSec`→`timeout`，handler 到点返回 `state=running` + 部分输出。
- **T3（gateway）**：`source=md` 走新策略（md > 全局 > 界面 > 调用级；policy_lock 锁死）；默认 `auto/30s/background`；`on_timeout=fail` → `taskx.Fail`（取消 + 超时错误），`background` → `taskx.Detach`；`manifest.timeout_sec` 退出新链路。非 md 工具保留旧运行时语义（兼容）。
- **T4（llm-server）**：删除 `gatewayArgs` / `inProcessTool` / `arbitratedTool`；派发层不再注入 `async`；工具调用上下文与 `_meta` 统一为 `display_name`；`mcp_find` 引导改用 `match`。
- **T5（前端）**：异步页改为 sync/async/auto + 超时动作（fail/background）+ `policy_lock` 灰置；沙箱页 stdio 默认开、HTTP 仅开启注入；chat tool-call 状态图标复用 `TASK_STATE_META`（执行中/转异步/等待/完成/失败/终止/中断）。
- **沙箱**：gateway stdio 按开关决定 `Prepare`/Low IL（未配置默认开）；HTTP 维持“仅开启注入”。

已跑测试：

- `chonkpilot-framework/plugin/src`：全部包通过。
- `chonkpilot-agent/gateway/src`：`go test ./...` 通过（含新增 policy 表驱动测试）。
- `chonkpilot/src/lib/llm/server`：通过。
- `chonkpilot/src/lib/mcp-server`：通过。
- `chonkpilot-plugins/{file,task,crontab}`：通过。
- `chonkpilot/src/frontend`：821 项全部通过。

后续批次已完成（2026-11-08 第二批）：

- **T2**：codegraph 9、web 3、mcp 3、browser 1、desktop 1、demo 1 均补 `.tool.md`（exe/包嵌入 + 代码注册，`_meta.source=md`）。
- **T6 P0**：
  - codegraph：9 工具 workdir 改 `_meta.chonkpilot.work_dir`（可选显式参数需绝对/不越界）；查询走只读路径，不再 `MkdirAll`/Reconcile 写索引。
  - web：SSRF/内网默认拒绝（可配 `allow-private`）、重定向逐跳校验、禁 Host 覆盖、maxBytes 硬上限、4xx/5xx 置错、8MiB 明确报错、charset 解码。
  - mcp：`mcp_invoke` 带 `request_id` 同步返回真实结果、透传上下文、禁自调用、arguments 校验；`mcp_load` 带 instance_id；`mcp_find` 加 `limit/total/truncated` 与 `match`。
  - browser：eval/script 别名、screenshot 过沙箱、enginehost context 归属校验、timeoutMs 上限。
  - desktop：无目标截图改整屏/报错 + recover、目标窗口进 schema、move/size 标志修正、screenshot 过沙箱、list_windows 补 class/visible。
  - task：task_run cwd 限 work_dir 子树、requires 安装到项目内 deps、task_llm 按 waitSec 放宽桥超时、task_stop 同步应答 + cascade、task_status limit/total/truncated、subchat 参与者校验与 kind 守卫。
  - file：file_patch 全 hunk 校验/CRLF、file_diff 标准 unified diff + context、file_find 目录/tree/root 校验；file/crontab 11+6 个 md 补 `on_timeout` 并断言 `_meta`。
- **T7**：`build-plugins.ps1` 重建 11 个插件并部署 `dist/plugins`；`build-mcp-server.ps1` 重建 mcp-server；`build-desktop.ps1` 重建前端 dist 与 `dist/chonkpilot.exe`（含 gateway/llm-server/前端改动）；删除 `dist/capability/tools/server` 与 `dist/other/capability/tools/server`；file 契约与 dist 逐字一致；task manifest 已同步。
- **T8**：全量回归通过 —— framework 10 包、gateway、llm-server、mcp-server、file/task/crontab/web/mcp/browser/desktop/codegraph、scriptrun、frontend 821 项、demo py_compile。

仍保留的 P1/P2（不阻塞目标架构）：

- file 跨进程锁/expectMd5 TOCTOU、maxTargets 静默截断、file_diff 输出字节上限。
- codegraph 查询不再实时 Reconcile（索引新鲜度依赖后台事件）、部分返回无 truncated。
- mcp_find 仍检索全局注册表；prompts/resources 读取失败仍吞为 0 条。
- task_llm >600s 委派仍无 ack+异步两阶段；requires 安装进程仍以插件权限运行。
- browser/desktop 截图沙箱为插件侧预检，直接调引擎 UDS 可绕过；desktop context 归属未接 enginehost 新校验。
- crontab list 默认 50 条（行为变化，UI 列表不受影响）。
## 10. 增量：crontab 一次性调度 + 智能体动作（2026-11-08 第三批）

### 10.1 一次性调度（`once`）

- 契约：`crontab_add` / `crontab_update` 的调度参数改为三选一：`cron`（周期）、`delay`（相对时长，如 `50m` / `2h`）、`at`（绝对时间：RFC3339 / `2026-11-08 18:30` / unix 秒）。`cron` 与 `at/delay` 互斥，`at` 必须晚于当前时间。
- 数据：`Task` 新增 `once` / `run_at`（unix 秒）；周期任务 `once=false` 且 `cron` 非空，一次性任务 `once=true` 且 `cron` 为空。`next_run_at` 在一次性任务上等于 `run_at`。
- 调度：1s ticker 对一次性任务只认领一次（认领即清零 `next_run_at`），执行收口后自动停用（`disabled_reason=completed_once`）；启动恢复超出 `missed-grace-sec` → `once_missed` 终态停用，不补跑。显式改期会重新武装（`enabled=true`、清空终态原因）。
- 工具返回 / UI：`taskView` 增加 `schedule_type`、`once`、`run_at`；任务页以「一次性 · 时间」展示，直达详情可见停用原因。

### 10.2 智能体动作（`action.type=agent`）

- 契约：`{type:"agent", param:{prompt, agent?, title?, timeout_sec?, notify?}}`。`prompt` 必填；`timeout_sec` 0 = 插件 `default-timeout-sec`（默认 600s）；`notify` 省略 = true（完成通知 + 结果摘要）。
- 宿主能力：新增 `plugin/v1/agent.run`（`plugin_caps.go`）——**不依赖父轮次**（定时任务没有 turn），由能力确保会话存在、创建 turn、启动完整轮次（全工具面，可读项目文件），按 `timeout_sec` 等待收尾并返回 `state / text / turn_id`。实现复用 `startScheduledTurn`（`subllm.go`）：同一会话已有运行轮次 → `session_busy`，不覆盖。
- 插件执行：`runner.runAgent` 先确保「项目根会话 → 任务子会话」，再调用 `plugin/v1/agent.run`；结果正文写入 `RunRecord.OutputTail`，`session_id/dialog_id/turn_id` 回填任务，子会话可打开查看完整对话。宿主未提供能力 → 动作失败并留痕，不静默降级为无工具 LLM。
- 典型用法：`crontab_add` with `delay:"50m"` + `action:{type:"agent",param:{prompt:"进行代码审查：检查本次改动…"}}`；固定时间用 `at`。

### 10.3 测试

- crontab：调度解析（cron/delay/at/互斥/过期）、一次性恢复（未来/grace/错过）、一次性执行后自动停用、agent 动作解析/Canonical/`runAgent`（fake host + 通知）。
- llm-server：`go test ./...` 通过（新增能力编译 + 既有回归）。
