---
plugin: mcp
tool: mcp_find
contract: programmatic
handler: src/main.go:124
reviewed_at: 2026-11-08
status: verified
---

# mcp_find 审查

## 1. 一句话结论

可用，但有风险：检索面是**全局注册表**而不是当前实例工具面，`limit` 静默截断且资产可能被工具挤没，`prompts/resources` 读取失败会被吞成“0 条”。

## 2. 调用链

LLM tool_call
→ llm-server 分发（落 running 行、建任务节点）
→ gateway `doCall` → mcp 插件 provider（origin=builtin，注入 `_meta.chonkpilot`）
→ 插件 MCP HTTP `tools/call` → SDK handler（`chonkpilot-framework/plugin/src/sdk/plugin.go:544-557`）
→ `mcpPlugin.find`（`src/main.go:124`）
→ `plugin/v1/tools.list` / `plugin/v1/prompts.list` / `plugin/v1/resources.list`
→ 桥 `forwardMQ`（非命令类 → `Emit().Wait()`，`framework/plugin/src/bridge/capabilities.go:222-236`）
→ gateway `handleToolsList` / `handlePromptsList` / `handleResourcesList`
→ 插件按 query/limit 过滤 → `mcpResult` 缩进 JSON（`sdk/plugin.go:613-621`）。

插件注册：`p.Tool("mcp_find", …)` + `p.HotTool`（`src/main.go:69-77,99-101`）；无 `.tool.md`，**schema 由代码构造，以 `src/main.go:70-76` 为准**。LLM 实际看到的 schema 还会被网关注入必填 `tool_call_display_name`（`chonkpilot-agent/gateway/src/registry.go:265-267`），代码 schema 里没有它。

## 3. 契约面

无 YAML。代码内 schema：

| 参数 | 类型 | required | 实现默认 | 说明 |
|---|---|---|---|---|
| `query` | string | 否 | `""` | 名称/描述的子串匹配，忽略大小写（`main.go:140,160-162`） |
| `type` | string | 否 | `tool` | tool/skill/prompt/resource/all；非法值报错（`main.go:349-358`） |
| `limit` | integer | 否 | 配置 `default-limit`（默认 20） | `>0` 覆盖；`<=0` 回落 20；硬顶 200（`main.go:130-139`；`config.go:41-46,85-87`） |

契约问题：
- 描述未写默认 20、上限 200、以及“超出即静默截断”。
- 描述说“skill 是 prompt 别名”，实现确实**不按 kind 过滤**（`main.go:361-371,406-420`），`type=skill` 会把 prompt 一起返回，反之亦然。
- 宿主注入的系统指引仍按旧内置 schema 描述本工具：`memory_guide.go:162` 与 `scenarios/default/main.agent.md:16` 都让模型传 `purpose`，但代码 schema 没有该参数（`main.go:70-76`），传了会被忽略。

插件配置（非 manifest 字段，`src/config.go:34-38`）：`plugins.mcp.default-limit`（1–200）、`plugins.mcp.include-non-hot`、`plugins.mcp.invoke-timeout-sec`；默认 `20 / true / 120`（`config.go:85-87`）。manifest 声明 `hot_tools` 3 个、`timeout_sec 600`、`category meta`、`namespace "-"`（`src/initdata/.../mcp/manifest.json:18-24`）。

## 4. 实现面

- 检索范围（第 1 项必核）：遍历 `listGroups(type)`（`main.go:143-156,361-371`）。
  - tool：`plugin/v1/tools.list`，载荷 `{}` → gateway 以 `instance_id` 为空走 `routesAll()`，返回**所有实例/所有 scope** 的已注册工具（`gateway/src/mcpgateway.go:596-606`）。包括：本插件自身三个工具、所有内置插件工具、用户/项目 capability dir 节点工具、第三方 MCP server 工具。
  - prompt/skill：`plugin/v1/prompts.list` → `handlePromptsList` 用 `eachPrimitive("", true)` 全量聚合（`primitives.go:64-97`），并并入 `reg.allAssets()` 的 prompt/skill 资产（`primitives.go:99-114`）。
  - resource：`plugin/v1/resources.list` 同样全量（`primitives.go:160-195`）。
  - 非 hot 工具：`_meta.hot` 为假时，只有 `IncludeNonHot=false` 才丢弃（`main.go:398-400`）；默认 `true`，所以**默认包含非 hot 工具**。
