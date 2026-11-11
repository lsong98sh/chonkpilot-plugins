# 工具调用三层一致性 review：YAML 契约 / 界面设置 / gateway 实际处理

> 审查日期：2026-11-08。只读审查，未改源码/契约/产物。
> 范围：file / task / crontab 的 25 个 `.tool.md`；8 个工具设置面；gateway、llm-server、mcp-server 的实际调用链。
> 证据均为源码 `file:line`；推断标 [推断]，未确认标 [待核实]。
> 路径别名：`PLUG`=E:/BizWorks/chonkpilot-plugins；`FW`=E:/BizWorks/chonkpilot-framework/plugin/src；`HOST`=E:/BizWorks/chonkpilot/src；`GW`=E:/BizWorks/chonkpilot-agent/gateway/src；`FE`=HOST/frontend/src。

## 1. 一句话结论

**三层没有形成单一事实源**：YAML 头对插件工具基本是“装饰性契约”（async/timeout/category/idempotent/runtime/args 都不进运行时）；界面上可设的 mode/threshold/hard_timeout/cancel_on_timeout 中有一部分没有消费者，还有一部分被 llm-server 的强制 `async=always` 覆盖；gateway 的真实行为既不等同于契约值，也不等同于 UI 显示值。最危险的不是“缺功能”，而是**UI 让用户以为已经配好的超时/沙箱，实际没有生效或语义相反**。

## 2. 三层数据流

```text
.tool.md
  ├─(A) 插件框架 contract.Parse ──> name/title/hot + # 摘要首段 + schema
  │                               └─> 插件 MCP tools/list（_meta 只有 hot）
  ├─(B) 宿主 mcp-server contractmd ─> 全量 meta：async/async-threshold/timeout/
  │                                  category/idempotent/output/runtime
  │                                  └─> _meta.*（仅“宿主自扫描契约”这条路径）
  └─(C) manifest mcp.* ────────────> hot_tools / timeout_sec / category / origin
                                      └─> gateway servers/register

tools/list
  ├─> UI：契约现值（_meta.async/async-threshold/timeout）+ 工具设置页
  │        └─> usr 键 tool_async / tool_sandbox ──> gateway 覆盖层
  └─> llm-server：hot 过滤 / 场景白名单
           └─> gatewayArgs：对多数工具强制调用级 async=always
                   └─> gateway doCall：async 决策 → 超时裁决 → 执行 → mcp-tasks-report
```

关键分流：

- **A 与 B 是两套解析器**。插件工具（file/task/crontab）走 A，只拿到 name/title/hot；B 虽能解析全量 meta，但只扫描 `<exeDir>/capability`，且 `contractsource.go` 明确跳过 `plugins/`（`HOST/lib/mcp-server/server/contractsource.go:163-209`、`HOST/lib/assembly/assemble.go:69-89`）。
- **C 是第二事实源**。manifest 的 `timeout_sec` 在 gateway 侧压过工具 `_meta.timeout`（`GW/mcpgateway.go:939-955`），`hot_tools` 决定 LLM 工具面（`GW/registry.go:257-264`）。
- **llm-server 是第三个改写点**。它对几乎所有非进程内、无 `_meta.timeout` 的工具注入调用级 `async=always`（`HOST/lib/llm/server/turn.go:342-389`），而 `always` 在 doCall 里立即返回 pending 且不再设超时点（`GW/mcpgateway.go:1028-1037`）。

## 3. 字段流转矩阵

