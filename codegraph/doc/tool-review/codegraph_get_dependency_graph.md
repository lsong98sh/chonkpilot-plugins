---
plugin: codegraph
tool: codegraph_get_dependency_graph
contract: src/tools/codegraph/codegraph_get_dependency_graph.tool.md
handler: src/engines/treesitter/server/server.go:954（toolDependencyGraph）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_get_dependency_graph 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、无上限、file 大小写敏感、空结果无原因、Go 分组 import 丢失、Rust `pub use` 错拼、schema 未闭合、`require` 虚假声明等问题已修复。

## 1. 一句话结论

可用。默认只返回**有导入**的文件，按绝对路径排序后支持 `limit/offset` 分页；`total`/`totalEdges` 为真实值。`file` 支持相对/绝对路径（大小写不敏感）与路径段后缀，多命中返回 `ambiguous`；空结果用 `note` 区分未命中、无导入、空索引、翻页越界。Go 分组 `import ( ... )` 与 Rust `pub use` 已修复；JS/TS 的 `require()`/动态 `import()` 明确不提取，工具描述不再声称支持。

## 2. 调用链

1. LLM `codegraph_get_dependency_graph` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolDependencyGraph`。
3. `toolDependencyGraph`：
   - `validateDependencyGraphArgs`（未知键/类型/file·limit·offset·include_empty）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`，不 Reconcile/不写盘）→
   - `w.DependencyGraphPage(file, limit, offset, include_empty)`：遍历已索引文件 → 过滤 → 跳过空导入（默认）→ 排序 → 分页 → 汇总 edges；
   - 返回 JSON；未命中/空结果带 `note`。
4. 参数错误 `isError=true` 文本；门禁状态 `isError=false` 的 `status` 对象。

## 3. 契约面

- 契约源 = `codegraph_get_dependency_graph.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`additionalProperties:false`；`limit` 1..500 default 50；`offset` >=0；`include_empty` boolean default false；`file` 大小写不敏感，相对/绝对/路径段后缀。
- 摘要：静态导入语义、默认跳过空文件、`total/returned/edges/totalEdges`、`deps` 键为绝对路径、原始串未解析、Go 分组/Rust pub use 已支持、JS require/动态 import 不提取、notes、workdir 规则。
- 返回示例：全项目分页、单文件命中、四类空结果、门禁与参数错误。

## 4. 实现面

- **参数**：`workdir`/`file` 必须字符串；`include_empty` 必须布尔；`limit` 缺省 50、>500 截到 500、<1 报错；`offset` <0 报错。未知键报错。
- **file 匹配**：`norm(file)` 后大小写不敏感全等，或路径段后缀（`main.go` 命中 `src/main.go`）；绝对路径先归一为相对。命中多个文件时全部返回，`file_matched=true`、`ambiguous=true`（`total>1`）。
- **取数**：`DependencyGraphPage` 收集匹配文件（`include_empty=false` 时丢掉 `len(Imports)==0`），`sort.Strings` 后按 `offset/limit` 切页；`total` 为切页前文件数，`totalEdges` 为切页前导入总数；`edges` 为本页导入数。
- **返回**：`{workdir,total,returned,files,limit,offset,truncated,edges,totalEdges,deps}`；`files` 保留旧字段 = `returned`；`deps` 值复制为 `[]` 不为 `nil`。
- **notes**：`file_not_indexed`（file 未命中）、`no_dependencies`（有已索引文件但无导入，或 file 命中但无导入）、`no_indexed_files`（`include_empty=true` 且索引为空）、`offset_beyond_total`。
- **导入提取**：Go 递归 `import_spec_list`（分组 import 已支持）；Rust 取 `use` 关键字之后文本（兼容 `pub use` / `pub(crate) use`）；JS/TS 仅静态 `import_statement`/`export_statement`；Python/Java 原有规则。
- **只读门禁**：`readyGate` 使用 `OpenReadOnly` + `EnsureReadyReadOnly`，不 `MkdirAll`、不 Reconcile、不写索引。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 全项目/分页 | false | `{workdir,total,returned,files,limit,offset,truncated,edges,totalEdges,deps:{abs:[…]}}` |
| 单文件命中 | false | 上述 + `file,file_matched`；多个文件另有 `ambiguous:true` |
| file 未命中 | false | `total:0,deps:{},file_matched:false,note:"file_not_indexed"` |
| 命中但无导入 | false | `total:0,deps:{},file_matched:true,note:"no_dependencies"` |
| 索引为空 | false | `total:0,deps:{},note:"no_dependencies"`（默认）/ `"no_indexed_files"`（include_empty=true） |
| 翻页越界 | false | `returned:0,note:"offset_beyond_total"` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 值仍是未解析的原始导入串 | 模型可能把外部模块/相对路径当仓库内路径；需要解析请看 `codegraph_find_circular_deps`。契约已声明。 |
| P2 | 无 import 行号/位置 | 需要再读文件定位导入语句；后续可在 `FileInfo` 存行号。 |
| P2 | 语言级解析为近似 | Python 相对导入、Java 通配 `.*`、Rust 嵌套 `use a::{b,c}` 按原始串返回；不做别名/展开。 |
| P2 | 解析失败/超 8MB/被排除文件不可见 | 结果无 `skipped`/`parseErrors`，覆盖率不可知。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | `files` 为兼容字段 | 语义 = `returned`；新代码用 `total/returned/edges/totalEdges`。 |
| P3 | `enabled=false` 不阻止查询 | 与配置页文案不一致；跨工具统一问题。 |
| P3 | `require()`/动态 `import()` 不提取 | 已从 description/契约移除虚假声明；如需支持需扩展 `extractImports`（静态字面量可做）。 |

已修（2026-11-10）：workdir 无绑定/查询写盘、无 limit/分页、file 大小写敏感且仅精确、空结果无原因、Go 分组 import 丢失、Rust `pub use` 拼错、schema 未闭合/零校验、`require` 虚假声明、未就绪文案指向不可见工具、`total` 截断。

## 7. 测试/验收

- `TestDependencyGraphExtraction`：Go 分组 + 单行 import；Rust `pub use`/`pub(crate) use`；JS 静态 import 提取、`require()`/动态 `import()` 不提取。
- `TestToolDependencyGraphPageAndFilters`：默认为空文件跳过、`include_empty`、分页/truncated/offset 越界、file 大小写不敏感/路径段后缀/绝对路径、多命中 `ambiguous`、`file_not_indexed`、`no_dependencies`、严格参数。
- `TestToolDependencyGraphEmptyIndex`：空索引下 `no_dependencies` / `include_empty=true` 的 `no_indexed_files`。
- 既有 `TestImports`（`Workspace.Imports` 旧接口）回归。
- 回归：`go test -count=1 ./server/...` 通过。