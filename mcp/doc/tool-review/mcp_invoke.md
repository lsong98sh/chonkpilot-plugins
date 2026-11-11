---
plugin: mcp
tool: mcp_invoke
contract: programmatic
handler: src/main.go:240
reviewed_at: 2026-11-08
status: verified
---

# mcp_invoke 审查

## 1. 一句话结论

**不建议当前形态**：不传 `request_id`，桥把它当“命令类”fire-and-forget，插件恒返回 `{ok:true,result:{accepted:true,topic:"mcp-tools-call"}}`；目标工具的结果、失败、超时、取消全部拿不到，而 LLM 看到的是成功。叠加“不传 instance/session/turn/tool_call_id/purpose”，instance-scoped 工具解析失败且错误被吞；`mcp_invoke` 还能自调用，绕过 llm-server 的场景工具白名单。

## 2. 调用链

LLM tool_call（外层，llm-server 已做白名单判定，`llm-server/turn.go:1464-1481`）
→ gateway `doCall`：剥离 `tool_call_display_name`→`req.Context.Purpose`（`gateway/src/mcpgateway.go:831-840`）
→ mcp 插件 provider（origin=builtin，`_meta.chonkpilot` 带 instance_id/work_dir/session/turn/tool_call_id/purpose，`gateway/src/contextmeta.go:13-49`）
→ MCP `tools/call` → SDK handler（`sdk/plugin.go:544-557`）
→ `mcpPlugin.invoke`（`src/main.go:240`）
→ `p.Call(ctx, "plugin/v1/tools.call", {name, arguments, work_dir})`（`main.go:264-266`）
→ 桥 `handleToolsCallGate`：三条件准入（`bridge/toolgate.go:69-109`）
→ `forwardMQ(..., command=true, args)`（`toolgate.go:87`）
→ **args 无 `request_id` ⇒ `hasRequestID=false` ⇒ `EmitAsync` + 立即返回 `{accepted:true,topic:"mcp-tools-call"}`**（`bridge/capabilities.go:222-236,238-248`；`commandMethods["plugin/v1/tools.call"]=true`，`capabilities.go:135`）
→ 插件 `unwrap(raw)` → `{ok:true,"tool":name,"result":{accepted:true,topic:"mcp-tools-call"}}`（`main.go:270`）
→ 背景 MQ 分支里 gateway 才真正 `doCall`（`mcpgateway.go:819-1082`）→ 目标 provider → 终态 `mcp-tasks-report`（`mcpgateway.go:391-421`）→ llm-server 按 `tool_call_id`/`task_id` 定位失败（`llm-server/handler_task.go:142-180`）→ 结果丢弃。

**schema 由代码构造**（无 `.tool.md`），以 `src/main.go:89-96` 为准；LLM 可见 schema 另被网关注入必填 `tool_call_display_name`（`gateway/src/registry.go:265-267`）。

## 3. 契约面

| 参数 | 类型 | required | 实现默认 | 说明 |
|---|---|---|---|---|
| `name` | string | 是 | — | “工具名（LLM 可见名）”；实际按 gateway **暴露名**解析（受 namespace 影响，`registry.go:309-327,342-355`） |
| `arguments` | object | 否 | `{}` | 非 object 时**静默置空**（`main.go:246-249`） |
| `timeoutSec` | integer | 否 | 配置 `invoke-timeout-sec`（默认 120） | clamp 5–1800（`main.go:250-259`；`config.go:85-87`） |

契约/描述问题：
- 描述“按名称调用一个工具并返回其结果”（`main.go:88`）与实现相反：**从不返回结果**，只返回受理 ack。
- 描述未说明 `work_dir` 必须属于已打开项目、目标必须在该 work_dir 的已装配工具面内（`toolgate.go:91-108`）。
- 描述未说明 `arguments` 类型错误时会被清空。
- `timeoutSec` 实际无效（见 §4）。
- 宿主指引要求 mcp_find/mcp_load 的参数风格，但未对 mcp_invoke 说明“这是唯一的跨工具调用入口、目标名必须是暴露名”。

## 4. 实现面

