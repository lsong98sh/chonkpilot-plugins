# mcp 工具审查索引

审查对象：`mcp` 插件（元工具：检索 / 加载 / 调用）。
契约来源：**程序化注册，无 `.tool.md`**；三个工具的 schema 均由 `src/main.go` 的 `p.Tool(...)` 代码构造（`src/main.go:69-97`），LLM 可见 schema 还会被网关注入必填 `tool_call_display_name`（`chonkpilot-agent/gateway/src/registry.go:265-267`）。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| mcp_find | 可用，有风险 | 0 | 4 | 5 | [mcp_find.md](./mcp_find.md) |
| mcp_load | 有风险（skill/resource 链路断裂） | 1 | 3 | 4 | [mcp_load.md](./mcp_load.md) |
| mcp_invoke | **不建议当前形态** | 3 | 6 | 4 | [mcp_invoke.md](./mcp_invoke.md) |
| 合计 | — | **4** | **13** | **13** | — |

## 工具一句话结论

- **mcp_find**：能列全（含非 hot、插件工具、技能、资源），但检索面是全局注册表而非当前实例；`limit` 静默截断（默认 20、上限 200），`type=all` 时工具先占满额度；prompts/resources 读取失败被吞成 0 条。
- **mcp_load**：`tool` 分支可用；`skill/prompt/resource` 分支不带 `instance_id`，而用户/项目级 capability 节点全部注册在 instance scope（`capnodes.go:397-400` + `primitives.go:31-58`），因此知识库技能/资源**恒 not found**。另外无 `kind` 参数（宿主指引却要求传），返回正文/JSON 双份。
- **mcp_invoke**：不传 `request_id`，桥按命令类 fire-and-forget（`bridge/capabilities.go:135,222-236`），插件恒返回 `{ok:true,result:{accepted:true}}`；目标结果/失败/超时/取消全部丢失。再叠加 `instance_id/session/turn/tool_call_id/purpose` 未透传，内层任务无归属、instance-scoped 工具解析失败且错误被吞；`mcp_invoke` 可自调用，嵌套后绕过 llm-server 的场景工具白名单。

## 分配清单 vs 源码实测（范围差异）

| 分配项 | 实测 | 依据 |
|---|---|---|
| 契约来源“程序化注册，无 .tool.md” | 成立；已在每篇文档写明“schema 由代码构造”并以代码为准 | `src/main.go:69-97` |
| 3 个工具、hot_tools 3、timeout 600、category meta | 成立（`namespace "-"`、`origin builtin`，工具名不加前缀） | `manifest.json:18-24` |
| mcp_find：检索范围 / query 空值 / limit 上限 / 排序截断 | 范围=gateway 全局（`tools.list` 空 instance → `routesAll`；`prompts/resources.list` 恒 all）；query 空=列全量；上限 200、默认 20；工具按名排序，跨组固定 tool→prompt→resource，达到 limit 即 break，无截断标记 | `main.go:124-169,274-344`；`mcpgateway.go:596-606`；`primitives.go:64,160` |
| mcp_load：四类返回结构与体积 | tool=entry+顶层双份 schema；skill/prompt=content+text 双份；resource=content+text+uri+mimeType；无字节上限；skill/resource 实际不可读（instance scope） | `main.go:455-489`；`primitives.go:31-58,121-233` |
| mcp_invoke：递归 / 权限与沙箱 / async 回执 / 错误透传 / 体积 / namespaces | 递归无深度限制且可自调用；准入三条件（`.chonkpilot` + 工具面 + 实例打开），无插件级权限；async 终态因无 tool_call_id 无法归属 → 结果丢弃；仅准入/传输错误透传，目标错误全丢；当前无结果故无体积问题（同步化后需上限）；目标名是 gateway 暴露名，空 instance 只在 global 解析 | `main.go:240-271`；`toolgate.go:91-140`；`mcpgateway.go:819-1082,391-421`；`registry.go:309-355`；`handler_task.go:142-180` |
| invoke 与展示名注入的交互 | 外层 `Purpose` 有值（`CallContext.Purpose`），插件不向下传 → 内层任务/气泡丢展示名 | `sdk/plugin.go:61-73`；`contextmeta.go:41-45`；`main.go:262-266` |
| 两种 isError | 已在每篇 §5 分开：元工具自身失败=isError true + 纯文本；被调工具失败=不可见（isError false 的假成功） | `sdk/plugin.go:552-556,624-626` |