| YAML 字段 | 插件解析 A | 宿主解析 B | 进 `_meta` | UI 可见/可设 | gateway 实际消费 | 生效结论 |
|---|---|---|---|---|---|---|
| `name` | 是 | 否（用文件名） | 否 | 工具名 | 路由 | 生效，但宿主忽略 YAML name |
| `title` | 是（描述前缀） | 是（`mcp.Tool.Title`） | 否 | 工具名旁 | tools/list 不带 title | 半生效，gateway 后丢失 |
| `args` | 否 | 是 | 否 | metaForm 可编辑 | 仅宿主 executor spawn | **插件工具全失效** |
| `async` | **否** | 是 | 插件侧否；宿主非 auto 才写 | 异步页 mode | doCall async 决策 | **插件工具全失效**，落 auto 后被 llm-server 改 always |
| `async-threshold` | **否** | 是 | 仅宿主 >0 写 | 异步页 threshold | auto 的超时转后台点 | 25 个契约均未声明；UI 写 usr 覆盖 |
| `category` | **否** | 是 | 插件侧否；manifest → `_meta.server.category` | 分组/筛选 | UI 分组、LLM 过滤 | 契约 category 失效，实际用 manifest |
| `hot` | 是 | 是 | 是（true 才写） | MCP 对话框勾选 | LLM 工具面 | 结果对、机制错（SDK 无条件全 hot） |
| `runtime` | **否** | 是 | 否 | metaForm 可编辑 | 宿主分派 | **插件工具全失效**；`builtin` 写法误导 |
| `idempotent` | **否** | 是 | 宿主 true 才写 | metaForm 可编辑 | **无消费者** | 全失效 |
| `output` | **否** | 是 | 否 | metaForm 可编辑 | 仅宿主 stdout/code/file | 插件工具全失效；`json` 非合法枚举 |
| `timeout` | **否** | 是 | 插件侧否 | 异步页“超时”列显示的是 hard_timeout | gateway 裁决 + executor 硬杀 | **插件工具全失效**，实际取 manifest `timeout_sec` |
| schema/`# 参数` | 是 | 是 | 否 | — | LLM inputSchema | 生效但运行时零校验；gateway 另注入 required `tool_call_display_name` |
| `# 摘要` | 只取首段 + `title：` 前缀（`FW/contract/contract.go:174-193`） | 取整段（`contractmd.go:246`） | 否 | — | LLM description | **20/25 多段契约在插件侧被截断**，最多丢 89% |
| `# 返回值` | 否 | 否 | 否 | — | 无 | 纯文档，零消费 |
| （无 sandbox 字段） | — | — | — | 沙箱页 | `_meta.chonkpilot.sandbox` | YAML 无法表达工具级沙箱 |

## 4. 优先级与默认值（真实生效链）

| 项 | 低 → 高优先级 | 实际结论 |
|---|---|---|
| async | 契约 `_meta.async` → usr `tool_async.mode` → `Params.AsyncMode`（CLI） → 调用级 `args.async` | **llm-server 把调用级设成 always**，前两层对 LLM 调用全部被压掉（`turn.go:342-389`）；仅非 LLM 直调 gateway 时才按前三层 |
| threshold | 契约 `async-threshold` → 有效 timeout → usr `threshold` | 仅 `auto` 生效；usr 显式 0/-1 = 无阈值，但不会清除 `_meta`，UI 仍显示契约值 |
| hard_timeout | prj `timeout_sec` → 契约 `timeout` → usr `hard_timeout` | **只在宿主 executor 生效**（`HOST/lib/mcp-server/server/executor.go:199-213`）；插件/第三方工具无 executor，完全无效 |
| cancel_on_timeout | 无契约来源 → usr | 仅 manual/never 的超时分支消费（`GW/mcpgateway.go:1118-1136`）；auto/always 写入不生效 |
| timeout（裁决） | 全局 60s → `_meta.timeout` → server entry `timeout_sec` → 调用级 | gateway 与 executor 两条链顺序相反：gateway 是 “server > 契约”，executor 是 “契约 > prj”（见 P1-3） |
| sandbox | — | stdio 下游**无条件 Low IL**（`GW/provider.go:687-699`）；`tool_sandbox.mcps` 只决定是否注入 `_meta.chonkpilot.sandbox`（`GW/provider.go:1261-1294`），不控制进程隔离；`tool_sandbox.executors` 无消费者 |
| hot | 契约 hot / manifest hot_tools / SDK 自报 | SDK 把 `Tool()` 注册的全部工具加入 hot_tools（`FW/sdk/plugin.go:558`）；`HotTool` 的标记从未被读取；白名单非空时 hot 过滤整体绕过 |
| description | 插件首段 / 宿主全文 | LLM 看到的是插件首段（tools/list 由插件 MCP 返回），不是契约全文 |