- 入口与参数：`name` 空 → 报错；`arguments` 断言失败 → 空 map；`timeoutSec` clamp（`main.go:242-259`）。
- 超时：`context.WithTimeout(ctx, timeout)`（`main.go:260-261`）只作用于 `p.Call`。但：
  1. 异步 ack 在毫秒级返回，超时根本不会触发；
  2. SDK `p.Call` 内部再包一层 **固定 15s**（`Options.Timeout` 未设置 → `defaultWait=15s`，`sdk/plugin.go:39,149-151,316-318`，`main.go:53-61` 未设置 Timeout），所以即使改成同步，`timeoutSec`/配置 120/上限 1800 也都不可达，实际上限 15s；
  3. 没有把 `timeout` 映射到 gateway 的调用级超时（gateway 从 `arguments.timeout`/`_timeout` 读，`mcpgateway.go:824-830`）。
- 上下文：插件手里有完整 `CallContext`（`sdk/plugin.go:61-73`：InstanceID/WorkDir/DataDir/Session/Turn/ToolCallID/TopSession/Parent/Purpose/Sandbox），但**只用了 `cx.WorkDir`**（`main.go:265`）。对比内置实现 `toolmeta.metaInvoke` 会传 `instance_id/session/turn/tool_call_id/purpose/request_id` 并强制 `async:"never"`（`chonkpilot/src/lib/mcp-server/toolmeta/toolmeta.go:367-391`）。
- 准入（同步，失败会真报错）：`toolgate.admitToolCall` 三条件——`work_dir` 归一后存在且含 `.chonkpilot`、该 work_dir 有活动实例、`name` 在“该 work_dir 各实例工具面的并集”内（`toolgate.go:91-109,126-140,144-180`）。注意：
  - 工具面拉取失败时 `toolAllowed` 直接 `continue` 并最终返回 false（`toolgate.go:126-139`），会被报成 `tool_not_allowed`（误导）。
  - 工具面按**实例并集**判白，但实际 `doCall` 用 `findFor(req.Name, req.InstanceID)`，而本插件传的 `instance_id` 为空 ⇒ 只在 **global** 路由空间查（`mcpgateway.go:856-860`；`registry.go:342-355`）。因此“gate 通过、执行时 unknown tool”的组合必然存在（instance-scoped 工具）。
- 递归/自调用：`mcp_invoke` 自身在该工具面内（hot，`manifest.json:21`）⇒ 准入通过 ⇒ 可以 `mcp_invoke` 调 `mcp_invoke`。插件无深度、次数、自名检测（`main.go:240-271`）；gateway 也无“元工具不可再调元工具”的限制。嵌套深度受模型输出 JSON 深度限制，但每层都会创建一个 gateway 执行任务（无 tool_call_id 归属），且错误全丢。
- 白名单旁路：llm-server 只对**顶层**调用的 `args["name"]` 做目标二次判白（`turn.go:1470-1481`；`isMetaInvokeTool` 只认 `mcp_invoke`，`turn.go:1795-1798`）。嵌套调用不经过 llm-server，因而 `mcp_invoke{name:"mcp_invoke", arguments:{name:"<任意工具>", …}}` 可绕过场景 agent 的工具白名单（实际能否执行取决于 gateway 工具面与 work_dir 准入，但不再受 agent 白名单约束）。
- 沙箱：外层沙箱策略不会下传给内层调用；内层由 gateway 按目标 provider 自己的 `tool_sandbox.mcps` 开关与 `work_dir` 重新推导 `_meta.chonkpilot.sandbox`（`gateway/src/provider.go:1255-1295`）。因此内层工具看到的是“按目标 server 开关重建的白名单”，而不是外层调用上下文里的同一份策略（[推断] 若两侧开关不一致，约束强弱会变化；未发现显式校验两者一致）。
- 副作用：内层调用真实执行（有写盘/网络/进程副作用的工具都会被触发），但在 LLM 侧不可见；无幂等保护——模型可能因“没有结果”重复调用，导致副作用重复。
- 资源清理：无插件侧资源；内层任务由 gateway 执行池管理（`allowDetach` 等，`mcpgateway.go:998-1082`）。

## 5. 返回面：LLM 实际看到的所有形态

外层的 `isError` 只由插件返回的 error 决定；成功统一是缩进 JSON（`sdk/plugin.go:613-626`）。**被调工具的任何状态都不出现在外层 isError 里。**

