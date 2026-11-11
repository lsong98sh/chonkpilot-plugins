---
plugin: codegraph
tool: codegraph_analyze_complexity
contract: src/tools/codegraph/codegraph_analyze_complexity.tool.md
handler: src/engines/treesitter/server/server.go:1125（toolComplexity）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_analyze_complexity 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 description/实现 kind 不一致、topN 无上限、`topN` 字段同名不同义、排序不稳定、`symbols:null`、参数零校验、workdir 无绑定/查询写盘、未就绪文案指向不可见工具等问题已修复。

## 1. 一句话结论

可用。默认只返回 **func/method/constructor**（与描述一致），`kind=all` 才纳入 type/class 等；`topN` 缺省 20、硬上限 200，返回真实 `total` 与 `truncated`。排序确定（复杂度→文件→行→名称→kind），空结果为 `[]`，参数严格校验、查询只读。复杂度已剪枝嵌套函数/闭包（不计入外层），但仍与 gocyclo 口径不同；响应带 `coverage`（已索引/解析报错文件数）。

## 2. 调用链

1. LLM `codegraph_analyze_complexity` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolComplexity`。
3. `toolComplexity`：
   - `validateComplexityArgs`（未知键/类型/topN·minCc/kind）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`）→
   - `w.TopComplexityPage(file, kind, topN, minCc)`：全符号扫描 → file 子串/kind/复杂度过滤 → 稳定排序 → 截断 → 返回 `(page, total)`；
   - 返回 `{workdir,topN,returned,total,truncated,minCc,kind,metric,symbols[,note]}`。
4. 参数错误 `isError=true` 文本；门禁状态 `isError=false` 的 `status` 对象。

## 3. 契约面

- 契约源 = `codegraph_analyze_complexity.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`additionalProperties:false`；`kind` enum `function/func/method/constructor/all` default `function`；`topN` 1..200 default 20；`minCc` >=1 default 1。
- 摘要：复杂度口径（1+决策点、不计 `&&`/`||`、switch case 计数、嵌套函数计入外层的限制）、`kind` 语义、file 为**大小写不敏感子串**、`total`/`truncated`、稳定排序、已索引范围、workdir 规则。
- 返回示例、空结果 `no_matches` / `file_not_indexed`、门禁与参数错误。

## 4. 实现面

- **参数**：`workdir`/`file`/`kind` 必须字符串；`kind` 归一为小写，空值默认 `function`，非法值报错；`topN` 缺省 20、<1 报错、>200 截到 200；`minCc` 缺省 1、<1 报错；未知键报错。
- **过滤**：`complexityKindMatch`：`all`/空不过滤；`function` = func/method/constructor；其余按 kind 全等（大小写不敏感）。file 用 `norm(file)` 的小写相对路径或原始绝对路径做子串匹配。
- **取数/排序**：`AllSymbols()` 全量扫描 → 过滤 → `sort.Slice` 按复杂度降序、文件、行号、名称、kind；`total` 为截断前匹配数。
- **复杂度**：`complexityOf` 以定义节点为根遍历，遇到内层定义/匿名作用域即剪枝（与 `callsWithin` 同口径），嵌套函数/闭包的决策点不计入外层。
- **覆盖信息**：`IndexCoverage` 返回已索引文件数与 `HasErr`（解析报错）文件数，响应带 `coverage{indexed_files,parse_error_files}`。
- **返回**：`topN` = 生效的请求值（已截到 200）；`returned` 本页数量；`truncated = total > returned`；`metric:"cyclomatic"`；`symbols` 空为 `[]`。
- **notes**：`total==0` 时 `file_not_indexed`（file 非空且索引无匹配路径）或 `no_matches`。
- **只读门禁**：`readyGate` 使用 `OpenReadOnly` + `EnsureReadyReadOnly`，不 `MkdirAll`、不 Reconcile、不写索引。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 成功 | false | `{workdir,topN,returned,total,truncated,minCc,kind,metric,symbols:[…],coverage:{indexed_files,parse_error_files}}` |
| 空结果 | false | `returned:0,total:0,symbols:[],note:"no_matches"` |
| file 未命中 | false | `…,note:"file_not_indexed"` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 口径与 gocyclo 不同 | 不计 `&&`/`||`，但 switch/default case 各计 1；跨工具数值对不上，已声明“用于相对排序”。如需对齐需产品确认口径（会改变所有数值）。 |
| P2 | 部分文件不可见 | >8MB、解析失败、被排除的文件不入索引；响应已给 `coverage.indexed_files/parse_error_files`，但 >8MB/被排除的文件仍无法从结果判断。 |
| P3 | file 为子串匹配 | `a.go` 会命中 `a_test.go`；契约已明确为子串过滤，不再暗示单文件精确匹配。 |
| P3 | `topN` 名称非 `limit` | 保留旧名以兼容；响应已有 `returned`/`total`/`truncated` 消除歧义。 |
| P3 | handler 忽略 ctx | 取消后仍会完成全量扫描；topN 上限限制了下游输出。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | `enabled=false` 不阻止查询 | 与配置页文案不一致；跨工具统一问题。 |

已修（2026-11-10）：description/实现 kind 不一致、topN 无上限、`topN` 字段同名不同义、排序不稳定、空结果 `null`、字符串数字静默回落、minCc 语义未声明、file 大小写敏感/未说明子串、workdir 无绑定/查询写盘、未就绪文案指向不可见工具、schema 未闭合/零校验、嵌套函数决策点计入外层（已剪枝）、缺覆盖信息（已加 coverage）。

## 7. 测试/验收

- `TestComplexityPrunesNestedFunctions`：闭包内决策点不计入外层（complexity=2）、`coverage` 字段。
- `TestToolComplexityFiltersAndCap`：默认只含函数类、`kind=all/method`、topN 截断与 `total`/`truncated`、topN 上限 200、稳定排序、minCc 空结果、file 子串/未命中、coverage、严格参数（未知键/0/非法 kind/类型/小数）。
- 既有 `TestTopComplexity`（兼容接口：top3/minCc/file/no index）回归。
- 回归：`go test -count=1 ./server/...` 通过。