## 5. 一致性问题

### P0（必然误用 / 安全预期反转）

| # | 问题 | 证据 |
|---|---|---|
| X-P0-1 | **插件工具的 YAML 头基本不生效**：async/timeout/category/idempotent/output/runtime/args 被 `contract.Parse` 静默丢弃，`contract.Register` 只写 `_meta.hot` | `FW/contract/contract.go:23-29,72-80,101-105`；`PLUG/file/src/main.go:60`；`PLUG/task/src/main.go:160`；`PLUG/crontab/src/main.go:181` |
| X-P0-2 | **llm-server 强制 `async=always`，使 UI 的 mode/threshold/cancel_on_timeout 与 gateway 超时裁决对插件工具全部失效**：`always` 立即返回 pending，不再设任何超时点 | `HOST/lib/llm/server/turn.go:342-389`；`GW/mcpgateway.go:1028-1037` |
| X-P0-3 | **沙箱页语义与实现相反**：UI 写“未设置=不隔离”，但所有 stdio MCP 子进程无条件下发 Low IL；开关只影响 builtin/inject 的 `_meta`，user MCP 连 `_meta` 都不注入 | `FE/composables/useToolSandbox.js:41,95-126`；`GW/provider.go:585,687-699,1212-1262` |
| X-P0-4 | **产物契约漂移**：`dist/plugins/file/tools/file` 仍是旧版（required `purpose`、含已删除的 `ranges`），源码已移除；现场若运行该 dist 产物，LLM 会按旧 schema 调用 | `PLUG/file/src/tools/file/file_read.tool.md` vs `HOST/dist/plugins/file/tools/file/file_read.tool.md`；`HOST/build/plugin-apps/file/tools/file/file_read.tool.md` |

### P1（错误不可恢复 / 配置空转 / 优先级矛盾）