| # | 形态 | isError | LLM 可见内容 / 语义 |
|---|---|---|---|
| A | 受理成功（**当前所有“正常”调用的唯一形态**） | **false** | `{"ok":true,"result":{"accepted":true,"topic":"mcp-tools-call"},"tool":"<name>"}`。目标工具在后台执行，结果不会回来 |
| B | `work_dir` 缺失/不是项目 | true | `plugin/v1/tools.call: not_a_chonkpilot_project: work_dir 缺失或不可解析（"…"）：必须是已存在的项目目录`（或 `<dir> 下没有 .chonkpilot 目录…`）（`toolgate.go:91-102`；`bridge/errors.go:41-49`；`sdk/plugin.go:324`） |
| C | 项目未在宿主打开 | true | `plugin/v1/tools.call: work_dir_not_open: work_dir 无活动实例（未在宿主打开）: <dir>`（`toolgate.go:101-104`） |
| D | 工具不在本 work_dir 工具面 / 工具面拉取失败 | true | `plugin/v1/tools.call: tool_not_allowed: 工具 "<name>" 不在该 work_dir 的已装配工具面内`（`toolgate.go:105-107,126-139`）——网关抖动时同样是这句 |
| E | `name` 为空/全空白 | true | `name 不能为空`（`main.go:244`） |
| F | `arguments` 不是对象（字符串/数组/null） | **false** | 静默 `{}` → 目标工具以空参数执行 → 形态 A（`main.go:246-249`） |
| G | 目标工具不存在 / instance-scoped 但只在 global 解析失败 | **false** | 形态 A；gateway 的 `unknown tool %q`（`mcpgateway.go:857-860`）经 MQ 返回后无人消费 |
| H | 目标工具返回业务错误（`isError:true`） | **false** | 形态 A；目标错误只出现在 `mcp-tasks-report.result_summary`，因无 `tool_call_id` 无人匹配（`handler_task.go:142-180`） |
| I | 目标工具异步（always/auto 阈值）/ manual/never 超时裁决 | **false** | 形态 A；`pendingResult`/`awaitingResult`（`mcpgateway.go:1311-1336`）永远不会传到插件；终态同样丢失 |
| J | 目标工具执行超时/取消 | **false** | 形态 A |
| K | 递归自调用 | **false** | 逐层形态 A；无“递归已拒绝”的返回形态（此形态不存在） |
| L | 桥/传输失败（插件进程与桥断链） | true | 裸传输错误文本（如 `plugin/v1/tools.call: <transport error>`），无 code |
| M | 参数解析失败（MCP arguments 非法 JSON） | true | `参数解析失败: <err>`（`sdk/plugin.go:546-549`） |
| N | 返回 JSON 编码失败 | true | `json 编码失败: <err>`（`sdk/plugin.go:617-620`） |

