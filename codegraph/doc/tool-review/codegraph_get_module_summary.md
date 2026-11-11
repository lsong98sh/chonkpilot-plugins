---
plugin: codegraph
tool: codegraph_get_module_summary
contract: src/tools/codegraph/codegraph_get_module_summary.tool.md
handler: src/engines/treesitter/server/server.go:1200（toolModuleSummary）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_get_module_summary 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 path 纯字符串前缀/大小写敏感、topComplexity5 含非函数、排序不稳定、scope 空前缀为空串、统计口径未声明、workdir 无绑定/查询写盘、未就绪文案指向不可见工具等问题已修复。

## 1. 一句话结论

可用，且是本组返回体积最可控的工具（固定 ≤5 条热点 + 语言计数）。`path` 按路径段边界且大小写不敏感匹配（不再吞 `srcfoo`）；`topComplexity5` 只含 func/method/constructor 并稳定排序。摘要口径、`parseErrorFiles`/`lastIndexedAt` 已在契约声明；空 `path` 的 `scope` 返回 workdir。

## 2. 调用链

1. LLM `codegraph_get_module_summary` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolModuleSummary`。
3. `toolModuleSummary`：
   - `validateModuleSummaryArgs`（仅 workdir/path 且必须字符串）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`）→
   - `w.ModuleSummary(path)`：按路径段过滤已索引文件 → 汇总 files/symbols/langCounts/parseErrorFiles → 函数类符号稳定排序取前 5 → `lastIndexedAt`；
   - 追加 `workdir`；`files==0 && path!=""` 时 `note:"path_not_indexed"`。
4. 参数错误 `isError=true` 文本；门禁状态 `isError=false` 的 `status` 对象。

## 3. 契约面

- 契约源 = `codegraph_get_module_summary.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`additionalProperties:false`；`workdir`/`path` 字符串。
- 摘要：path 路径段边界 + 大小写不敏感、`files` 是已索引文件数（非仓库总数）、`symbols` 含全部符号类别、`topComplexity5` 只含函数类且稳定排序、`langCounts` 以语言名为键、`parseErrorFiles`/`lastIndexedAt`、空 path 的 scope=workdir、workdir 规则。
- 返回示例、范围内无文件 `path_not_indexed`、门禁与参数错误。

## 4. 实现面

- **参数**：只允许 `workdir`/`path`，未知键报错；两者必须字符串；`path` TrimSpace。
- **范围**：`pr = lower(norm(path))`；匹配 `lower(rel) == pr` 或 `lower(rel)` 以 `pr+"/"` 开头。`src` 命中 `src/a.go`，不命中 `srcfoo/a.go`；Windows 索引路径大小写归一，输入大小写均可。
- **统计**：`files`/`symbols`/`langCounts`/`parseErrorFiles` 都按范围内已索引文件累计；`symbols` 为 `Σ len(fi.Symbols)`。
- **topComplexity5**：仅 `complexityKindMatch("function")`；排序复杂度降序 → 文件 → 行号 → 名称 → kind；最多 5 条。
- **元信息**：`lastIndexedAt` 取自 `Meta().LastIndexedAt`（Unix 毫秒，0=未知）。
- **scope**：`path` 非空时为 `abs(norm(path))`；空时为 `workdir`。
- **只读门禁**：`readyGate` 使用 `OpenReadOnly` + `EnsureReadyReadOnly`，不 `MkdirAll`、不 Reconcile、不写索引。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 全项目 | false | `{workdir,scope:workdir,files,symbols,langCounts,topComplexity5:[…],parseErrorFiles,lastIndexedAt}` |
| 指定目录 | false | 同上，`scope` 为绝对目录路径 |
| 范围内无文件 | false | `files:0,symbols:0,langCounts:{},topComplexity5:[],note:"path_not_indexed"` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 未索引文件仍不可见 | `parseErrorFiles` 只覆盖“已入索引但 HasErr”；>8MB、被 exts/skip/gitignore 排除、不支持语言的文件不在统计中，`files` 不能当仓库文件总数。契约已声明。 |
| P3 | 无按目录拆分 | 只有单层前缀统计；对比多模块需多次调用（可接受）。 |
| P3 | `lastIndexedAt` 为原始 Unix 毫秒 | 契约已注明；模型可自行换算。 |
| P3 | handler 忽略 ctx | 取消后仍会完成统计；返回体很小，影响有限。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | `enabled=false` 不阻止查询 | 与配置页文案不一致；跨工具统一问题。 |

已修（2026-11-10）：path 纯字符串前缀/大小写敏感、topComplexity5 含非函数、排序不稳定、scope 空前缀为空串、统计口径未声明、缺 parseErrorFiles/lastIndexedAt、langCounts 键未说明、workdir 无绑定/查询写盘、未就绪文案指向不可见工具、schema 未闭合/零校验。

## 7. 测试/验收

- `TestToolModuleSummaryScopeAndCoverage`：全项目 scope=workdir、files/symbols、parseErrorFiles（含语法错误文件）、lastIndexedAt、topComplexity5 只含函数；`src`/`SRC`/`./src/`/绝对路径均命中、不吞 `srcfoo`；未命中 `path_not_indexed`；严格参数。
- 既有 `TestModuleSummary`（3 文件/5 符号/语言分布/topComplexity5 函数类/前缀过滤/no index）回归，topComplexity5 断言已改为 3 条函数。
- 回归：`go test -count=1 ./server/...` 通过。