| # | 问题 | 证据 |
|---|---|---|
| X-P1-1 | `hard_timeout` 对插件/第三方工具可编辑并写库，但无 executor 消费；gateway 转发时还会剔除该键 → 孤儿配置 | `FE/views/config/SettingsToolAsyncPage.vue:333-335,421`；`HOST/lib/llm/server/execconfig.go:172-186`；`executor.go:199-213` |
| X-P1-2 | 阈值框从不显示“未设置”，载入即填前端兜底 30；任意编辑会把 30 写入 usr，静默改变实际阈值（真实回落是 gateway 的 60s/契约值） | `FE/SettingsToolAsyncPage.vue:216,282-283,351`；`GW/mcpgateway.go:943,956-959` |
| X-P1-3 | 两套超时源顺序相反：gateway `server timeout_sec > _meta.timeout`，executor `hard_timeout > 契约 timeout > prj timeout_sec`；同一工具可能出现“gateway 判超时、executor 仍在跑”或相反 | `GW/mcpgateway.go:939-955`；`HOST/lib/mcp-server/server/executor.go:189-213` |
| X-P1-4 | `cancel_on_timeout` 对所有模式开放且提示不提限制，但 auto/always 不消费；用户以为“到点取消”实际是转后台/永不裁决 | `FE/SettingsToolAsyncPage.vue:337-338`；`GW/mcpgateway.go:1095,1118-1136` |
| X-P1-5 | `# 摘要` 插件侧只取首段并加 `title：` 前缀，宿主侧取全文；同一契约两套描述，LLM 看到的是截断版 | `FW/contract/contract.go:174-193`；`contractmd.go:246-279`；样本：subchat_open 130/1209 字、file_write 38/500 字 |
| X-P1-6 | hot 机制错误：SDK 无条件把全部注册工具加入 hot_tools，`HotTool` 标记无效；契约 `hot:false` 也会被暴露 | `FW/sdk/plugin.go:96-102,255-264,558`；`GW/registry.go:92-104` |
| X-P1-7 | `tool_sandbox.executors` 写入 mcp-server Config 但无人读取；沙箱页仍保留/文案仍宣称 executors 行（core/desktop/browser/dsl），实际已退场 | `FE/composables/useToolSandbox.js:38-41`；`HOST/lib/mcp-server/server/config.go:403-421`；`FE/locales/zh-CN/config.json:274-290` |
| X-P1-8 | MCP 对话框 `sandbox` 与 `tool_sandbox.mcps` 双写同一概念：dialog=true 仅在 mcps 未显式配置时并入；dialog=false 无法关掉已有 mcps=true，UI 无提示 | `FE/views/config/EditMCPDialog.vue:306-316,462-463`；`GW/mcpgateway.go:1723`；`GW/tooloverride.go:192-208` |
| X-P1-9 | `Params.AsyncMode`（CLI/全局）在 usr `tool_async.mode` 之后应用，优先级与注释相反；CLI 可静默压掉 UI 模式 | `GW/mcpgateway.go:899,904-919` |
| X-P1-10 | `touch_files` 缺省不一致：非 self 的只读工具（web_search/web_fetch）前端默认“涉及=打点”，gateway 默认 false → UI 徽标误导 | `FE/utils/toolTouchFiles.js:25-38`；`GW/tooloverride.go:239-262` |
| X-P1-11 | `_meta` 无法表达 false/未设置：hot/idempotent 只在 true 时透出，runtime/output 不进 tools-list → UI 只能把“键缺失”当缺省，契约现值不可判定 | `HOST/lib/mcp-server/server/contract.go:227-249` |
| X-P1-12 | 异步业务失败在 gateway 终态仍是 `state=done`，`mcp-tasks-report` 不携带 isError；llm-server 一律落 `completed`，UI 显示完成 | `GW/tasks.go:331-334`；`GW/mcpgateway.go:409-420`；`HOST/lib/llm/server/turn.go:1270-1281` |
| X-P1-13 | `_meta.chonkpilot.sandbox.writable_dirs` 是当前 workdir + 所有在线实例目录的并集（provider 回调忽略 workdir 参数），多实例时互相放宽 | `GW/provider.go:1282-1293`；`HOST/lib/llm/server/execconfig.go:279-318,339-348` |
| X-P1-14 | `tool_call_display_name`/purpose 对 HTTP 插件可达，但 spawn executor 的 env 未注入 PURPOSE/SESSION/TOOL_CALL_ID 等（注释与实现不符） | `GW/registry.go:32-63,265`；`GW/mcpgateway.go:831-840`；`HOST/lib/mcp-server/server/config.go:567-598`；对照注释 `server.go:37-48` |

### P2（文案/默认值/可发现性）

