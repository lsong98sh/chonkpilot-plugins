---
plugin: codegraph
tool: codegraph_find_circular_deps
contract: src/tools/codegraph/codegraph_find_circular_deps.tool.md
handler: src/engines/treesitter/server/server.go:1050（toolCircularDeps）
reviewed_at: 2026-11-10
status: verified
---

# codegraph_find_circular_deps 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 workdir 无绑定/查询写盘、输出无上限、`cycles:null`、环集合不稳定、自环漏报、未就绪文案指向不可见工具、schema 未闭合等问题已修复。

## 1. 一句话结论

可用。按 **workdir 相对路径**输出简单环，自环默认包含；从字典序最小节点出发枚举，同一环只报一次且结果确定。`limit/offset` 分页 + `truncated` 控制输出体积（环数潜在指数级，不返回 `total`）。参数严格校验、查询只读；解析仍是启发式，0 环不代表绝对无循环。

## 2. 调用链

1. LLM `codegraph_find_circular_deps` → gateway → 插件 MCP。
2. 查询工具闭包：args → `ResolveWorkdirFromMeta` → `toolCircularDeps`。
3. `toolCircularDeps`：
   - `validateCircularDepsArgs`（未知键/类型/limit·offset/include_self）→ `readyGate`（`OpenReadOnly` + `EnsureReadyReadOnly`，不 Reconcile/不写盘）→
   - `w.FindCyclesPage(limit, offset, include_self)`：构建已索引文件图 → `resolveImport` → 排序邻接 → 从最小节点 DFS 枚举简单环 → 分页；
   - 返回 `{workdir,cycleCount,returned,limit,offset,truncated,cycles,hint[,note]}`。
4. 参数错误 `isError=true` 文本；门禁状态 `isError=false` 的 `status` 对象。

## 3. 契约面

- 契约源 = `codegraph_find_circular_deps.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`additionalProperties:false`；`limit` 1..500 default 50；`offset` >=0；`include_self` boolean default true。
- 摘要：相对路径 + 闭环重复起点、自环形态、解析启发式与“0 环不绝对”、同一环只报一次、分页/truncated、无 total 的原因、已索引范围、workdir 规则。
- 返回示例、空结果 `no_cycles`、翻页越界 `offset_beyond_total`、门禁与参数错误。

## 4. 实现面

- **参数**：`workdir` 必须字符串；`include_self` 必须布尔（默认 true）；`limit` 缺省 50、>500 截到 500、<1 报错；`offset` <0 报错；未知键报错。
- **建图**：只保留 `resolveImport` 能解析到已索引文件的边；邻接去重 + `sort.Strings`；`include_self=false` 时跳过 `t==p`。
- **确定性**：节点按相对路径排序；`resolveImport` 在 Go/Py/Java 的目录/前缀多候选时取**字典序最小**文件（`firstIndexed`），不再受 map 遍历顺序影响。
- **枚举**：每个环只从其字典序最小节点出发（遍历时跳过 `order[v] < startIdx`），DFS 找到回到起点的边即记录；同一环不会因旋转重复，自环由 `path=[start]` + `v==start` 记录。
- **分页**：`limit>0` 时最多枚举 `offset+limit+1` 条；多出的 1 条用于置 `truncated=true`，再切出 `[offset:offset+limit]`。`limit<=0` 仅供内部/测试（不限量）。
- **返回**：`cycleCount`/`returned` = 本页数量；`cycles` 为 `[][]string`，空为 `[]`；`hint` 固定说明；空结果 `note=no_cycles`，越界 `offset_beyond_total`。
- **只读门禁**：`readyGate` 使用 `OpenReadOnly` + `EnsureReadyReadOnly`，不 `MkdirAll`、不 Reconcile、不写索引。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有环 | false | `{workdir,cycleCount,returned,limit,offset,truncated,cycles:[["a.go","b.go","a.go"],…],hint}` |
| 自环 | false | `cycles` 含 `["a.go","a.go"]`（`include_self` 默认 true） |
| 无环 | false | `cycleCount:0,returned:0,cycles:[],note:"no_cycles",hint` |
| 翻页越界 | false | `returned:0,note:"offset_beyond_total"` |
| 还有下一页 | false | `truncated:true` |
| 未初始化/构建中/索引错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法 | **true** | 错误文本 |
| workdir 缺失/越界/非绝对 | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 解析仍是启发式 | 只覆盖可解析到仓库内文件的边；别名/包重命名/TS path alias/外部模块不参与，0 环不能证明无循环。契约已声明。 |
| P2 | 单个环长度无上限 | `limit` 只限制环**数量**；一个数千文件的巨环仍可能返回较大体积。后续可加 `max_cycle_length` 或对超长环标记。 |
| P2 | 无路径/目录范围过滤 | 大仓库只能全图枚举；后续可加 `path` 前缀限定子图。 |
| P2 | handler 忽略 ctx | 取消后仍会跑完当前枚举；枚举已按 limit 截断，影响有限。 |
| P3 | 不返回 `total` | 完整环数可能指数级，无法廉价给出；用 `truncated` + 翻页判断。 |
| P3 | `hint` 每条响应固定出现 | 约占几十 token；可改到契约/描述里，但保留可降低误读。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | `enabled=false` 不阻止查询 | 与配置页文案不一致；跨工具统一问题。 |

已修（2026-11-10）：workdir 无绑定/查询写盘、环数/字节无上限、`cycles:null`、map 迭代导致结果不稳定、自环漏报、只报回边集合而非简单环、未就绪文案指向不可见工具、schema 未闭合/零校验、绝对路径输出体积大。

## 7. 测试/验收

- `TestToolCircularDepsPage`：2 个环（含共享节点）、相对路径、重复调用结果一致、limit/offset/truncated、越界 note、严格参数。
- `TestToolCircularDepsSelfAndNoCycles`：默认包含自环、`include_self=false` 返回空 `[]` + `no_cycles`。
- 既有 `TestFindCycles`（无环/无索引）回归。
- 回归：`go test -count=1 ./server/...` 通过。