补充：
- 没有 `status` 字段；`pending`/`awaiting` 语义在插件层不可达。
- 没有 `code` 字段；bridge 的 `not_a_chonkpilot_project`/`tool_not_allowed` 等稳定码只以 `code: msg` 文本形式混在错误字符串里（`bridge/errors.go:41-49`）。
- 返回体积：当前不会爆（因为根本没有结果）。一旦改为同步且不设上限，目标工具的大结果会被原样放进 `result`（`main.go:270`），有爆上下文风险。
- 恢复路径：只有 B/C/D/E/L 带可读原因；A/F/G/H/I/J 都表现为“成功”，模型没有可执行的恢复动作，最可能的错误行为是**重复调用**（副作用重复）。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | 不传 `request_id` ⇒ 桥按命令类 fire-and-forget，恒返回 `{accepted:true}`；插件把 ack 当结果返回 | `main.go:264-270`；`capabilities.go:112-150(135),222-236,238-248` | 契约“返回其结果”与实现相反；目标结果/失败/超时/取消全部丢失；LLM 必然误判成功 |
| P0 | 不传 `instance_id/session/turn/tool_call_id/purpose` ⇒ 内层任务无归属；空 instance 只在 global 路由解析 | `main.go:264-266`；`sdk/plugin.go:61-73`（字段齐全但只用 WorkDir）；`mcpgateway.go:857,998-1003,391-421`；`registry.go:342-355`；`handler_task.go:142-180` | instance-scoped 工具报 `unknown tool` 且错误被吞；终态无法回填 LLM/任务树；展示名丢失 |
| P0 | `mcp_invoke` 可自调用且无深度/次数限制；llm-server 只对顶层 `args.name` 判白 | `main.go:240-271`；`toolgate.go:91-108`；`turn.go:1470-1481,1795-1798` | `mcp_invoke(mcp_invoke(<任意工具>))` 绕过场景 agent 工具白名单；可放大执行任务 |
| P1 | `arguments` 非 object 时静默 `{}`，目标工具以空参数执行 | `main.go:246-249` | 写类工具可能误删/误写；模型不知道参数被丢弃 |
| P1 | `timeoutSec` 完全无效：异步立即返回 + SDK 固定 15s + 未映射 gateway 调用级 timeout | `main.go:250-266`；`sdk/plugin.go:39,149-151,316-318`；`mcpgateway.go:824-830,939-953` | 用户/模型无法控制目标超时；配置 120、clamp 1800 均为死代码 |
| P1 | 被调工具 `isError` 不透传（即使改成同步，外层仍是 `ok:true`） | `main.go:270` | 失败需二次解析 `result`；模型极易把失败当成功 |
| P1 | `cx.Purpose` 不下传；内层调用无 `purpose`/展示名 | `main.go:262-266`；`gateway/src/contextmeta.go:41-45`；`subjects.go:73-78` | 任务树/聊天气泡只显示工具名；依赖 purpose 的工具（如 subchat_*）语义降级 |
| P1 | 工具面拉取失败被报成 `tool_not_allowed` | `toolgate.go:126-139,156-165` | 网关抖动时错误文案误导，模型会换工具而非重试 |
| P1 | 修复为同步后：mcp 插件 `timeout_sec=600` + 默认 invoke 120s，单次调用可长期占用 LLM 轮次 | `manifest.json:22`；`config.go:85-87`；`mcpgateway.go:939-953` | 同步阻塞风险；需调用级 async 策略配合 |
| P2 | schema 无 `name` 枚举/示例、无 `async`/批量参数 | `main.go:89-96` | 模型拼错工具名（结果仍是“成功”）；无法批量或控制异步 |
| P2 | 未说明 `work_dir` 必须属于已打开项目 | `main.go:88`；`toolgate.go:91-108` | 非项目会话必失败，模型不知原因 |
| P2 | 返回体积无上限（同步化后） | `main.go:270` | 大结果原样进上下文 |
| P2 | 未禁止/未提示 `name="mcp_invoke"` 自调用 | `main.go:88-97` | 模型可能无意义自套，放大任务与 token |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| **补 `request_id`**（如 `"req-"+cx.ToolCallID`）使桥走同步 `Emit().Wait()`，真正返回目标结果 | 优化（correctness，最高优先） | 契约兑现，失败可感知 | 低（1 行级，`main.go:264-266`） | 行为变化；若同步化则受 SDK 15s 限制，需同时处理超时 | **现在做** |
| 透传 `instance_id/session/turn/tool_call_id/top_session/parent/purpose`（`CallContext` 已有） | 优化（correctness） | instance-scoped 工具可执行；终态能回填 LLM/任务树；展示名不丢 | 低（1 文件） | 非 breaking（gateway 已接受这些字段，`subjects.go:61-86`） | **现在做** |
| 同步化时传 `arguments["async"]="never"`（对齐内置 `metaInvoke`）或显式 `async` 透传 | 改善 | 目标行为可预期，避免 pending/awaiting 形态丢失 | 低（1 文件） | 目标若依赖异步会被强制同步（超时风险需评估） | 现在做 |
| 把目标 `isError`/`status` 提升到外层（如外层 `isError=true` 或返回 `{ok:false,error:{...}}`） | 改善 | 模型无需二次解析即可判失败 | 低（1 文件） | 返回结构变化，需同步契约/测试/前端展示 | 现在做 |
| 禁止自调用/限制递归深度（如 name 为 mcp_invoke 时直接拒绝，或透传 depth 并在 gateway 侧限制） | 优化（安全） | 消除白名单旁路与任务放大 | 中（插件 + gateway 或仅插件拒绝） | 拒绝新形态属 breaking（合理） | 现在做 |
| `arguments` 非对象时报参数错误，而非清空 | 改善（安全） | 避免空参数执行 | 低（1 文件） | 行为更严格 | 现在做 |
| `timeoutSec` 映射到 gateway 调用级 `timeout`（写入 `arguments["timeout"]`），并让 SDK 不再用 15s 截断（设置 `Options.Timeout` 或改 p.Call 超时策略） | 优化 | 超时可预期 | 中（插件 + 可能改 SDK 复用层） | 需评估长同步调用对轮次的影响 | 排期做 |
| 返回 `{status, task_id}` 供异步目标（若选择保留异步）并能被 `task_status` 查询 | 增强 | 长任务可追踪 | 高（需任务归属与状态查询链路） | 新增字段 | 排期做 |
| 支持批量调用（`calls:[{name,arguments}]`） | 增强 | 少轮次、少 token | 中 | 新参数 | 仅记录 |
| 描述里写清 `name` 必须是“gateway 暴露名”（内置插件无前缀；第三方/实例节点可能带前缀） | 改善 | 减少拼错 | 低 | 无 | 现在做 |

