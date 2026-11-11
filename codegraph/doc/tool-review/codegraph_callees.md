---
plugin: codegraph
tool: codegraph_callees
contract: src/tools/codegraph/codegraph_callees.tool.md
handler: src/engines/treesitter/server/server.go（toolCallees）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_callees 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、resolved 无候选数、多命中静默首条、非函数 total=0、total 截断、id/file 语义不一致、limit 无上限等问题已修复。

## 1. 一句话结论

可用。目标支持 id 优先与 file+name（大小写不敏感）；多命中返回 `symbols` 候选；每个 callee 有 `candidates`（0=未索引/外部，1=resolved，>1=同名多处）；`total` 真实并支持 `limit/offset`；非函数目标明确 `not_callable`；参数严格校验、查询只读。

## 2. 调用链

1. LLM `codegraph_callees` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolCallees`。
3. `toolCallees`：
   - `validateCalleesArgs`（未知键/类型/id·file+name/limit·offset）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`）→
   - id 非空：`FindSymbol(id)`；否则 `FindSymbol("",file,name)`；未命中返回 `not_found` + 原因；
   - `CalleesOf(hits[0], limit, offset)`：遍历目标的 `Calls` → 每个名字查 `byName` → `resolved/candidates` → 分页 → total。
4. 成功/未命中为 JSON（`isError=false`）；参数错误 `isError=true` 文本。

## 3. 契约面

- 契约源 = `.tool.md`；schema/description/`_meta` 来自 md。
- schema：`additionalProperties:false`；`id` 优先；`limit` 1..500 default 50；`offset` >=0；file/name 大小写不敏感。
- 摘要：名字级启发式、被调名只留最后一段、candidates 语义、多命中候选、非函数 `not_callable`、calls 去重排序/无调用点/嵌套闭包不计、total/分页、workdir 规则。
- 返回值：成功示例（含 candidates）、空调用/非函数说明、门禁、参数错误。

## 4. 实现面

- **定位**：`FindSymbol` id 优先；file 精确、大小写不敏感；name 大小写不敏感全等。id 路径忽略 file/name。
- **目标候选**：`matches=len(hits)`、`symbol=hits[0]`、`symbols=hits`、`ambiguous=len>1`；callees 只针对首条。
- **调用列表**：`CalleesOf` 遍历 `target.Calls`（索引已去重/字典序）；每个名字取 `ix.byName[lower]`：
  - `candidates=0` → `resolved=false`（外部/未索引）；
  - `candidates=1` → `resolved=true`，回填 kind/file/line/id；
  - `candidates>1` → `resolved=false`（同名多处）。
- **分页**：`total` 为全部调用目标数，`returned` 当前页，`truncated = total > offset+returned`；`limit` 默认 50、上限 500。
- **空结果 note**：`not_callable`（目标 kind 非 func/method/constructor）、`no_calls`、`offset_beyond_total`；`callees:[]`。
- **not_found note**：`file_not_indexed` / `id_not_found` / `name_not_found`。
- **参数校验**：未知键/非字符串报错；id 为空且 file+name 不全报错；limit<1、offset<0、非整数报错；limit>500 截到 500。
- **workdir/只读**：`OpenReadOnly` 不创建 `.chonkpilot`；`EnsureReadyReadOnly` 不 Reconcile/不写盘；门禁文案不再指向不可见工具。
- **限制**：`CalleeRef.Name` 是最后一段（无 receiver/qualifier）；calls 无调用点行、无源码顺序；嵌套闭包/箭头函数的调用不计入外层。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有调用 | false | `{workdir,matches,ambiguous,symbol,symbols,total,returned,limit,offset,truncated,callees:[{name,resolved,candidates,…}]}` |
| 目标已定义但无调用 | false | `callees:[],note:"no_calls"` |
| 非函数目标 | false | `callees:[],note:"not_callable"` |
| 目标多命中 | false | `matches>1,ambiguous:true,symbols:[…]`；callees 只针对首条 |
| 翻页越界 | false | `returned:0,note:"offset_beyond_total"` |
| 未找到 | false | `{status:"not_found",note:"file_not_indexed|id_not_found|name_not_found",workdir,message}` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 解析仍是“全项目唯一同名” | `candidates` 现在能区分 0/1/>1，但没有 receiver/类型解析；真实项目 `New/Close/Run` 多数 candidates>1。需要更重的解析才能提升 resolved 率。 |
| P2 | 被调名只保留最后一段 | `a.Close()`/`b.Close()` 都成为 `Close`，receiver/qualifier 丢失；可后续在索引保存 qualifier。 |
| P2 | calls 语义有限 | 去重 + 字典序，不是源码调用顺序/次数；嵌套闭包/箭头函数内的调用不计入外层。契约已说明。 |
| P3 | 无调用点行号/顺序 | 模型需再读文件定位调用点。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | 全量复制符号做 byName 解析 | 单次查询 O(n)；大索引延迟。 |

已修（2026-11-10）：workdir 无校验/查询写盘、resolved 无候选数、多命中静默首条、非函数 total=0、total 截断/无分页、limit 无上限、id/file+name AND、file 大小写敏感、not_found 原因、`callees:null`、参数零校验、schema 未闭合、门禁文案指向不可见工具。

## 7. 测试/验收

- `TestToolCalleesTargetsAndCandidates`：total/分页、candidates 0/1/2、id 优先、目标多命中 candidates、`not_callable`、offset 越界、三种 not_found note、参数校验。
- 既有 `TestCallees`（by id/file+name/limit/not found/no calls/no index）与 `TestCallGraphTools`（callees by file+name/limit/not found）回归；`limit` 断言已更新为真实 `total + returned/truncated`。
- 回归：`go test -count=1 ./server/...` 通过。