| # | 问题 | 证据 |
|---|---|---|
| X-P2-1 | `title` 到 gateway tools/list 即丢 | `GW/mcpgateway.go:605-630` |
| X-P2-2 | `_meta.idempotent` 全链路无消费者 | `HOST/lib/mcp-server/server/contract.go:250-254` |
| X-P2-3 | `# 返回值` 无任何代码消费 | 25 个契约；全仓无解析点 |
| X-P2-4 | `args/runtime/output` 对插件工具零消费；file 契约 `runtime: builtin` 与实际插件进程执行不符 | `HOST/lib/mcp-server/server/executor.go:61-74`；`PLUG/file/src/tools/file/*.tool.md` |
| X-P2-5 | 沙箱只有 executor/MCP 条目级，无工具级；YAML 也无 sandbox 字段 | `GW/tooloverride.go:74-96`；`FE/SettingsToolSandboxPage.vue:3-14` |
| X-P2-6 | 非 hot 工具按名调用无门；唯一门是本轮白名单 + `mcp_invoke` 目标检查 | `GW/mcpgateway.go:857`；`HOST/lib/llm/server/turn.go:1464-1481` |
| X-P2-7 | `tool_result` 不在 `inProcessTool` 白名单，经白名单暴露会被强制 always，返回 pending | `HOST/lib/llm/server/turn.go:383-389`；`HOST/dist/capability/tools/server/tool_result.tool.md:4-11` [待核实] |
| X-P2-8 | 旧域契约仍留在 `dist/capability/tools/server`（含已删除 tool_result），与 task 插件重复 | `HOST/build-mcp-server.ps1:119-122`；`HOST/dist/capability/tools/server/*` |
| X-P2-9 | `ToolDoc.Category` 注释称“缺省由目录名派生”，实现无派生 | `HOST/lib/mcp-server/server/contract.go:64,170` |
| X-P2-10 | `cancel_on_timeout` 失焦回落用 `savedMap[row.name]` 而非 `cfgKey`，历史 `self_<名>` 键回落丢失 | `FE/SettingsToolAsyncPage.vue:516` vs `:272-280` |

## 6. 典型链路验证

| 工具 | 契约声明 | UI 可设 | 真实链路 | 结果 |
|---|---|---|---|---|
| `file_read` | `async: never`、`timeout: 120` | mode/threshold/hard_timeout | 插件契约丢弃 → `_meta` 无 async/timeout → gateway 落 auto → llm-server 改 always → 无裁决点 | 契约的 never/120 完全不生效；UI 无法真正“同步返回” |
| `task_run` | `async: never`、`timeout: 600` | 同上 | 同上；manifest 600s 也只作为 server 级参数，always 下不设点 | 长任务永不判超时，只能靠用户/任务层 stop |
| `task_stop` | `async: never`、`timeout: 120` | 同上 | 同上；实际成功路径另有 15s/30s 应答超时问题（见 task_stop 审查） | UI 超时配置无效 |
| `crontab_add` | `async: never`、`timeout: 30` | 同上 | 契约丢弃；manifest 60s；llm-server always | 30s 与 60s 均不裁决 |
| `web_search` | 无 `.tool.md` | mode/threshold/hard_timeout | manifest 180s；llm-server always | 超时只在 gateway 内部逻辑，UI 的 hard_timeout 无效 |
| `browser_run` | 无 `.tool.md` | 同上 | manifest 180s；engine client 自带 120s | 两层超时；UI 配置不参与 |

## 7. 修复建议

### 7.1 契约层（先定“单源”）

1. **二选一收口**：要么让插件 `contract.Parse` 解析全量 meta 并写入 `_meta`，要么从 25 个 `.tool.md` 删除 `args/runtime/output/idempotent/category/timeout/async` 这些死字段，只保留 name/title/hot/schema/description。不要维持“写着但不生效”。
2. **manifest 由契约生成**：`hot_tools`/`timeout_sec`/`category` 从 `.tool.md` 汇总生成，消除 manifest 与契约双源；至少加一致性校验。
3. **描述统一**：插件侧改为取 `# 摘要` 全文（或明确约定只写一段），去掉 `title：` 前缀，与宿主 contractmd 一致。
4. **产物门禁**：构建后比对 `dist/plugins/*/tools`、`build/plugin-apps/*` 与源码契约；删除 `dist/capability/tools/server` 旧域契约。

### 7.2 UI 层（先把“显示值 = 生效值”）