## 8. 建议的测试/验收（不执行）

- 同步回执：目标工具返回文本 → `mcp_invoke` 必须直接拿到该文本；目标 `isError` → 外层可见失败（修复验收）。
- 上下文透传：项目级 dir 节点工具（instance scope）经 `mcp_invoke` 可执行；任务树出现被调工具节点；chat brief 显示 inner purpose。
- 准入失败三码：`not_a_chonkpilot_project` / `work_dir_not_open` / `tool_not_allowed` 的文本与 isError。
- 工具面拉取失败（gateway 未起/超时）→ 当前误报 `tool_not_allowed`（记为修复前基线）。
- `arguments` 分别为 object / string / array / null / 缺失 的行为。
- `timeoutSec` = 1/5/120/1800/非数字；验证当前完全无效（修复后验证是否真正生效）。
- 递归：`mcp_invoke(mcp_invoke(x))`，验证当前可执行、修复后拒绝（含 depth 上限）。
- 白名单旁路：场景 agent 白名单只含 mcp_invoke 时，嵌套调用非白名单工具 → 当前可绕过，修复后必须被拒。
- 目标为异步/超时/取消三形态：修复前全部看不到，修复后需覆盖 pending/awaiting/终态。
- 结果体积：大输出目标工具，统计外层返回字节数与是否截断。

## 9. 基线修正（仅当共通基线不成立时）

- 基线 1“gateway doCall → 插件 handler → 返回 → llm-server 回喂”：对本工具**不成立**。插件到 gateway 的那一段是 `plugin/v1/tools.call` 桥方法，桥对无 `request_id` 的命令类方法走 fire-and-forget（`capabilities.go:222-236`），LLM 看到的是 ack 而不是目标结果。
- 基线 2“`tool_call_display_name` 由 LLM 填、gateway 剥离后写 `purpose`、下游插件看得到”：外层成立（`CallContext.Purpose` 有值），但本插件**不向下传**，内层调用看不到。不能据此认为内层任务的展示名正常。
- 基线 6“异步工具结果经 mcp-tasks-report 回执”：在本路径上回执会发出，但因内层任务缺少 `tool_call_id/session/turn`，llm-server 无法归属（`handler_task.go:142-180`），等价于没有回执。
- 基线 3“沙箱只对写工具生效”：本工具自身不写盘；但它是**跨工具调用通道**，内层写工具的沙箱由目标 provider 重新推导（`provider.go:1255-1295`），不是外层策略的透传——审查其他写工具时不能假设经 `mcp_invoke` 调用仍受同一白名单约束。
- 基线 8“file 的 `{results[],summary}`”：本工具返回单对象，无批量外壳。
## 10. 修复记录（2026-11-08 / 2026-11-11）

- **2026-11-08（同步化 + 上下文）**：契约迁移到 `src/tools/mcp/mcp_invoke.tool.md`；载荷新增 `request_id`，桥从 fire-and-forget 改走 `Emit().Wait()`，目标结果/错误/超时/取消真实返回；`name/arguments` 之外透传 `instance_id/session/turn/tool_call_id/display_name/top_session/parent/data_dir`（内层任务可归属、展示名不丢、instance-scoped 工具可解析）；**禁止 `mcp_invoke` 自调用**；`arguments` 非 object（含 null）报参数错误；目标 `isError` → 外层错误；`{accepted:true}` 受理回执 → 明确错误；异步目标的 `structuredContent`（`status=pending|awaiting`、`task_id`）原样透出；SDK 宿主调用上限从 15s 放宽（`invokeTimeoutMax+60`），避免同步等待被提前截断。
- **2026-11-11（体积与超时口径）**：返回结果加 **1MiB 上限**——文本按 UTF-8 边界截断、结构化结果按 JSON 文本截断，返回 `truncated:true` 与 `total_bytes` 并附恢复提示（`TestInvokeOutcomeTruncation`）；契约 `timeout` 从 600 提到 1860，与 `timeoutSec` 的 5–1800 区间对齐（原先 >600 的等待必然先被外层转 awaiting）。
- 仍为基线（未改）：插件侧 `timeoutSec` 到点只结束等待，不取消目标（目标可能继续在后台执行）；调用级 `async` 未强制，异步目标按自身策略返回 pending；`mcp_invoke` 的跨场景工具白名单仍由 llm-server 在顶层按 `args.name` 判定。
