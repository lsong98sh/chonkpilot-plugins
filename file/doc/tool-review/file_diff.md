---
plugin: file
tool: file_diff
contract: file/src/tools/file/file_diff.tool.md
handler: file/src/lib/filetool/edit.go:139-203
reviewed_at: 2026-11-08
status: verified
---

# file_diff 审查

## 1. 一句话结论
**不建议当前形态（P0）**：返回的“unified diff”头两行内嵌了两个文件的完整内容，且没有任何 `@@` hunk，`context` 参数完全无效，产物无法被 `file_patch` 消费、也无法安全放入上下文。

## 2. 调用链
- manifest 注册 hot_tools（`chonkpilot/src/initdata/capability/plugins/file/manifest.json:21`）→ 插件 MCP `tools/list`。
- 契约由 `contract.LoadDir`（`file/src/main.go:48`）读入 `<root>/tools/file/*.tool.md`，`contract.Register` 只把 `name/title/hot/schema` 暴露给 LLM（`chonkpilot-framework/plugin/src/contract/contract.go:72-79,101-105`）。
- 调用：`filePlugin.call` → `dispatch`（`main.go:88-108`）→ `checkSandboxWritable` 对只读工具直接放行（`sandbox.go:38-40`；`writeTools` 不含 diff）→ `toolset.Dispatch`（`toolset/toolset.go:36-63`）→ `filetool.Dispatch` → `runDiff`（`edit.go:139`）。
- `runDiff` 逐 target 调 `diffOne(left,right)`（`edit.go:154`），**未把 target 对象传入**，故 `context` 无消费方。

## 3. 契约面
- YAML：`async: never`、`timeout: 120`、`idempotent: true`、`hot: true`（契约 5-12 行）。插件注册链路不传输 async/timeout/idempotent（见 C1），这些值在插件面不生效。
- schema：`targets[].context`（integer，minimum 0，缺省 3）声明存在，但实现无对应参数；`maxItems: 2000` 与运行时配置默认 200 不一致（C2）。
- 摘要声称“大文件（>16MB 任一侧）→ failed”（契约 20 行），实际阈值为 8MiB（`fileops/diff.go:12`）。
- 返回示例写 `diff: "--- a.old\n+++ a.new\n@@ …"`（契约 71 行），实际既没有文件名头，也没有 `@@`。
- LLM 可见描述只有摘要首段“按批比较两个文件，输出 unified diff（只读）…”（C4），16MB/context 规则都不可见；而这两条本身也不准确。

## 4. 实现面
- 参数解析：`parseRequest` 只校验 `targets` 形状（`filetool.go:75-96`）；`left`/`right` 逐项 `resolvePath` 强校验绝对路径/`~/`/`!/`（`edit.go:146-147`、`filetool.go:133-145`）。
- 读取：`os.ReadFile` 一次性读入两侧全文（`edit.go:162-169`），**在此之后** `RenderFileDiff` 才检查 `maxDiffBytes`（`fileops/diff.go:16`）——超大文件在限制生效前已进入内存。
- 比较：`fileops.DecodeText` 自动解码（UTF-16/UTF-8/GBK/ShiftJIS/Big5/EUC-KR），按解码后文本判 `identical`（`edit.go:170-177`），因此仅 BOM/编码不同的两份文件会判“相同”。
- 渲染：`diffmatchpatch.DiffMain` → `renderUnifiedDiff`，头两行 `--- %s` / `+++ %s` 的实参是**文件正文** `a`/`b`（`fileops/diff.go:25-27`），不是文件名；正文行只加 `-`/`+`/空格，无 hunk 头、无行号、无 context 裁剪。
- 限制：无输出体积上限、无分页/截断、无 `next` 指针；`added/removed` 由 `countDiffLines` 统计（`edit.go:190-203`）。
- 副作用：无（只读）；不写盘、不建临时文件。
- 并发/幂等：纯读，幂等成立。
- 清理：`os.ReadFile` 自动关闭，无句柄泄漏。

