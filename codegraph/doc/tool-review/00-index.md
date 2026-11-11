# codegraph 工具审查索引

> 审查范围：本文件由 **codegraph-B** 建立，覆盖 `codegraph_find_circular_deps` / `codegraph_analyze_complexity` / `codegraph_get_module_summary` / `vfts_query`。
> codegraph-A 负责的同插件工具（`codegraph_symbol_search` / `codegraph_get_symbol_info` / `codegraph_callers` / `codegraph_callees` / `codegraph_get_dependency_graph`）追加到下表末尾。
> **2026-11-10 复核**：LLM 可见查询工具已迁移到 `src/tools/codegraph/*.tool.md`（schema/description/_meta 来自 md），管理工具仍程序化注册；workdir 已统一从 `_meta.chonkpilot.work_dir` 注入/校验。`codegraph_symbol_search` / `codegraph_get_symbol_info` / `codegraph_callers` / `codegraph_callees` / `codegraph_get_dependency_graph` 本轮已修（真实 total/分页、严格参数、空结果/未找到 note、只读查询；callees 另加目标候选与 `candidates`；依赖图另加 `include_empty`、file 后缀匹配与 Go/Rust 导入修复），对应行已按当前状态更新。B 组 `codegraph_find_circular_deps` / `codegraph_analyze_complexity` / `codegraph_get_module_summary` / `vfts_query` 也全部复核修复（分页/确定性枚举/自环；函数类过滤/上限/闭包剪枝；路径段匹配/覆盖信息；topK 上限/严格校验/会话排序），9 个工具均无 P0/P1。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|:--:|:--:|:--:|---|
| codegraph_find_circular_deps | 可用（2026-11-10 已修：limit/offset 分页、相对路径、确定性枚举、自环、空结果 `[]`、严格参数、只读查询） | 0 | 0 | 4 | [codegraph_find_circular_deps.md](./codegraph_find_circular_deps.md) |
| codegraph_analyze_complexity | 可用（2026-11-10 已修：默认只含函数类、topN 上限/truncated、稳定排序、嵌套闭包剪枝、coverage、严格参数、只读查询） | 0 | 0 | 2 | [codegraph_analyze_complexity.md](./codegraph_analyze_complexity.md) |
| codegraph_get_module_summary | 可用（2026-11-10 已修：path 路径段边界/大小写不敏感、topComplexity5 只含函数类 + 稳定排序、scope=workdir、parseErrorFiles/lastIndexedAt、严格参数、只读查询） | 0 | 0 | 1 | [codegraph_get_module_summary.md](./codegraph_get_module_summary.md) |
| vfts_query | 可用（2026-11-10 已修：topK 上限/truncated、scope/role/match·query 严格校验、会话先相关度后 ts、path_ignored、session_filter_applied、score/line 口径、readiness 文案） | 0 | 0 | 1 | [vfts_query.md](./vfts_query.md) |
| codegraph_symbol_search | 可用（2026-11-10 已修：真实 total/分页、kind 校验、limit 上限、空结果 note、只读查询） | 0 | 0 | 3 | [codegraph_symbol_search.md](./codegraph_symbol_search.md) |
| codegraph_get_symbol_info | 可用（2026-11-10 已修：id 优先、多命中候选、file/id 大小写、not_found note、严格参数、只读查询） | 0 | 0 | 2 | [codegraph_get_symbol_info.md](./codegraph_get_symbol_info.md) |
| codegraph_callers | 可用（2026-11-10 已修：id 优先、真实 total/分页、目标身份、空结果 note、严格参数、只读查询） | 0 | 0 | 3 | [codegraph_callers.md](./codegraph_callers.md) |
| codegraph_callees | 可用（2026-11-10 已修：id 优先、目标多命中候选、candidates 0/1/>1、真实 total/分页、not_callable、严格参数、只读查询） | 0 | 0 | 3 | [codegraph_callees.md](./codegraph_callees.md) |
| codegraph_get_dependency_graph | 可用（2026-11-10 已修：真实 total/分页、include_empty、file 大小写不敏感+路径段后缀、空结果 note、Go 分组 import、Rust pub use、严格参数、只读查询） | 0 | 0 | 4 | [codegraph_get_dependency_graph.md](./codegraph_get_dependency_graph.md) |

