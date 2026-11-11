# task 工具审查索引

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| task_run | 有风险（沙箱两处绕过、提权误判、契约声称的 exe 用法失败） | 2 | 3 | 7 | [task_run.md](task_run.md) |
| task_llm | 不建议当前形态（默认等待被 SDK 15s 截断，子任务失控） | 1 | 2 | 5 | [task_llm.md](task_llm.md) |
| task_status | 有风险（`limit` 失效、树形状与契约不符） | 0 | 3 | 6 | [task_status.md](task_status.md) |
| task_stop | 不建议当前形态（成功停止被报为失败，取消仍生效） | 1 | 1 | 4 | [task_stop.md](task_stop.md) |
| ask_user | 可用但有风险（question 不校验、能力错误留 running、描述只有首段） | 0 | 3 | 5 | [ask_user.md](ask_user.md) |
| next_run | 可用但有风险（恢复轮恒报未启用；潜在幂等键丢 prompt） | 0 | 1 | 6 | [next_run.md](next_run.md) |
| subchat_open | 有风险（participants 静默丢弃并降级为单聊、参数不校验、首轮失败不回流） | 0 | 3 | 6 | [subchat_open.md](subchat_open.md) |
| subchat_finish | 有风险（无会话类型守卫，主会话也会被置 ready） | 0 | 1 | 5 | [subchat_finish.md](subchat_finish.md) |
| **合计** | — | **4** | **17** | **44** | — |

> 本索引覆盖 task 插件全部 8 个工具。`task_run`/`task_llm`/`task_status`/`task_stop` 由本次审查批次 A 产出；`ask_user`/`next_run`/`subchat_open`/`subchat_finish` 由并行批次 B 产出，本索引中的结论与本计数直接取自各自文档（未二次复核其证据）。

> **2026-11-10 修复说明（next_run / subchat_open / subchat_finish / ask_user / task_stop）**：`next_run` 恢复轮补齐 `nextRunMax` / agent 白名单 / deny 集 / 已登记 inbox 项回查，append 幂等命中后强制覆盖 content，成功文案含次数/上限，插件透传 `is_error`。`subchat_open` 补齐固定参数存在性校验（注册表/llms 可用时）、`mode` 返回、`is_error` 透传、prompt 上限与 schema/返回值。`subchat_finish` 非 `sub_chat` 工具面全部排除，插件透传 `is_error`，成功返回 session_id/lifecycle，契约补返回值。`ask_user` 的 P1 已修（契约 `async: async`→`sync`、摘要首段改非阻塞、补返回值/schema 上限/运行时校验、worker 识别 `isError`）；`task_stop` 的 P0/P1 已修（改走 `plugin/v1/task.stop`、`cascade` 生效），并补齐**命中 ask 任务节点时撤销提问**。同日按用户要求再修 `ask_user`：历史答案双份（tool 行只写标记）、投递失败答复落库、同会话答复逐条立即投递（不再等待全部作答）、选项空 value 归一/空对象拒绝（自由文本问答保留）。各文 §6/§7 已按现状更新；上表计数仍为审查时基线，未随修复回改。

> **2026-11-09 修复说明（task_run）**：`task_run.md` §10 记录了本轮修复，§6 表中两条 P0 已过时 —— `cwd` 自 2026-11-08 起不再进入沙箱可写白名单；`requires` 已改为项目内 `work_dir/.chonkpilot/deps` 并在 2026-11-09 完成安装沙箱化/可取消/计入 `ms`。同时修复 exit 5 提权误判、Windows `shell + file` 扩展名分派、`env` 大小写去重，并新增调用级 `maxOutputKB` 与超限完整输出落盘（`output_file`/`total_bytes`/`output_bytes`）。上表结论与 P0/P1/P2 计数仍为审查时基线，未随修复回改。

## 审查范围与依据