## 5. 返回面：LLM 实际看到的所有形态
1. **两侧一致**：`{results:[{left,right,status:"ok",identical:true,added:0,removed:0,diff:""}],summary{...}}`，isError=false。字段齐全，可直接判断。
2. **存在差异**：同上但 `identical:false`，`diff` 为“伪 unified diff”（头行含全文，正文无 `@@`），`added/removed` 可能因头行内嵌正文而被误计（正文中以 `+`/`-` 开头的行会被当作新增/删除）。
3. **部分成功**：某 target 路径不合法/读失败 → 该项 `status:"failed"` + `error`（前缀 `left: ` / `right: ` 或 `left is a directory` 类 OS 错误）；其余项照常，isError=false。
4. **全部失败**：仍 isError=false，仅 `summary.failed=total`（C3）。
5. **截断**：不存在。输出不截断、无分页，大文件差异性 diff 会把两份全文与全部上下文行一起返回。
6. **超限**：任一侧 >8MiB → 该项 failed，error=`file too large for diff (> 8388608 bytes); compare md5 via file_find meta=hash instead`（可恢复指引明确）；但读取已完成，内存风险仍在。
7. **参数级整体失败**：`targets` 缺失/非数组/空/超 2000 → 纯文本 isError=true（`targets is required` / `targets must be an array` / `targets length must be 1..2000` / `targets[0] must be an object`）。
8. **异步受理/运行中/终态**：C1 适用时，首次返回“已转后台”+task_id，终态经 mcp-tasks-report 回执（`chonkpilot/src/lib/llm/server/turn.go:342-357`；是否真转后台取决于 gateway 对插件工具 `_meta.timeout` 的补齐，[待核实]）。
9. **取消/超时**：`ctx` 结束时 `toolset.Dispatch` 立即返回纯文本 `context canceled` / `context deadline exceeded`，isError=true（`toolset/toolset.go:58-63`）；diff goroutine 继续跑，结果被丢弃。
10. **内部错误**：结果 JSON 序列化失败 → `marshal result: ...` isError=true（`filetool.go:122-126`）；panic 收口为 `tool file_diff panic: ...` isError=true（`toolset/toolset.go:42-46`）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | diff 头两行写入整份文件内容（`--- <全文>` / `+++ <全文>`），且正文无 `@@` hunk；产物不是 unified diff，`file_patch` 会报 `no hunks in diff` 或误把无前缀正文当上下文 | `fileops/diff.go:25-27`；`fileops/patch.go:39-41,84-86`；契约 71 行 | 输出被正文重复污染、体积约 3 倍；diff→patch 工作流必然失败 |
| P0 | `context` 参数完全未实现：`runDiff` 调 `diffOne(left,right)`，target 对象被丢弃 | `edit.go:154,160`；契约 48-52 行 | 文档承诺的唯一输出体量控制手段失效，模型无法降噪 |
| P1 | 契约写 >16MB 失败，实际 8MiB；且大小检查在 `os.ReadFile` 之后 | 契约 20 行；`fileops/diff.go:12,16`；`edit.go:162-169` | 8–16MB 文件预期内失败；GB 级文件先 OOM 再报错 |
| P1 | 无输出上限/分页/截断；最坏（8MB 两侧、差异多）可产生数十 MB 文本 | `fileops/diff.go:25-45`；`edit.go:182-186` | token 爆量，上下文不可恢复 |
| P2 | 不做二进制/大文本判定，二进制按 `DecodeText` 解码后比较，输出乱码 | `edit.go:170`；`fileops/fileops.go:96-99` | 误判“有差异”、无意义大 diff |
| P2 | `added/removed` 会把头行内嵌正文中以 `+`/`-` 开头的行计入 | `edit.go:190-203` | 统计值不可信 |