**本组合计（按各文档 §6 行数累计；跨工具共性问题会在每个受影响工具的文档里重复计）：P0 = 0，P1 = 0，P2 = 23。**
其中 A 组 5 个工具：P0 = 0，P1 = 0，P2 = 15；B 组 4 个工具：P0 = 0，P1 = 0，P2 = 8。

## 范围差异与注册链核实（B 组）

1. **分配清单与源码一致**：codegraph 引擎 `toolSpecs`（`src/engines/treesitter/server/server.go:23-172`）确实包含分派的 3 个工具；`vfts_query` 由插件自注册（`src/vfts_query.go:45-69`）。
2. **hot 工具全集 = 9**：`src/main.go:31-41` 与 `src/initdata/capability/plugins/codegraph/manifest.json:21-31` 均为 9 个；本组只覆盖 4 个，其余 5 个归 codegraph-A。
3. **hot 过滤成立**：llm-server 只把 `Hot=true` 的工具发给 LLM（`chonkpilot/src/lib/llm/server/server.go:285-295`）；插件经 `_meta.hot` + `p.MCPHot(hotTools...)` + manifest `hot_tools` 三处一致声明。
4. **该 workdir 里 `hot=false` 的工具（LLM 不可见）**：
   - codegraph 引擎：`codegraph_configure` / `codegraph_initialize` / `codegraph_status` / `codegraph_index_paths`（`server.go:23-70`，均未设 `Hot`）；
   - vfts 引擎：`vfts_configure` / `vfts_index` / `vfts_status` / `vfts_dict_get` / `vfts_dict_set` / `vfts_session_index` / `vfts_session_clear` / `vfts_session_status`（`src/engines/vfts/server/server.go:24-145`，均 `Hot=false`）。
   - 影响：多个查询工具的未就绪文案让 LLM “先调 `codegraph_initialize` / `vfts_index`”，但这两个工具**不在 LLM 工具面**（P1，已在各工具文档记录）。
5. **`vfts_query` 两个注册点已裁决**：
   - `main.go:71` 调 `vfts.RegisterManagementTools` → `registerTools(srv,false)` 在循环首行跳过所有 `Hot` 工具（`vfts/server/server.go:182-186`），引擎版 `vfts_query` **不注册**；
   - `main.go:74` 调 `registerVftsQuery` 注册**唯一**一个 `vfts_query`，schema **不含 workdir**（`src/vfts_query.go:31-42`）；
   - `vfts.RegisterTools`（含引擎版）只在引擎独立可执行文件使用（`src/engines/vfts/main.go:87`），插件进程不调用。
   - **结论：无重复注册、无覆盖；LLM 看到的是插件版（无 workdir）。** 补注：go-sdk `AddTool` 同名是“后注册替换”（`modelcontextprotocol/go-sdk@v1.7.0/mcp/server.go:318-323`），不报错——若将来误用 `RegisterTools`，插件版会静默覆盖引擎版（可维护性风险）。
6. **`vfts_query` 的 workdir 注入链已核实**：gateway `callContextMeta`（`chonkpilot-mcp-gateway/gateway/contextmeta.go:13-50`）对 builtin 下游注入 `_meta.chonkpilot{work_dir,session,top_session,...}`（provider.go:1231）；插件按 `work_dir → instance 登记 → 唯一工作区` 回退（`src/vfts_query.go:113-135`），失败显式报错、不猜路径。
7. **vfts 管理工具未纳入本次审查**：按分派要求只核实“LLM 不可见”是否成立（成立，见第 4 条），不逐个出文档。
8. **附带发现（不在 4 个工具的直接实现里，但影响其返回面）**：
   - `status.json` 查询门（`gateIndexStatus`）在 codegraph/vfts 两侧都保留，但**生产代码没有写方**（`WriteIndexStatus` 仅测试引用）；`docs/spec/20-modules/2B-vfts.md` 已记载该机制 2026-10-25 退场 → 代码与 spec 存在残留漂移（P2，已在各文档记录）。
   - 本组 4 个工具均未声明 `_meta.async` / `_meta.timeout`；gateway 对 builtin 按 `auto`、阈值回落 server `timeout_sec=300`（manifest.json:32；gateway mcpgateway.go:928-934、972）→ 达 300s 才转后台，不是“缺省超时即转后台”。