## 额外发现（不在分配清单）

1. **两套 meta 实现并存漂移**：`chonkpilot/src/lib/mcp-server/toolmeta/toolmeta.go` 仍保留内置 `mcp_find/mcp_load/mcp_invoke`（schema 含 `purpose`/`kind`、`async:"never"`、完整上下文透传），但 `dist/capability/tools/meta/` 与 `src/initdata/capability/tools/meta/` 均为空 → 当前**不暴露**内置版，实际以本插件为准 [待核实：是否构建期生成 meta 契约]。
2. **宿主指引过期**：`llm-server/memory_guide.go:162-163` 与 `scenarios/default/main.agent.md:16` 让模型给 `mcp_find.purpose`、`mcp_load.kind`，本插件都没有这两个参数（会被忽略）；指引还说 prompt 已移出检索面，但本插件支持 `type=prompt` 并纳入 `all`。
3. **配置面**：manifest `config.scope=plugin` 但实现是**用户级** usr 键 `plugins.mcp.*`（`config.go:33-46,85-87`），默认 `default-limit=20 / include-non-hot=true / invoke-timeout-sec=120`。
4. **build/dist 产物**：`build/plugin-apps/mcp/chonkpilot-plugin-mcp.exe` 与 `dist/plugins/mcp/` 无法从源码核对行为（只读审查），本次结论均以 `mcp/src` 源码为准；未发现源码与 manifest 的版本冲突。

## 文档清单

- `E:/BizWorks/chonkpilot-plugins/mcp/doc/tool-review/mcp_find.md`
- `E:/BizWorks/chonkpilot-plugins/mcp/doc/tool-review/mcp_load.md`
- `E:/BizWorks/chonkpilot-plugins/mcp/doc/tool-review/mcp_invoke.md`
- `E:/BizWorks/chonkpilot-plugins/mcp/doc/tool-review/00-index.md`（本文件）

## 修复进度（2026-11-08 / 2026-11-11）

> 上表与各文 §6 是**审查时基线**，计数未随修复回改。当前实现状态：

- **2026-11-08（P0 清零 + 契约化）**：三个工具全部迁移到 `src/tools/mcp/*.tool.md`（schema/description/async/timeout 单源，`contract.Parse + MetaFor`）；`mcp_invoke` 补 `request_id` 走同步、透传 `instance_id/session/turn/tool_call_id/display_name/top_session/parent/data_dir`、禁止自调用、`arguments` 严格校验、目标 `isError` 透传为外层错误、受理回执判失败、异步状态透出；`mcp_load` 的 skill/prompt/resource 携带 `instance_id`（链路打通）；`mcp_find` 增加 `total`/`truncated`、`type=all` 轮转、`match` 加权排序。明细见各文 §10。
- **2026-11-11（遗留收口）**：`mcp_load` 按名称全量查工具（不受 `include-non-hot` 检索过滤影响）；`prompts/resources` 列表读取失败不再吞成 0 条（find 报错、load 区分“读取失败”与“未找到”）；`mcp_invoke` 返回加 1MiB 上限（`truncated`/`total_bytes`，UTF-8 边界截断），契约 `timeout` 600 → 1860 与 `timeoutSec` 5–1800 对齐。宿主侧指引同步修正（`memory_guide.go`、`scenarios/default/main.agent.md`）：去掉不存在的 `kind`/`purpose` 参数，补 `prompt` 与 `match`。
- **仍为已知边界（未改）**：`mcp_find` 检索面是宿主全局能力面（跨实例，契约已注明）；entry 携带完整 `input_schema`；`mcp_load` 返回 `content`+`text` 双份；`mcp_invoke` 的 `timeoutSec` 到点只结束等待、不取消目标；跨场景白名单仍由 llm-server 在顶层按 `args.name` 判定。