- 契约来源：插件内 embed 的 `src/tools/task/*.tool.md`。
- 插件源码根：`E:/BizWorks/chonkpilot-plugins/task/src`；宿主调用链：`E:/BizWorks/chonkpilot/src/lib/llm/server/*`；网关：`E:/BizWorks/chonkpilot-agent/gateway/src/*`（只读，仅用于核对落库/回执，不在其中找插件实现）。
- 运行时 manifest：`E:/BizWorks/chonkpilot/dist/plugins/task/manifest.json`（插件启动时从 exe 目录读取，`sdk/manifest.go:118-137`）；源码仓副本：`E:/BizWorks/chonkpilot/src/initdata/capability/plugins/task/manifest.json`。
- 审查为只读；未执行 build/test/插件进程，未修改源码、契约、manifest、产物。

## 跨工具的共性发现（影响全部 8 个工具）

1. **契约的 `async`/`timeout` 不下发 gateway，实际档位被宿主强制改写成 `always`**：插件契约经 `go:embed` 只产出 MCP schema；`plugin.mcp.register` 只上报 `{url,token,hot_tools}`（`E:/BizWorks/chonkpilot-framework/plugin/src/sdk/plugin.go:586-591`），gateway 从 `tools/list` 读不到工具级 `_meta.async/_meta.timeout`，只回落 server 级 `timeout_sec=600`（`E:/BizWorks/chonkpilot-framework/plugin/src/mgr/api.go:345-354`）。宿主 `turn.gatewayArgs` 对非“进程内/已声明超时裁决”工具注入 `async=always`（`E:/BizWorks/chonkpilot/src/lib/llm/server/turn.go:342-358`），该档位**不设超时点**（`E:/BizWorks/chonkpilot-agent/gateway/src/mcpgateway.go:1027-1037`）。因此契约里的 `async: never`、`timeout: 600/120` 均不成立，`mcp-tools-timeout` 的 wait/cancel 裁决面在正常链路不可达。—— 建议按插件平台级 P1 修正（注册时随 meta 下发，或插件显式自报）。
2. **插件→宿主能力调用的 15s 隐含上限**：`options.Timeout` 缺省 15s（`sdk/plugin.go:39,54,149-151`），`p.Call` 对每次桥调用套 `WithTimeout`（`sdk/plugin.go:316-331`、`protocol/frame.go:353-358`）。`task_llm` 因此无法兑现“等到收尾”（P0）；所有新增的长等待能力都会踩同一坑。
3. **异步回执吞错误详情（仅传输层级失败）**：gateway 侧 `err != nil`（provider 断开、传输错误、执行取消）时，`result_summary` 在 `turn.handleAsyncDone` 被替换为 `任务结束(error|cancelled)`（`turn.go:1278-1280`），LLM 只能靠 `task_status` 反查任务节点 `Error`；进程重启后不可恢复。注意：插件返回的 MCP `isError` 走另一条路径（`res.IsError` + `err=nil` → 任务 `done` + 原文），不会被压缩成该文案。
4. **isError 语义在异步链路被抹平**：插件返回的 MCP `isError` 在网关执行池仍记 `state=done`（`E:/BizWorks/chonkpilot-agent/gateway/src/tasks.go:306-345` 执行结束分支只看 `err`），LLM 侧不置 `isError`；模型唯一判据是正文 `ok`/错误文本。
5. **产物漂移（只记录，不修改）**：`dist/plugins/task/*` 与 `build/plugin-apps/task/*` 的 exe 为 2026-11-07 22:45（12,667,904 B），源码侧 exe 为 2026-11-08 10:13（12,738,560 B）；`dist/.../manifest.json`（2026-11-05）仍是 4 个 `hot_tools` 与旧描述，而 `src/initdata/.../manifest.json` 已含 8 个。按基线 7 记录为产物漂移。
6. **manifest 权限声明与强制面不匹配（文档级）**：`permissions.data` 声明的 `task.tree/task.tasks/task.stop` 由 `mgr.dataGuard` 强制；`plugin/v1/agent.delegate`、`plugin/v1/task.result`、`plugin/v1/ask.user` 等能力方法在桥侧无按插件权限校验，manifest 的 `llm`/`session` 声明目前仅为声明。
7. **四个会话编排工具（ask_user/next_run/subchat_*）被刻意排除在 `async=always` 之外**：`inProcessTool` 白名单包含它们（`turn.go:383-389`），因此它们保持同步语义；本组 `task_run/task_llm/task_status/task_stop` 不在白名单内，返回面必须按异步回执理解。