9. **共性（供 codegraph-A 参考，预计其 5 个工具同样存在）**：
   - `workdir` 来自 LLM，`Open` 只做 `filepath.Abs` + `os.Stat`，不校验是否当前项目、不支持 `~/`/`!/`、不拒绝相对路径 → 可能静默分析错误目录；
   - 查询会为 workdir `MkdirAll(.chonkpilot/codegraph)`，且每次查询前跑 `EnsureReady → Reconcile` 全仓 walk+stat（recMu 串行化），大仓库延迟高、取消不生效（handler 丢弃 ctx）；
   - 参数数字只认 `float64`，字符串数字被静默忽略。

## 范围差异与注册链核实（A 组）

1. **分配清单与源码一致**：5 个工具均在 `toolSpecs`（`src/engines/treesitter/server/server.go:23-172`）中，`Hot:true`（server.go:83/96/110/124/135）。`toolSpecs` 共 13 项 = 4 个管理工具 + 8 个 codegraph 查询工具；`vfts_query` 由插件另注册（`src/main.go:74`）→ 与 manifest `hot_tools` 的 9 个一致（`manifest.json:21-31`）。A 组不覆盖其余 3 个 codegraph 查询工具与 `vfts_query`（B 组文档）。
2. **「hot=false 的 LLM 不可见」只成立于“不注入”，不成立于“不可调用”**：llm-server 默认工具面只下发 `Hot=true`（`chonkpilot/src/lib/llm/server/server.go:285-295`），但 `visibleTools` 保留全部工具（同文件 258-280）；调用处 `toolAllowed` 只在白名单非空时校验（`turn.go:1366-1370、1458-1466`），gateway `findFor` 也不看 hot（`mcpgateway.go:829`）。`codegraph_initialize` / `codegraph_status` 等仍可被按名调用，但 2026-11-10 起查询工具的未就绪文案**不再点名**这些不可见工具，模型不会因错误提示去调用它们。
3. **`workdir` 来源（2026-11-10 已修）**：5 个 A 组工具 `workdir` 均改为**可选**，缺省取 `_meta.chonkpilot.work_dir`；显式传入时经 `ResolveWorkdirFromMeta` 校验必须为当前项目内绝对路径（拒绝 `~`/相对/越界）。查询走 `OpenReadOnly` + `EnsureReadyReadOnly`，不再 `MkdirAll`/Reconcile/写盘。
4. **宿主自动注入 workdir 的旧通道已退场**：`execx` 曾支持“缺省回落 `CHONKPILOT_WORKDIR`”（`src/engines/treesitter/execx/execx.go:5-8、62-66`），但桌面端 `core-mcp-server` 已于 2026-11-02 退役、executor 全部退场（`chonkpilot/src/lib/gui/host/main.go:225、587`），当前运行时不存在该回落路径。
5. **`codegraph_status` 仍隐藏**：模型在 `not_initialized` / `pending` / `error` 时无法用可见工具查询进度；门禁文案改为“请稍后重试或请用户初始化项目索引”，不再指向不可见工具。
6. **配置 `enabled` 不参与工具面/门禁**：`codegraph_configure` 只写 `meta.Enabled`（index.go:28-44），`readyGate` 与 5 个查询 handler 全无 `Enabled` 判定；插件启动即注册全部工具、无按启用状态重注册路径。前端文案称“停用即从工具面移除”（`chonkpilot/src/frontend/src/locales/zh-CN/projectConfig.json:54`）与实际不符（P2）。
7. **schema 运行时校验（2026-11-10 已修）**：5 个 A 组查询工具均做严格参数校验（未知键、类型、取值边界），不再只依赖 LLM 侧 schema；管理工具（hot=false）仍只做宽松取值。
8. **`total` 语义（2026-11-10 已修）**：`symbol_search` / `callers` / `callees` / `get_dependency_graph` 均返回**未截断的真实 total**，并带 `limit/offset/returned/truncated`；模型可判断是否漏结果并续取。
9. **查询只读（2026-11-10 已修）**：查询不再 `MkdirAll`、不再 `Reconcile`、不写索引库；同 workdir 的并发查询只读加载。`Initialize` 仍未持 `recMu`（index.go:184），与 `Clear`/`Reconcile` 的单飞约定不一致，自动重建与查询并发时仍有交错写风险（[推断]，P2/管理面）。
10. **Go 分组 import（2026-11-10 已修）**：`extractImports` 递归 `import_spec_list`；`TestDependencyGraphExtraction` 已验证 `import ( "fmt"; "os" )` 可被提取。Rust `pub use` / `pub(crate) use` 同步修复。