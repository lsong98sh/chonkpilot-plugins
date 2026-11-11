---
plugin: task
tool: task_status
contract: src/tools/task/task_status.tool.md
handler: src/main.go:304
reviewed_at: 2026-11-08
status: verified
---

# task_status 审查

## 1. 一句话结论

可用但有风险：单任务查询语义清晰，但**`limit` 是无效参数**（任务列表/任务树返回对象而非数组，截断逻辑永不生效），任务树模式可无界灌入上下文；契约把 `tasks` 写成数组、把任务树写成 `{}`，与真实 `{nodes:[…]}`/`{list:[…]}` 不符。

## 2. 调用链

1. LLM `tool_call task_status` → turn 建节点/worker（`turn.go:1522-1578`）。
2. `gatewayArgs` 注入 `async=always`（`turn.go:342-358`）→ gateway `always` 档位立即 pending、后台执行、无超时点（`mcpgateway.go:1027-1037`）。
3. 插件 handler `taskPlugin.status`（`src/main.go:304-333`）：
   - 有 `task_id` → `statusOne`（`main.go:337-381`）→ 宿主能力 `plugin/v1/task.result`，500ms 轮询到终态或 `waitSec` 到期。
   - 无 `task_id` → `p.DataRead("task","tree")` + `p.DataRead("task","tasks")`（`main.go:319-326`）。
4. 数据面：`plugin/v1/data.read` → `data-tasktree-list` / `data-tasktree-tasks` → persist 同主题应答（`E:/BizWorks/chonkpilot-framework/plugin/src/bridge/plugins.go:215-266,273-328`；`E:/BizWorks/chonkpilot/src/lib/data/persist/tasktree.go:45-61`）。
5. 结果同 task_run，经 `mcp-tasks-report` 回执投喂 LLM（`turn.go:1257-1293`）。

## 3. 契约面

YAML：`async: never`、`timeout: 120`、`idempotent: true`（`task_status.tool.md:3-13`）。参数：`task_id`、`waitSec`、`limit`（均非必填，`task_status.tool.md:23-40`）。返回：单任务 `{ok,task_id,state,result|output}`；任务树 `{ok,limit,tree:{},tasks:[]}`（`task_status.tool.md:44-62`）。

契约问题：
- `limit` 描述“最多返回的任务条数”（`task_status.tool.md:37`）——实现**永不截断**（P1-1）。
- 任务树示例 `"tasks": []` 与实际 `{"list":[…]}` 不符；`"tree": {}` 过于含糊，实际 `{"nodes":[…]}`（P1-2）。
- 返回值未文档化：`state=not_found`、`message` 字段（`main.go:364-374`）、终态同时带 `result` 与 `output`、tree 模式的 `limit` 回显。
- 未说明 `task_id` 的解析顺序（任务节点 → gateway 执行 id → message 表 tool_call_id → 子轮次 turn），也未说明跨轮次/重启后的可见性限制。
- `async: never`/`timeout: 120` 未下发 gateway，实际 `always`（见 task_run §9），`waitSec` 无任何超时兜底。
- `waitSec` 无最小/最大值；`limit` 无最小/最大值。

## 4. 实现面

- 入口校验：`cx.InstanceID` 为空 → 错误（`main.go:305-307`）；单任务模式额外要求 `cx.Turn`（`main.go:338-340`）。
- 单任务：`waitSec>0` 计算 deadline；循环调用 `plugin/v1/task.result`；`state!="running"` 即返回；超时/`waitSec<=0` 返回 `running`+`message`+`output`；`!found` 返回 `state=not_found`（`main.go:341-380`）。
- 轮询间隔固定 500ms（`main.go:378`），未使用宿主已提供的**事件驱动** `plugin/v1/task.wait`（`plugin_caps.go:142-162`、`tool_task.go:160-183`）。
- 任务树：`limit` 取参数 → 配置 `plugins.task.status-limit`（默认 50，1–500）→ 50（`main.go:311-318`、`config.go:117`）。`tree`/`tasks` 直接 `unwrap` 后返回；`trimTasks` 只处理 `[]any`，而真实结构是 `map`（`main.go:327-332,384-390`）→ 截断恒不生效。
- 过滤口径：按 `session_id = cx.Session`（主会话看主树；子代理看自己的子会话），不按 `top_session` 聚合（`main.go:319-326`）。
- 只读、无副作用；`idempotent:true` 正确。
- 资源：`waitSec` 越大轮询越久（2 次/s 桥调用）；`limit` 无上限校验，但当前也不生效。

