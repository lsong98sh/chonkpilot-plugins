---
plugin: codegraph
tool: codegraph_symbol_search
contract: src/tools/codegraph/codegraph_symbol_search.tool.md
handler: src/engines/treesitter/server/server.go（toolSymbolSearch）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_symbol_search 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、`total` 截断、`kind` 大小写、`limit` 无上限、schema 未闭合等问题已修复。

## 1. 一句话结论

可用。`total` 为真实命中总数，支持 `limit/offset` 分页与排序后截断；`kind` 大小写不敏感且非法值报错；空结果带 `note`；workdir 从 `_meta` 注入/校验，查询路径只读不建目录、不 Reconcile。剩余主要是状态类应答的 isError 语义与全量扫描性能。

## 2. 调用链

1. LLM `codegraph_symbol_search` → gateway（`async:auto`、`timeout:30`）→ 插件 MCP。
2. `registerSpecs` 的查询工具闭包：
   - 解析 args → `ResolveWorkdirFromMeta`（缺省取 `_meta.chonkpilot.work_dir`；显式 workdir 必须绝对、不得 `~`、不得越出当前项目）→
   - `toolSymbolSearch`：`validateSymbolSearchArgs`（未知键/类型/kind/limit/offset）→ `readyGate` →
   - `readyGate`：`gateIndexStatus`（status.json 有界等待 5s）→ `OpenReadOnly`（不 MkdirAll）→ `EnsureReadyReadOnly`（不 Reconcile/不写盘）。
3. `Workspace.SearchSymbolPage`：遍历全部符号 → 过滤（kind/file/query）→ 按 `file/line/name` 排序 → `offset/limit` 切片，返回 `total` 与当前页。
4. 成功为 JSON 文本（`isError=false`）；参数错误为 `isError=true` 文本。

## 3. 契约面

- 契约源 = `.tool.md`（`src/tools/codegraph/codegraph_symbol_search.tool.md`），描述/schema/`_meta` 全部来自 md。
- schema：`additionalProperties:false`；`kind` enum 8 类；`limit` 1..500 default 50；`offset` >=0；`workdir` 可选（meta 注入）。
- 摘要：真实 `total`、`returned`、`truncated`、`limit/offset`、空结果 `note`（`no_match|file_not_indexed|index_empty|offset_beyond_total`）、索引门禁状态、workdir 规则。
- 返回值：成功 JSON、空结果示例、`status=not_initialized|pending|error` 门禁、参数错误 `isError=true`。

## 4. 实现面

- **参数校验**：未知键显式失败；字符串字段类型检查；`kind` 去空格转小写并校验 8 类；`limit` 缺省 50、<1 报错、>500 截到 500；`offset` <0 报错。非整数/字符串数字报错。
- **查询语义**：
  - `query` 空 = 不按名字过滤；`file` 为路径子串（相对/绝对任一命中）；`kind` 大小写不敏感。
  - **先收集并排序全部命中，再分页**；`total` 为未截断总数，`returned` 为当前页条数，`truncated = total > offset+returned`。
- **空结果诊断**：返回 `symbols:[]`（非 null）、`indexed_files` 与 `note`：
  - `offset_beyond_total`：翻页越界（total>0 且 offset>=total）；
  - `index_empty`：索引文件数为 0；
  - `file_not_indexed`：file 过滤未命中任何已索引文件（含语言不支持）；
  - `no_match`：其它无命中。
- **workdir/只读**：查询路径 `OpenReadOnly` 不创建 `.chonkpilot`；`EnsureReadyReadOnly` 不 Reconcile、不写索引；索引新鲜度由后台维护。
- **门禁**：`gateIndexStatus` 对 indexing/pending 最多等待 5s（100ms 轮询）；未初始化/构建中/错误返回结构化 `status`；错误文案不再指向 LLM 不可见的 `codegraph_initialize`。
- **限制**：`limit` 500 上限；`SearchSymbolPage` 每次复制并排序全部符号（O(n)），大仓库有 CPU/内存成本，但不再按索引序提前截断。
- 无 context 取消（handler 忽略 ctx）；索引门等待与全量遍历可能超过 30s，gateway 会转后台。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有命中 | false | `{total,returned,limit,offset,truncated,indexed_files,symbols:[…]}` |
| 无命中 | false | 同上，`symbols:[]` + `note:"no_match"` |
| 文件未索引/语言不支持 | false | `symbols:[]` + `note:"file_not_indexed"` |
| 索引为空 | false | `symbols:[]` + `note:"index_empty"`, `indexed_files:0` |
| 翻页越界 | false | `symbols:[]` + `note:"offset_beyond_total"` |
| 未初始化 / 构建中 / 索引错误 | false | `{status:"not_initialized|pending|error", state, message}`（按 status 处理） |
| 参数非法（未知键/类型/kind/limit/offset） | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本（`ResolveWorkdirFromMeta` 返回） |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 门禁状态 `not_initialized/pending/error` 仍 `isError=false` | 契约已明确按 `status` 处理；若希望客户端按错误重试，可把 `error` 分支改为 isError。属跨查询工具的统一取舍。 |
| P2 | 全量复制 + 排序所有命中 | `total` 真实值需要遍历全部符号；大索引（数十万符号）单次查询 CPU/内存较高。可后续加按名索引预筛或服务端缓存。 |
| P2 | 索引维护工具对 LLM 不可见 | `codegraph_initialize/status` 仍 hot=false；本工具只能用 `status=not_initialized` 提示“请用户初始化”，无法自愈。 |
| P3 | `indexed_files` 只给文件数，不给语言分布/最后索引时间 | 诊断仍有限；可复用 status 的字段。 |
| P3 | 无 `query` 时返回排序后的任意符号 | 契约已说明；若误用为“列全部符号”，仍可能拉入大结果（受 limit 500 限制）。 |

已修（2026-11-10）：旧审查 P0 workdir 无校验/查询写盘（已由 `ResolveWorkdirFromMeta` + `OpenReadOnly` 修复）、`total` 截断、无分页、`kind` 大小写、非法 kind 静默 0、`limit` 无上限、`symbols:null`、schema 未闭合/无 enum、空结果不可区分、错误文案指向不可见工具。

## 7. 测试/验收

- `TestSearchSymbolPage`：分页 total、offset 越界返回空数组、kind 大小写、`HasIndexedFile`/`IndexedFileCount`。
- `TestValidateSymbolSearchArgs`：默认值、limit 上限截到 500、未知键、非法 kind/类型/limit/offset。
- `TestToolSymbolSearchPaginationAndNotes`：handler 分页/truncated、`kind:"FUNC"`、`file_not_indexed`/`no_match`/`offset_beyond_total`、非法 kind/未知参数。
- 已有 `TestSearchSymbol`、`TestWorkdirFromMeta`、`TestWorkdirValidation`、`TestContractMetaListed` 回归。
- 回归：`go test -count=1 ./...`（server 包）。