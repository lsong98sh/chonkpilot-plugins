---
plugin: mcp
tool: mcp_load
contract: programmatic
handler: src/main.go:174
reviewed_at: 2026-11-08
status: verified
---

# mcp_load 审查

## 1. 一句话结论

有风险：`tool` 分支可用；**skill/prompt/resource 分支在真实能力面下基本不可用**——加载请求不带 `instance_id`，而用户/项目级 capability 节点全部注册在 instance scope，gateway 只会去 global scope 找，必然 `not found`。另外只收 `name`，不接受宿主指引要求的 `kind`。

## 2. 调用链

LLM tool_call
→ gateway `doCall` → mcp 插件 provider（origin=builtin）
→ MCP `tools/call` → SDK handler（`sdk/plugin.go:544-557`）
→ `mcpPlugin.load`（`src/main.go:174`）
→ `lookupEntry`（`main.go:204`）：依次 `plugin/v1/tools.list` → `prompts.list` → `resources.list`，按 name 精确命中（tool 优先）
→ tool 分支：直接用列表条目组装（不再二次取详情，`main.go:183-185,440-452`）
→ skill/prompt：`plugin/v1/prompts.get {name}`；resource：`plugin/v1/resources.read {uri}`（`main.go:440-452`）
→ `mergeLoad`（`main.go:455-489`）→ `mcpResult` 缩进 JSON。

**schema 由代码构造**（无 `.tool.md`），以 `src/main.go:80-85` 为准。

## 3. 契约面

| 参数 | 类型 | required | 说明 |
|---|---|---|---|
| `name` | string | 是 | 工具名 / 技能或提示名 / 资源名或 uri（`main.go:83`） |

无 `kind` 参数。而宿主注入的指引明确要求 `kind`：

- `chonkpilot/src/lib/llm/server/memory_guide.go:163`：`mcp_load（name=资产名，kind=资产类型；内容实时读取）`
- `chonkpilot/src/initdata/capability/scenarios/default/main.agent.md:16`：同上。

由于 schema 没有 `additionalProperties:false`，模型按指引传 `kind` 不会报错，会被**静默忽略**（`main.go:174-200` 只读 `name`）。同名工具与资产并存时永远先命中工具（`main.go:204-211`）。

返回字段（代码）：`ok`、`kind`、`type`、`name`、`entry`、可选 `description`/`server`；tool 另加 `input_schema`；skill/prompt 另加 `content` + `text`；resource 另加 `uri?`/`content` + `text`/`mimeType?`（`main.go:455-489`）。

## 4. 实现面

- 解析：`name = TrimSpace(asString(args["name"]))`，空则报错（`main.go:175-178`）。
- 查找：**大小写敏感、精确匹配**；tool 命中优先，其次 prompt/skill（`promptEntries` 用 `requested="prompt"`，但宿主 `type=skill` 会保留为 skill，`main.go:406-420`），最后 resource 按 `name` 或 `uri` 命中（`main.go:217-221`）。
- **instance 作用域（核心问题）**：`callDetail` 只把 `name`/`uri` 传给宿主，不带 `instance_id`（`main.go:443-449`）。gateway `handlePromptsGet` / `handleResourcesRead` 用 `eachPrimitive(req.InstanceID, false, …)`（`primitives.go:121-157,198-233`），而 `eachPrimitive` 在 `!all` 时只保留 `scope==global` 或 `scope==instance` 的 provider（`primitives.go:31-58`）。能力面 dir 节点注册时 `"scope": instanceID`（`chonkpilot/src/lib/llm/server/capnodes.go:397-400`，provider scope 落到 `mcpgateway.go:1732-1744`）。因此 `instance_id` 为空 ⇒ 只剩 global scope provider；项目级/项目私有级/user 级能力节点（scope=实例 id）**全部被排除**。
  - 对比：`mcp_find` 的 `prompts.list`/`resources.list` 用 `eachPrimitive("", true)` 全量聚合并并入注册资产（`primitives.go:64,99-114,160-195`），所以“列得出来、读不回来”。
  - 结论：能力面技能/资源走 `mcp_load` 时，宿主返回 `prompt %q not found` / `resource %q not found`（`primitives.go:153-155,222-224`）。