## 5. 返回面：LLM 实际看到的所有形态

> 生产链路强制 `async=always`，以下 JSON 均由回执投喂；任务态恒为 `done`（插件不置 MCP `isError`），故 LLM 侧 `isError` 位不可靠。

A. 单任务终态 `done`：
`{"ok":true,"task_id":"tk-…","state":"done","result":"<节点 ResultSummary 全文>","output":"<partial，通常为空>"}`（`main.go:367-370`）。

B. 单任务 `error`：`{"ok":true,"task_id","state":"error","result":"任务失败: <节点 Error>","output":…}`（`session/task_tools.go:62-73`；`main.go:367-370`）。注意 `ok` 仍为 `true`。

C. 单任务 `cancelled`：`{"ok":true,"task_id","state":"cancelled","result":"任务已取消: <节点 Error>","output":…}`。

D. 运行中（`waitSec<=0` 或等待到期）：`{"ok":true,"task_id","state":"running","message":"任务仍在运行（可加大 waitSec 继续等待）","output":"<已产出部分>"}`（`main.go:371-374`）。

E. 未找到：`{"ok":false,"task_id","state":"not_found","message":"未找到任务 <id>"}`；不是 isError，LLM 需读 `ok`（`main.go:363-366`）。

F. 任务树模式：`{"ok":true,"limit":50,"tree":{"nodes":[<节点行>]},"tasks":{"list":[<任务行>]}}`（`main.go:327-332`；`E:/BizWorks/chonkpilot/src/lib/data/facade/wire/domains.go:217-244`）。节点行含 `task_id/title/status/state/tool_call_id/created_at/…`，**不含 result/error 文本**，要拿错误详情需再按 `task_id` 单查。

G. 空结果：`{"ok":true,"limit":N,"tree":{"nodes":[]},"tasks":{"list":[]}}`（`omitempty` 可能让字段缺失，取决于序列化侧）。

H. “异步结果已不可用”：单查命中层判终态但 message 表无结果行时，`result=异步结果已不可用（任务已终态，但 message 表无该调用的结果行）`、`state=error`（`tool_task.go:282-305`；`session/task_tools.go:57-58`）。

I. 内部错误（能力/数据面失败、turn 不存在）：`查询任务失败: <err>` / `读取任务树失败: <err>` / `读取任务列表失败: <err>`，裸文本 isError（`main.go:320-326,360-362`）。

J. 查询过程被取消（用户停止/父轮次取消）：插件侧 `ctx.Err()` 上抛；若已被 gateway 判取消，LLM 可能看到 `任务结束(cancelled)`（`main.go:375-377`；`turn.go:1275-1277`）。