跨工具共通问题 C1（异步元数据缺失）、C2（maxTargets 静默截断到 200）、C3（全项失败 isError=false）、C4（描述仅首段）、C5（schema 不做运行时校验）同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 头部改为文件路径（`--- left` / `+++ right`），正文加 `@@ -a,b +c,d @@` hunk 头与行号，或至少去掉全文注入 | 优化 | 输出可读、可被 patch 消费、体积降一个数量级 | 中（2 文件：diff.go、edit.go） | 输出格式变化，需同步契约示例与测试；非 breaking API | 建议现在做 |
| 实现 `context`（4 行参数传递 + hunk 合并） | 改善 | 兑现契约，模型可控输出量 | 低 | 与上一条一起做最自然 | 建议现在做 |
| 阈值对齐（8MB 或 16MB 二选一，且先 Stat 后读取） | 优化 | 消除预期外失败与 OOM | 低 | 仅改常量/校验顺序 | 建议现在做 |
| 增加 `maxDiffBytes` 输出预算 + `truncated`/`next` 或“仅统计”模式 | 增强 | 防爆上下文，可续读 | 中 | 新增字段向后兼容 | 排期做 |
| 二进制/非文本直接 fail 并提示改用 `file_find meta=hash` | 改善 | 避免无意义 diff | 低 | 行为收紧，需契约同步 | 排期做 |

## 8. 建议的测试/验收（不执行）
- 两个小文本文件差异：断言 `diff` 头为路径、含 `@@`、可被 `file_patch` 应用（round-trip）。
- `context=0/1/3` 三种取值：断言输出行数随参数变化。
- 8MiB/8MiB+1/16MiB 文件：断言失败阈值与契约一致，且失败前不 OOM（可用 `Stat` 前置校验断言）。
- 二进制文件（含 NUL）：断言行为（拒绝或 explicit 标记），不得静默乱码。
- 200/201/300 targets：断言 C2 截断是否可见（当前预期为静默）。
- 全部 target 读失败：断言 isError 与 summary.failed。

## 9. 基线修正
无。基线 4（读工具不做 symlink/沙箱校验）在本工具成立：`writeTools` 不含 `file_diff`（`sandbox.go:22-31`），且路径只做 R-11 字符串解析，不 EvalSymlinks。
## 10. 修复记录（2026-11-08）

| 原问题 | 修复 | 验证 |
|---|---|---|
| P0 伪 unified diff（头行内嵌全文、无 `@@`） | 前一批已修：头为实际路径、正文带 `@@` hunk 与行号，可被 `file_patch` 消费 | `TestDiffPatchRoundTrip` |
| P0 `context` 未实现 | 前一批已修：`runDiff` 把 target 传入 `diffOne`，按 `2*context` 合并 hunk | `TestRenderFileDiffContext` |
| P1 8MB 输入限制（且读后检查） | **按用户口径移除输入大小限制**；`file_diff` 不再按文件大小拒绝（比较结果与大小无关） | 代码/契约 |
| P1 输出无上限 | `diff` 最多 128K（按行边界截断）；`diff_truncated/diff_omitted=true`、`diff_bytes`=完整 diff 字节、`diff_returned_bytes`=实际返回；`added/removed` 仍按完整 diff 统计 | `TestDiffOutputCap128K` |
| P2 内容一致但编码不同被吞 | 按各自源编码解码后比较；返回 `left_encoding/right_encoding/left_bom/right_bom`；`identical=true` 表示文本一致（不返回 `byte_identical/encoding_same` 聚合字段） | `TestDiffSameTextDifferentEncoding` |
| P2 二进制/乱码 | 本批未改（仍按文本解码） | — |
| P2 `added/removed` 误计 | 前一批已修（只统计 `@@` 之后行） | `TestDiffOutputCap128K` |
| maxTargets / async 元数据 | 前两批已修 | `TestTargetsLimitSingleSource` 等 |

> 复盘口径：`file_diff` 的“结果与文件大小无关”，不应按 8MB 预拒绝；输出预算 128K 才是上下文保护。截断后的 diff 不可直接用于 `file_patch`，契约已注明。