- query 空值：`query=""` 不过滤，列该类型全部（受 limit）。
- 排序与截断：工具按名称升序（`main.go:293`）；prompt/resource 保持宿主顺序；`type=all` 固定 `tool→prompt→resource`（`main.go:361-371`），在第一个达到 `limit` 处 `break`（`main.go:158-168`）。因此 `type=all` 时工具会先吃满额度，skill/resource 可能一条都拿不到。
- 截断无标记：返回只有 `count = len(out)`（`main.go:169`），没有 `total`/`truncated`/`next`；宿主侧也无分页。
- 失败路径不对称：`tools` 读取失败整体失败（`main.go:146-150,276-277`）；`prompts`/`resources` 读取失败被 `listItems` 吞为 nil（`main.go:323-327`），最终以 `count:0` 成功返回（`main.go:151-155`）。
- 体积：每条 entry 带完整 `input_schema`（`main.go:49,381-385`），`limit=200` 时可把 200 份 schema（含注入的展示名参数）塞进上下文；无字节上限。
- 无副作用（纯读），无并发/幂等/清理问题；每次调用最多 3 次宿主往返，均为同步等待（tools/prompts/resources 各一次）。

## 5. 返回面：LLM 实际看到的所有形态

宿主插件层：handler 返回 `(value,nil)` → `mcpResult` 缩进 JSON；返回 `error` → `mcpError(err.Error())` 纯文本 + `IsError=true`（`sdk/plugin.go:552-556,613-626`）。没有 `status`/`code` 字段，错误码只可能出现在文本里。

| # | 形态 | isError | LLM 可见内容 |
|---|---|---|---|
| A | 成功（有命中） | false | `{"count":N,"entries":[{name,kind,type,description,server?,hot?,uri?,mimeType?,input_schema?}],"ok":true,"type":"tool|skill|prompt|resource|all"}`（缩进 JSON，键按字母序） |
| B | 空结果 | false | `{"count":0,"entries":[],"ok":true,"type":…}`；无“没有资产/建议放宽 query”等提示 |
| C | `type` 非法 | true | `type 只允许 tool/skill/prompt/resource/all（收到 "x"）`（`main.go:357`） |
| D | 工具列表读取失败 | true | `读取工具列表失败: plugin/v1/tools.list: <detail>`（`main.go:276-277`；`sdk/plugin.go:316-325`） |
| E | prompt/resource 列表读取失败 | **false** | 视为该组 0 条，可能整体 `count:0`（`main.go:323-327`）——内部故障伪装成“没有资产” |
| F | 结果 JSON 编码失败 | true | `json 编码失败: <err>`（`sdk/plugin.go:617-620`） |
| G | 参数解析失败（arguments 非法 JSON） | true | `参数解析失败: <err>`（`sdk/plugin.go:546-549`） |
| H | 超时/取消（外层） | 由 llm-server 裁决 | gateway 600s / 异步裁决链，插件本身不返回该形态 |