K. 传输层级失败（provider 断开/transport error）：`任务结束(error)`（详情丢失，见 task_llm P1-2）。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | **`limit` 是无效参数**：`tasks`/`tree` 为对象（`{list}`/`{nodes}`），`trimTasks` 只认 `[]any` → 永不截断；返回里的 `limit` 具有误导性 | `main.go:327-332,384-390`；`wire/domains.go:217-244`；`jsonval.Unwrap`（`E:/BizWorks/chonkpilot-util/jsonval/jsonval.go:91-108`） | 任务树可无界灌入上下文，token 爆量且模型以为已截断 |
| P1 | 任务树返回形状与契约不一致（`tasks` 数组 vs `{list}`，`tree` `{}` vs `{nodes}`） | `task_status.tool.md:55-62`；`main.go:327-332` | 模型按契约解析必然失败，需二次猜测 |
| P1 | `waitSec` 无任何兜底上限且 gateway `always` 档位无超时点；单任务等待会长期占住本轮 | `main.go:341-345,375-380`；`mcpgateway.go:1027-1037`；契约 schema 30-34 | 误填大 `waitSec` 即长时间挂起，用户只能停止整轮 |
| P2 | `state=not_found`、`message`、终态双字段 `result+output` 未文档化 | `main.go:364-374`；契约 44-53 | 模型需容错 |
| P2 | 单任务 `error/cancelled` 仍 `ok:true`；插件 isError 在异步回执中被压成任务 `done` | `main.go:367-370`；`turn.go:1254-1289` | `ok` 与 `isError` 语义不可靠 |
| P2 | 用 500ms 轮询而非宿主已有的事件驱动 `plugin/v1/task.wait` | `main.go:375-379`；`plugin_caps.go:142-162` | 每次等待 2 次/秒桥调用，浪费且延迟 |
| P2 | 任务树只按 `session_id` 过滤，子代理看不到主树聚合（与“当前会话”措辞一致，但未说明子代理行为） | `main.go:319-326` | 模型在子会话里可能以为整棵树为空 |
| P2 | 节点树行不含 result/error，而契约未提示“要详情需按 task_id 再查” | `wire/domains.go:32-66` | 多一次往返，模型易放弃 |
| P2 | `waitSec`/`limit` 无 range 校验，`limit` 传入超大值仍回显 | `main.go:312-318`；schema 27-38 | 参数误导 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 截断真正生效：从 `tasks.list` / `tree.nodes` 取数组切片，并返回 `total`/`truncated` | 优化 | 可控 token、语义自洽 | 低，`main.go:327-332,384-390` | 返回结构从对象改为“对象+数组切片”，需同步契约 | 建议现在做（P1 必修） |
| 契约改为真实形状：`tasks:{list:[…]}`、`tree:{nodes:[…]}`，并写明节点行字段 | 改善 | 模型一次解析成功 | 低，契约 | 文本层 | 建议现在做 |
| `waitSec` 加 `maximum:120`（与契约 timeout 对齐），并说明“到期返回 running” | 改善 | 不再误挂整轮 | 低，契约/`main.go:341` | 无 | 建议现在做 |
| 改用宿主 `plugin/v1/task.wait(timeout≤60)` 事件驱动，替代 500ms 轮询 | 优化 | 延迟 ↓、桥调用 ↓ | 低，`main.go:341-380` | 行为等价 | 排期做 |
| 单任务终态统一带 `isError` 语义（error/cancelled 且 ok:false）或改名为 `status` 明确 | 改善 | 模型判定稳定 | 低，`main.go:367-370` | 返回字段语义变化 | 排期做 |
| 增加 `state` 枚举说明（含 `not_found`）与 `message` 说明 | 改善 | 少猜 | 低 | 无 | 建议现在做 |
| 支持 `task_id` 省略 + `waitSec`（等任意任务变化）——可选增强 | 增强 | 减少轮询调用 | 中 | 新增语义 | 仅记录 |

## 8. 建议的测试 / 验收（不执行）

1. 造 200 个任务，`limit=10` → 断言 `tasks.list` 长度=10 且带 `total`（当前断言失败，复现 P1-1）。
2. 断言 tree 模式返回 `tree.nodes` / `tasks.list` 与契约一致。
3. 单任务 done/error/cancelled/running/not_found 五态字段逐项断言。
4. `waitSec=2` 对 running 任务 → 断言约 2s 返回 running+message+output。
5. `waitSec` 超大（如 3600）→ 断言有上限保护或明确拒绝。
6. 目标任务在本轮结束时被取消 → 断言 cancelled 文本与 `ok` 语义。
7. 空会话任务树 → 断言结构稳定（不是 `null`）。
8. 重启后按 task_id 查询 → 断言返回“异步结果已不可用”而非空串。
9. 子会话（委派子代理）调用 tree → 断言只看到本子会话节点。
10. 数据面不可达 → 断言 `读取任务树失败: …` 文本包含可诊断原因。

## 9. 基线修正（仅当共通基线不成立时）

- **基线 6 不成立**：`async: never`/`timeout: 120` 未下发 gateway；实际 `always` 且无超时点，故本工具不存在 `mcp-tools-timeout` 裁决面。
- **基线 8 的文件域 `{results[],summary}` 约定不适用**：本工具返回 `{ok,limit,tree,tasks}`；且 `ok` 不表示全部成功（`not_found` 时 `ok:false` 仍属正常业务结果）。