## task-B 批次发现（ask_user / next_run / subchat_open / subchat_finish）

> 本节由 task-B 产出，只列本批 4 个工具的**特有/共性**问题；逐工具证据与计数见各自 md（已计入上表）。共性发现 1/4/6 已覆盖一部分，此处不重复展开。

1. **B1 · LLM 可见 description 只取 `# 摘要` 第一段**（影响 ask_user / next_run / subchat_open）：框架 `describe` 用 `SplitN(...,"\n\n",2)[0]`（`E:/BizWorks/chonkpilot-framework/plugin/src/contract/contract.go:174-193`），插件 `src/main.go:160` 直接使用它。
   - ask_user：首段写“并等待回答”，与运行时非阻塞（立即返回占位结果）矛盾；批量聚合/超时说明不可见（契约 `:15-28`）。
   - next_run：丢“成功后本轮收尾 / 同轮 last-wins / 仅主会话 / 到限文案”（契约 `:16-19`）。
   - subchat_open：丢单聊/群聊判定、`{ok,session_id}` 终态、**结果不回流**、群聊 goal/playbook/convergence 与主 agent 必须在名单内（契约 `:15-54`）；`playbook/convergence` 描述又写“样板见摘要”，成为悬空引用（`:100-107`）。
   - subchat_finish 摘要只有一段，描述完整（唯一例外）。
2. **B2 · 这 4 个工具的契约 `async: never / timeout: 0` 不是运行时来源**：同步来自 `inProcessTool` 名字白名单（`turn.go:383-390`），gateway 超时来自 manifest `mcp.timeout_sec=600`（`src/initdata/capability/plugins/task/manifest.json:22`；优先级 `mcpgateway.go:942-952`）。契约 YAML 的 `async/timeout/runtime/output` 被 `contract.Parse` 丢弃（`contract/contract.go:23-29,63-92`）——与共性发现 1 同源，但本批结论是“实际 600s 而非 0”，不是“被强制 always”。
3. **B3 · 失败语义塌陷，且 ask_user 会留僵尸 running**：插件对 4 个工具都用 `pluginsdk.Text`（MCP `IsError=false`，框架 `sdk/plugin.go:613-618`）；即使 handler 返回 Go error（MCP `IsError=true`），`gwclient.Call` 只读 `structuredContent.status`（`src/lib/llm/server/gwclient.go:111-131`），worker 对非 ask 工具一律落 `completed` + `tasks.done(done)`（`turn.go:1614-1621`）。next_run/subchat 的 `{ok:false}`/`错误:` 文本因此都表现为“成功”，任务树不落 error（迁移前 `domainExecute` 会落 error）。ask_user 更严重：`isAskTool` 分支跳过落库与 done，MCP 级错误会让 tool 行与任务节点**永久 running**。
4. **B4 · 4 个契约均无 `# 返回值` 段**：`OK`、`{ok,session_id}`、`{"ok":true}`、占位文本、超时文本均未文档化；结合 B1，模型只能靠调用后的输出学习。
5. **B5 · 迁移后的旧路径/旧实现残留**（源码迁移已完成，残留集中在三处）：
   - 文档：`docs/ask-user/AskUser专题设计方案.md:263,274`、`docs/next-run/next_run自动续跑设计方案.md:6,78,296`、`docs/spec/30-function-points/3A-工具与编排.md:172,195` 仍指 `src/lib/llm/contracts/tools/*.tool.md`（已删）；`docs/spec/20-modules/25-mcp-server.md:57` 仍写“6 个域工具”（现只剩 `tool_result` 且该契约 2026-11-08 也已删除，需再核对）。
   - 产物：`dist/capability/tools/server/` 与 `dist/other/capability/tools/server/` 仍有 4 个旧 `category: server / runtime: builtin / core-only: true` 契约；构建脚本已显式删除该目录（`build-desktop.ps1:290-294`、`build-mcp-server.ps1:118-122`），属产物漂移，未修改。另：`dist/plugins/task/manifest.json`（2026-11-05）仍是 4 个 `hot_tools` 与旧描述，但**不影响本批 4 个工具的 hot 面**——插件 `plugin.mcp.register` 自报全部工具名作为 hot 列表（`sdk/plugin.go:558`），宿主对非空 `req.HotTools` 优先于 manifest（`mgr/api.go:331-339`）；仍应在部署前重建，避免 exe/描述/清单继续漂移。
   - 主仓旧分支：`src/lib/mcp-server/toolmeta/toolmeta.go:139-193` 仍保留 ask_user/next_run/subchat_* 的 domain 分支（ask_user 返回旧文案“已向用户提问，等待回答”、不传 `instance_id`），`toolmeta_spec_test.go:94` 仍断言旧文案。源码 `src/initdata/capability/tools/` 无对应 `category=server` 契约，正常构建不注册；若旧 dist 被使用会与插件工具名重复并走旧语义。[待核实：是否有部署仍携带旧 dist]
   - `turn.go`/`domainmcp.go` 在审查期间仍被并发编辑（mtime 11:22–11:28）；若后续提交改变 `isAskTool` 分支或 `inProcessTool` 名单，B2/B3 需复核。
