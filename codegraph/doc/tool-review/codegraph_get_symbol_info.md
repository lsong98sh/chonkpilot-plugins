---
plugin: codegraph
tool: codegraph_get_symbol_info
contract: src/tools/codegraph/codegraph_get_symbol_info.tool.md
handler: src/engines/treesitter/server/server.go（toolGetSymbolInfo）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_get_symbol_info 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、多命中只回第一条、file 大小写、id 与 file+name AND、not_found 不可区分等问题已修复。

## 1. 一句话结论

可用。id 优先且大小写不敏感；file+name 精确定位支持相对/绝对、大小写不敏感；多命中返回 `symbol` + `symbols[]` + `ambiguous`；not_found 带 `note`；参数严格校验；workdir 从 `_meta` 注入/校验，查询只读。

## 2. 调用链

1. LLM `codegraph_get_symbol_info` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta`（缺省取 `_meta`；显式 workdir 必须绝对、非 `~`、不越界）→ `toolGetSymbolInfo`。
3. `toolGetSymbolInfo`：
   - `validateGetSymbolInfoArgs`（未知键/类型/定位参数）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`）→
   - `FindSymbol(id,file,name)`：id 非空只按 id；否则 file 精确（大小写不敏感）+ name 大小写不敏感全等 →
   - 返回候选或 `not_found`。
4. 成功/未命中均为 JSON 文本（`isError=false`）；参数错误 `isError=true` 文本。

## 3. 契约面

- 契约源 = `.tool.md`，schema/description/`_meta` 来自 md。
- schema：`additionalProperties:false`；`workdir` 可选；`id` 提供时忽略 file/name；否则必须 file+name。
- 摘要：file 为精确路径（绝对/相对、大小写不敏感）；name 大小写不敏感；多命中返回全部候选；`calls` 仅函数类；`signature` 可能截断；not_found `note`；workdir 规则。
- 返回值：成功示例（含 `matches/symbol/symbols/ambiguous/workdir`）、not_found 示例、门禁状态、参数错误。

## 4. 实现面

- **id 优先**：`id != ""` 时忽略 file/name，按 `s.ID` 与 `w.norm(id)` **大小写不敏感**比较；绝对/相对 id 都可。
- **file+name**：file 经 `norm` 归一后与 `s.File` **大小写不敏感精确**比较（Windows 路径大小写不再误判）；name `EqualFold`。
- **候选**：`matches = len(hits)`；`symbol = hits[0]`（兼容旧返回）；`symbols = hits`（全部候选）；`ambiguous = matches>1`。排序 = file/line/name。
- **not_found note**：
  - `file_not_indexed`：file 未命中任何已索引文件（含语言不支持）；
  - `id_not_found`：id 路径未命中；
  - `name_not_found`：file 已索引但 name 不存在。
- **参数校验**：未知键/非字符串显式失败；缺 id 且 file+name 不全失败。
- **workdir/只读**：`OpenReadOnly` 不创建 `.chonkpilot`；`EnsureReadyReadOnly` 不 Reconcile、不写盘。
- **门禁**：`status=not_initialized|pending|error` 结构化返回（`isError=false`）；错误文案不再指向 LLM 不可见的 `codegraph_initialize`。
- **限制**：`FindSymbol` 每次复制全部符号做线性匹配（O(n)）；候选数取决于同文件同名符号数，无额外上限。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 唯一命中 | false | `{matches:1,ambiguous:false,workdir,symbol,symbols:[1]}` |
| 多命中 | false | `{matches:N,ambiguous:true,workdir,symbol:首条,symbols:[N]}` |
| id 未命中 | false | `{status:"not_found",note:"id_not_found",workdir,message}` |
| 文件未索引/语言不支持 | false | `{status:"not_found",note:"file_not_indexed",workdir,message}` |
| file 已索引但 name 不存在 | false | `{status:"not_found",note:"name_not_found",workdir,message}` |
| 未初始化 / 构建中 / 索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法（未知键/类型/缺定位参数） | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 门禁 `not_initialized/pending/error` 仍 `isError=false` | 契约已按 `status` 处理；跨查询工具的统一取舍。 |
| P2 | 线性扫描全部符号 | 单符号详情也要 `AllSymbols()` 复制+遍历；大索引延迟高。可后续用 id/file 索引。 |
| P3 | 候选数量无上限 | file+name 精确匹配下通常很小；若重载/同名极多，`symbols[]` 可能较大。可加 `limit`。 |
| P3 | id 仍依赖 workdir 归一 | 跨项目复用 id 不命中，会返回 `id_not_found`；契约已说明 id 来自当前项目搜索。 |
| P3 | 无 `indexedAt`/语言分布 | 结果归因只有 workdir；诊断能力有限。 |

已修（2026-11-10）：workdir 无校验/查询写盘（由 `ResolveWorkdirFromMeta` + `OpenReadOnly` 修复）、多命中仅首条、file/id 大小写敏感、id 与 file+name AND、not_found 原因、参数零校验、schema 未闭合、错误文案指向不可见工具。

## 7. 测试/验收

- `TestValidateGetSymbolInfoArgs`：id 参数保留、缺 name 报错、未知键/类型错误/全缺失。
- `TestToolGetSymbolInfoCandidatesAndNotes`：id 优先（忽略错误 file/name）、file+name 多命中 `symbols[]`/`ambiguous`、file 大小写不敏感、name 大小写不敏感、三种 not_found note、参数错误。
- `TestFindSymbol`：id/file+name/未命中/空索引回归。
- 回归：`go test -count=1 ./server/...` 通过。