- 第二层不一致：`prompts.list` 会并入 `reg.allAssets()` 的 prompt/skill 资产（`primitives.go:99-114`），但 `handlePromptsGet` **不查资产表**，只遍历下游原语（`primitives.go:121-157`）。仅以注册资产形式存在的 skill 永远 `not found`（实际资产注册量 [待核实]）。
- 列表读取失败：`lookupEntry` 对 `tools()` 的错误直接忽略；`promptEntries`/`resourceEntries` 的 `listItems` 失败返回 nil（`main.go:323-327`）。三者都失败时最终报“未找到条目”，把内部故障伪装成不存在（`main.go:179-182,204-223`）。
- 体积：无字节上限；同时存在重复字段：
  - tool：`entry.input_schema`（`entry` 内含 `Schema`，`main.go:40-50,462-465`）与顶层 `input_schema`（`main.go:472-474`）各一份 → schema 约 2 倍。
  - skill/prompt/resource：`content` 与 `text` 两份相同正文（`main.go:475-486`）→ 正文约 2 倍。
- 副作用：无写操作。并发/幂等安全（只读）。
- 资源清理：无。

## 5. 返回面：LLM 实际看到的所有形态

成功走 `mcpResult` 缩进 JSON；失败走 `mcpError` 纯文本 + `IsError=true`（`sdk/plugin.go:552-556,613-626`）。无 `status`/`code` 字段。

| # | 形态 | isError | LLM 可见内容 |
|---|---|---|---|
| A | tool 命中 | false | `{"description?":…,"entry":{…,"input_schema":{…}},"input_schema":{…},"kind":"tool","name":…,"ok":true,"server?":…,"type":"tool"}`（schema 出现两次） |
| B | skill/prompt 命中（仅当 provider 是 global scope） | false | `{"content":"<正文>","entry":{…},"kind":"skill|prompt","name":…,"ok":true,"text":"<正文>","type":…}`（正文两遍） |
| C | resource 命中 | false | `{"content":…,"entry":{…},"kind":"resource","mimeType?":…,"name":…,"ok":true,"text":…,"type":"resource","uri"?…}` |
| D | name 为空/全空白 | true | `name 不能为空`（`main.go:177`） |
| E | 未找到（含所有列表读取失败） | true | `未找到条目 "x"`（`main.go:181`）；无 code、无“可用名称相近项”提示 |
| F | 命中但详情读取失败（当前 skill/resource 的常态：instance scope 不可见） | true | `读取 prompt "x" 内容失败: plugin/v1/prompts.get: internal: prompt "x" not found`（`main.go:190-192`；`capabilities.go:231-234`；`sdk/plugin.go:324`）——resource 为 `读取 resource … 失败: … resources.read: internal: resource … not found` |
| G | 宿主详情返回非对象 | true | `宿主 plugin/v1/prompts.get 返回非对象结果`（`main.go:231-234`） |
| H | kind 不支持（不可达；仅内部构造异常） | true | `不支持加载条目 "x"（kind=…）`（`main.go:187-189`） |
| I | 参数解析失败 | true | `参数解析失败: <err>`（`sdk/plugin.go:546-549`） |
| J | 结果 JSON 编码失败 | true | `json 编码失败: <err>`（`sdk/plugin.go:617-620`） |