6. **B6 · 测试覆盖缺口**：插件侧 `task/src/tools_test.go:92-110` 只校验 4 个契约的必填字段形状，不测 handler/Text/is_error/占位/会话创建；宿主 L1/L2 用 `moved_domain_test.go` 的**测试适配器**模拟插件（`src/lib/llm/server/moved_domain_test.go`、`src/test/chonkpilot-llm/unittest/moved_domain_test.go`），不启动真实插件。迁移后的真实链路（插件 `Text` → MCP → gateway → worker）没有端到端用例。
7. **B7 · 能力调用也受 SDK 默认 15s 约束**（与共性发现 2 同一机制）：本批 4 个 `plugin/v1/*` 能力均为毫秒级操作（提问登记/计数与 inbox/建会话/lifecycle），当前无影响；若数据面卡住，插件会得到 15s 超时错误文本（`sdk/plugin.go:316-331`）。

### task-B 契约/源码核对结果

- 4 个契约存在且必填与实现一致：`ask_user{question}`、`next_run{prompt}`、`subchat_open{agent,prompt,llm,scenario_id}`、`subchat_finish{}`；`manifest.hot_tools` 8 个与 8 个契约一一对应（`manifest.json:21`）。
- 插件 handler 映射无误（`src/main.go:143-152`）；4 个能力调用均带 `instance_id/turn/tool_call_id`（`main.go:406-470`），manifest `permissions.session` 四项齐全（`manifest.json:49`）。
- 展示名：gateway 注入 `tool_call_display_name` 并在调用前剥离（基线 §2）；`subchat_open` 合法地把注入值用作子会话 `purpose`/标题（`plugin_caps.go:322-338`、`subchat.go:108-121`），不是缺陷。

### task-B 范围差异

- 分配清单 4 个工具全部覆盖，无缺失、无额外工具；`task_run/task_llm/task_status/task_stop` 由 task-A 覆盖。
- 分配提示“契约已从 `src/lib/llm/contracts/tools/` 迁到 task 插件”已核实：**源码层迁移完成**，残留见 B5（文档引用、dist 旧产物、mcp-server 旧 domain 分支）。
- 分配提示“契约写 `async: never / timeout: 0`，真实行为横跨插件与宿主”已核实：真实同步由 `inProcessTool` 决定、超时由 manifest 600s 决定（B2）。
- 分配要求核对的“等待中/超时/取消/用户答复/子会话失败”返回形态已在各工具 §5 穷举；ask_user 的“答复不是 tool-result 而是后续用户消息”是本批最特殊的返回模型。