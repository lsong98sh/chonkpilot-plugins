---
plugin: codegraph
tool: codegraph_callers
contract: src/tools/codegraph/codegraph_callers.tool.md
handler: src/engines/treesitter/server/server.go（toolCallers）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_callers 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、id 优先级、total 截断、目标身份缺失、name 未命中不可区分、limit 无上限等问题已修复。

## 1. 一句话结论

可用。id 真正优先；`total` 为真实调用方总数并支持 `limit/offset`；返回目标符号身份与候选数；name 未定义/无调用方/翻页越界均有 `note`；参数严格校验；查询只读。

## 2. 调用链

1. LLM `codegraph_callers` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolCallers`。
3. `toolCallers`：
   - `validateCallersArgs`（未知键/类型/id·name/limit·offset）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`）→
   - id 非空：`FindSymbol(id)`，未命中返回 `not_found/id_not_found`；否则 `target_symbol = hits[0]`、`target = hits[0].Name`；
   - name 路径：`target = name`，`FindByName(name)` 回填目标定义（可为 0 = 外部/未定义）；
   - `CallersPage(target,file,limit,offset)`：`byCall` 取候选 → file 过滤 → `anyCallMatches` → 排序 → 分页 → 返回 total。
4. 成功/未命中为 JSON（`isError=false`）；参数错误 `isError=true` 文本。

## 3. 契约面

- 契约源 = `.tool.md`；schema/description/`_meta` 来自 md。
- schema：`additionalProperties:false`；`id` 优先；`limit` 1..500 default 50；`offset` >=0；`file` 子串。
- 摘要：名字级启发式、跨语言同名会计入、callers[] 是调用方**定义**（line=定义行）、target 身份字段、空结果 note、workdir 规则。
- 返回值：成功示例、空调用方示例、门禁状态、参数错误。

## 4. 实现面

- **id 优先**：`id != ""` 时忽略 name，`FindSymbol(id,"","")` 解析目标；未命中直接 `not_found`。
- **目标身份**：id 路径 `target_symbol=命中符号`、`target_matches=1`；name 路径 `FindByName`（全名/最后一段，大小写不敏感）返回全部定义 → `target_symbol=首条`、`target_matches=N`、`target_ambiguous=N>1`；未定义时 `target_defined=false`。
- **召回**：`callLookupKeys(target)`（全名 + 最后一段）→ `ix.byCall` 并集；file 子串过滤；`anyCallMatches` 二次确认（全等/尾段/`.target` 后缀）；跨语言同名会命中。
- **分页**：先收集全部调用方，按 `file/line/name` 排序，再 `offset/limit` 切片；`total` 未截断、`returned` 当前页、`truncated = total > offset+returned`。
- **空结果**：`callers:[]` + `note`：
  - `offset_beyond_total`；
  - `target_not_indexed`（target_defined=false）；
  - `no_callers`（目标已定义但无调用）。
- **参数校验**：未知键/非字符串报错；id/name 都缺报错；limit<1、offset<0、非整数报错；limit>500 截到 500。
- **workdir/只读**：`OpenReadOnly` 不创建 `.chonkpilot`；`EnsureReadyReadOnly` 不 Reconcile/不写盘；门禁文案不再指向不可见工具。
- **限制**：`CallersPage` 复制全量符号并排序匹配集（O(n)）；`callers[].line` 是定义行，不是调用点。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有调用方 | false | `{workdir,target,target_defined,target_matches,target_symbol?,target_ambiguous?,total,returned,limit,offset,truncated,callers:[…]}` |
| 目标已定义但无调用方 | false | `total:0,callers:[],note:"no_callers"` |
| 目标未定义（外部/动态） | false | `target_defined:false,target_matches:0,note:"target_not_indexed"` |
| id 未命中 | false | `{status:"not_found",note:"id_not_found",workdir,message}` |
| 翻页越界 | false | `returned:0,note:"offset_beyond_total"` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 门禁 `not_initialized/pending/error` 仍 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P2 | 跨语言同名会计入 | 名字级索引无语言/类型解析；契约已明确，可用 file 缩小。可后续加 `lang` 过滤。 |
| P2 | `file` 是子串匹配 | `a.go` 也会命中 `data.go`；契约已声明。若需精确，后续加 `fileExact`/路径段匹配。 |
| P3 | 无调用点行号 | `callers[].line` 是调用方定义行；调用点位置未入索引，模型需再读文件。 |
| P3 | 无 `resolved/confidence` | 名字级启发式无法量化可信度；契约已声明无类型/重载解析。 |
| P3 | 全量扫描 | `CallersPage` 复制全量符号并排序；大索引延迟。 |

已修（2026-11-10）：workdir 无校验/查询写盘（`ResolveWorkdirFromMeta`+`OpenReadOnly`）、id 被 name 覆盖、`total` 截断、无分页/limit 上限、目标身份缺失、name 未命中与无调用方不可区分、`callers:null`、参数零校验、schema 未闭合、门禁文案指向不可见工具。

## 7. 测试/验收

- `TestToolCallersPaginationAndTargetIdentity`：total/truncated、target_symbol/target_matches、id 优先（name 指向别的符号）、file 过滤、offset 翻页/越界、no_callers/target_not_indexed/id_not_found、参数校验。
- 既有 `TestToolCallers`（name 路径、跨语言同名、id not_found）回归。
- 回归：`go test -count=1 ./server/...` 通过。