与 file_read 的差异：没有 `results[]/summary` 外壳，也没有部分成功概念；`count` 不是总线数，不能据此判断“已穷尽”。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 检索面 = 全局注册表，不按当前实例/项目过滤 | `main.go:275`（`tools.list` 空载荷）；`gateway/src/mcpgateway.go:599-606`；`primitives.go:64`（`eachPrimitive("", true)`） | 模型发现跨项目/跨实例工具与资产；随后 `mcp_invoke` 可能被准入拒或路由到别的实例 |
| P1 | `limit` 静默截断，`count` 只是返回数 | `main.go:158-169`；`main.go:137-139` | 模型以为已穷尽；查不到时反复换 query；无法继续翻页 |
| P1 | `type=all` 固定 tool→prompt→resource，tools 先占满 limit | `main.go:143-156,361-371` | 大工具面上 skill/resource 永远检索不到（尤其默认 limit 20） |
| P1 | prompts/resources 读取失败被吞成 0 条 | `main.go:323-327,299-320` | 宿主/gateway 故障时返回“没有资产”，模型不会重试 |
| P2 | 每条 entry 带完整 `input_schema`（且含注入的必填展示名参数） | `main.go:49,381-385`；`registry.go:265-267` | `limit=200` 时 token 爆量；模型已知道展示名参数，属重复信息 |
| P2 | skill/prompt “同义”，不按 kind 过滤 | `main.go:361-371,406-420`；`main_test.go:106-109` | 与描述自洽，但 `type=skill` 仍返回 prompt，语义含糊 |
| P2 | 默认包含非 hot；`hot:false` 因 `omitempty` 不输出 | `config.go:86`；`main.go:46,398-400` | 模型无法区分“非 hot”与“宿主未上报 hot” |
| P2 | 宿主指引要求 `purpose`，schema 无此参数 | `memory_guide.go:162`；`main.agent.md:16`；`main.go:70-76` | 模型按指引传 `purpose` 被忽略，检索退化为关键词（内置实现曾支持语义挑选，`toolmeta.go:216-221`） |
| P2 | `type` 默认 `tool`，与旧内置口径 `all` 不一致 | `main.go:349-358`；`toolmeta.go:205-207` | 迁移后省略 type 只拿到工具，与模型记忆/文档不一致 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 把 `query` 与 `limit` 的作用域写进描述（默认 20、硬顶 200、超出即截断） | 改善 | 少猜、少重复检索 | 低（1 文件） | 无 | 现在做 |
| 返回增加 `total` 与 `truncated:true`（或 `nextLimit` 提示） | 增强 | 模型知道还能继续要，避免误判“没有” | 低（1 文件） | 非 breaking（只加字段） | 现在做 |
| `type=all` 改为按相关度/组内交错取，而非工具先占满 | 优化 | 资产可被发现 | 中 | 结果顺序变化 | 排期做 |
| `prompts/resources` 读取失败改为显式 `isError` 或 `partial:true` 字段 | 优化 | 故障可恢复 | 低 | 非 breaking（新增字段）；若改 isError 需同步前端/测试 | 现在做 |
| 增加 `includeSchema:false` 或默认只返回 `has_schema`，schema 由 `mcp_load` 单独取 | 改善 | 大幅省 token（find 阶段通常不需要完整 schema） | 中（1 文件 + 描述） | 返回字段减少；需兼容调用方 | 排期做 |
| 支持 `scope`/`server` 过滤，默认只检索本实例可见面 | 改善 | 少跨项目噪音，减少后续 invoke 失败 | 中（需传 `instance_id`，1–2 文件） | 行为变化，需同步文档 | 排期做 |
| 修宿主指引：删除 `purpose`，改为 `query`；明确 kind 语义 | 改善 | 消除“指引让传不存在的参数” | 低（2 处文案） | 无 | 现在做 |
| 统一 `type` 默认值为 `all`（与文档/旧口径一致）或明确写死 `tool` | 改善 | 消除歧义 | 低 | 默认行为变化 | 排期做 |

## 8. 建议的测试/验收（不执行）

- query 命中名称、命中描述、忽略大小写、空 query 列全量。
- `limit` 边界：不传（走配置）、0、负数、1、200、201、字符串数字、浮点。
- `type=all` + 小 limit：验证工具是否吃掉全部额度（记录现状并作为回归基线）。
- `tools.list` 失败 → isError；`prompts.list`/`resources.list` 失败 → 当前静默空（应改为有标记）。
- 非 hot 工具在 `includeNonHot=true/false` 下的收录与 `hot` 字段有无。
- 返回体积：limit=200 时统计字节数。
- 网关注入 `tool_call_display_name` 后，schema 仍可通过 MCP 校验（LLM 端必填项）。

## 9. 基线修正（仅当共通基线不成立时）

- 基线 2“每个工具 schema 注入 `tool_call_display_name`”：对 mcp 插件成立，但**该参数不在插件代码 schema 里**（`main.go:70-76`），审查契约时应以网关注入后的 schema 为 LLM 可见面。
- 基线 5“契约 YAML/schema 是 LLM 可见面”：本插件无 YAML，schema 由代码构造；且运行时不强校验（`additionalProperties` 未限制，网关也不校验参数），实际以 handler 的 `asString/toInt` 容错为准。
- 无需其他修正：异步/展示名/沙箱基线在本工具不产生额外分支（find 是同步只读）。
## 10. 修复记录（2026-11-08 / 2026-11-11）

- **2026-11-08（契约化 + 检索面）**：契约迁移到 `src/tools/mcp/mcp_find.tool.md`（schema/description/async/timeout 单源，`contract.Parse + MetaFor`）；`type=all` 改为 tool → prompt → resource **轮转取用**；新增 `match` 分词加权排序（名称 2 分/描述 1 分，不丢弃未命中）；返回 `total`/`truncated`；`limit` 硬顶 200、缺省值由 `plugins.mcp.default-limit` 控制；`skill` 与 `prompt` 共用来源并按条目自报类型区分；检索面“全局能力面”写入契约。
- **2026-11-11（错误面）**：`prompts.list` / `resources.list` 读取失败不再吞成 0 条，`mcp_find` 返回 `isError` + 明确错误；`tools.list` 提取抽成纯函数 `toolsFromResult` 并补过滤语义测试（`TestToolsFromResultFiltering` / `TestItemsFromResult`）。
- 仍为基线（未改）：跨实例检索面（契约已如实说明）、entry 携带完整 `input_schema`、`purpose` 指引由宿主文档侧改用 `query`/`match`。