“部分成功”不存在：单条目工具，失败即整次失败。与 file_read 不同，没有 `results[]/summary`。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | 非 tool 分支不带 `instance_id`，而能力面技能/资源全部是 instance scope → `mcp_load` 读不到知识库技能/资源 | `main.go:443-449`；`primitives.go:31-58,121-157,198-233`；`capnodes.go:397-400`；`mcpgateway.go:1732-1744` | `mcp_find` 能列出、`mcp_load` 恒 `not found`；宿主指引的核心链路断裂 |
| P1 | 无 `kind` 参数，模型按宿主指引传 `kind` 被静默忽略；同名时 tool 优先 | `main.go:79-86,204-223`；`memory_guide.go:163`；`main.agent.md:16` | 模型以为按类型过滤了，实际返回/加载另一个同名条目 |
| P1 | 列表读取错误被吞，最终报“未找到条目” | `main.go:204-223,323-327` | 内部故障被误报为不存在，恢复动作错误（改 name 而不是重试） |
| P1 | `input_schema` 与 `content/text` 双份，无字节上限 | `main.go:455-489,40-50` | token 约 2 倍；大技能正文/大 schema 易爆上下文 |
| P2 | 注册资产只被 list 收录、get 不查 | `primitives.go:99-114` vs `121-157` | 仅以资产形式存在的 skill 列得出读不到（实际存量 [待核实]） |
| P2 | `prompts.get` 只取首条 message 的文本 | `primitives.go:140-151` | 多消息/非文本 prompt 被静默截断 |
| P2 | 大小写敏感精确匹配，无近似建议 | `main.go:204-223` | 模型拼错即失败，且不知道近似候选 |
| P2 | `includeNonHot=false` 时非 hot 工具无法加载，描述未说明 | `main.go:282,398-400`；`config.go:86` | 配置与工具面行为耦合，模型不知原因 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| `callDetail` 带上 `instance_id`（来自 `CallContext.InstanceID`） | 优化（correctness） | 技能/资源真正可读 | 低（`main.go` 1 文件；`loadRequest`/`callDetail` 加字段） | 非 breaking；需回归 prompts.get/resources.read | 现在做 |
| 新增 `kind`（tool/skill/prompt/resource，可选），并在 `lookupEntry` 中按 kind 约束 | 改善 | 消除同名歧义、与宿主指引对齐 | 低（1 文件） | 非 breaking（新增可选参数） | 现在做 |
| 去掉 `text` 重复字段；tool 不再重复 `input_schema`（保留 `entry` 或顶层其一） | 优化 | token 约减半 | 低（1 文件 + 测试） | 可能 breaking（依赖 `text` 的调用方/回放）；建议先加 `compat` 开关 | 排期做 |
| 增加 `maxBytes`/分页（如 `offsetBytes`）与 `truncated` | 增强 | 大资源可控，不爆上下文 | 中（需要与宿主 content 读取配合） | 新增字段为主 | 排期做 |
| 未找到时返回近似候选（name 子串/编辑距离 Top3） | 增强 | 一次命中，少试错 | 低（复用 find 列表） | 非 breaking | 现在做 |
| 列表读取失败与“不存在”区分：失败 → isError/internal；不存在 → isError/not_found 文案 | 改善 | 恢复动作正确 | 低（1 文件） | 文案变化 | 现在做 |
| `prompts.get` 支持多 message 拼接或返回 `messages[]` | 增强 | 不丢提示内容 | 中（gateway + 插件） | 返回结构扩展 | 排期做 |

## 8. 建议的测试/验收（不执行）

- tool 命中、skill 命中、resource 命中（按 name / 按 uri）三形态的字段完整性。
- **instance scope**：项目级 skill/resource 在 `instance_id` 缺失时必须复现 `not found`；带 `instance_id` 后必须成功（修复验收）。
- name 为空、前后空白、大小写不匹配、同名 tool/asset 并存（验证 tool 优先与 `kind` 修复）。
- `tools.list`/`prompts.list`/`resources.list` 分别失败 → 区分“内部错误”与“未找到”。
- `prompts.get` 返回多 message / 非 TextContent → 内容是否丢失。
- 体积：大技能正文、含大 schema 的工具，统计返回字节数与重复字段占比。
- `includeNonHot=false` 时加载非 hot 工具。

## 9. 基线修正（仅当共通基线不成立时）

- 基线 5“契约 YAML 由 contract.Register 注册”：不适用于本插件；schema 由 `p.Tool(...)` 代码构造，且运行时无强校验。
- 基线 2“网关注入 `tool_call_display_name`”：成立；但它只注入到工具 schema，不进入 `name`/`uri` 的条目空间，`mcp_load` 不接受也不应接受该参数。
- 基线 4“路径规则”：本工具不接触文件系统，`!/`、`~/`、沙箱均不适用；`uri` 由宿主原语解析。
## 10. 修复记录（2026-11-08 / 2026-11-11）

- **2026-11-08（链路打通 + 契约化）**：契约迁移到 `src/tools/mcp/mcp_load.tool.md`；`skill/prompt` 走 `plugin/v1/prompts.get`、`resource` 走 `plugin/v1/resources.read`，均携带调用上下文的 `instance_id`（修复项目/用户级资产恒 not found）；`tool` 分支返回 `input_schema`；同名条目 tool 优先；详情读取失败明确报错。
- **2026-11-11（全量与错误区分）**：按名称查工具改为**全量**（`listTools(true)`），不再受 `plugins.mcp.include-non-hot` 检索过滤影响；列表读取失败与“未找到”区分——未命中且存在列表错误时返回“读取条目列表失败: …”，不再误报“未找到条目”。
- 仍为基线（未改）：`content` + `text` 双份返回、`prompts.get` 只取首条 message、名称大小写敏感无近似建议（契约已写明精确匹配）。