1. 异步页的“契约现值”改为读 gateway 实际生效值（服务端计算后回传），不要前端自造 30 兜底；未设置就显示空/“默认”。
2. `hard_timeout` 对非 executor 工具禁用或标“不适用”；`cancel_on_timeout` 仅对 manual/never 开放并标明生效档。
3. 沙箱页重做语义：要么让 `tool_sandbox.mcps` 真正门控 spawn/Low IL，要么改成“仅 `_meta` 白名单策略”，删除 executor 行、`未设置=不隔离`、agentbox/L6 旧文案。
4. MCP 对话框 sandbox 与 `tool_sandbox.mcps` 合并为单源，明确 false 能否覆盖 true。
5. `touch_files` 补齐只读工具默认集合；修正 `savedMap[row.cfgKey]`。

### 7.3 gateway 层（先让超时/沙箱真实可感）

1. **不要对已有 usr mode/timeout 的工具强制 `always`**；至少让 usr `mode=never` 能抵抗调用级 always，并为 proxied 工具建立真正的超时裁决点。
2. `buildConn` 按 `sandboxOn(entry)` 决定是否 `sandbox.Prepare`；关闭时同时跳过 `ensureLowILWritableRoots`。
3. `_meta` 返回“实际生效值”，并把“裁决超时”与“执行硬上限”拆成两个字段；显式 0/-1 要能清除旧 `_meta`。
4. `mcp-tasks-report` 携带 isError/失败原因，不要让业务失败落成 completed。
5. 统一并文档化一条超时优先级链（调用级 > usr > server > 契约 > 全局），gateway 与 executor 共用。

### 7.4 需要拍板的 breaking change

- 停止强制 `async=always` 后，长工具会开始出现 awaiting/超时裁决，调用方与前端展示要同步。
- 沙箱开关真正生效后，现有“未设置也能写”的 MCP/插件可能失败。
- 契约删除死字段会改变 `tools-list` 的 `_meta` 形状；若保留则要补齐，二者都会影响 UI 与测试。
- manifest 由契约生成会改变 `hot_tools`/`timeout_sec` 的维护方式与构建链路。

## 8. 证据索引

- 插件契约解析：`FW/contract/contract.go:23-29,63-105,174-193`；`FW/sdk/plugin.go:96-102,247-264,544,558,586`。
- 宿主契约解析：`HOST/lib/mcp-server/server/contract.go:64,164-266`；`contractsource.go:163-209`；`HOST/lib/assembly/assemble.go:69-89,126-136`。
- gateway：`GW/registry.go:92-104,257-268`；`GW/mcpgateway.go:596-643,888-959,1027-1081,1088-1160,1245-1289,1367-1387,1588-1604,1723`；`GW/provider.go:585,687-699,1212-1294`；`GW/tooloverride.go:74-96,192-291`；`GW/tasks.go:331-334`。
- llm-server：`HOST/lib/llm/server/turn.go:342-389,459-463,1307-1318,1464-1481`；`server.go:282-295`；`execconfig.go:164-186,210-227,279-348`。
- UI：`FE/views/config/SettingsToolAsyncPage.vue:12,99-113,214-216,272-283,330-351,410-424,476-477,516`；`FE/views/config/SettingsToolSandboxPage.vue:3-14`；`FE/composables/useToolSandbox.js:38-41,95-126`；`FE/views/config/EditMCPDialog.vue:306-316,318-326,455-463`；`FE/utils/toolTouchFiles.js:25-38`；`FE/utils/metaForm.js:5,34-42`。

## 9. 待核实

- 现场运行的是 `dist/plugins/<plugin>` 产物还是 `build/plugin-apps` 产物，决定旧契约漂移是否已实际影响 LLM [待核实]。
- 外部 core-mcp-server 模式是否会下发 `tool_result` 等旧域契约 [待核实]。
- `mcp-server.Config.SetToolSandbox` 在 executor/execapi 是否真的无消费点（当前仅见定义与 config 存取）[推断]。
- gateway `tool_sandbox.mcps=false` 是否在所有 provider 分支都不注入 `_meta`（当前只见 builtin/inject 分支）[待核实]。