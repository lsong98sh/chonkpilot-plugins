---
plugin: file
tool: file_replace
contract: tools/file/file_replace.tool.md
handler: src/lib/filetool/edit.go:15
reviewed_at: 2026-11-08
status: verified
---

# file_replace 审查

## 1. 一句话结论

有风险：精确替换逻辑可用，但契约承诺的“锁内校验 `expectMd5`”没有锁（TOCTOU），且成功返回携带完整 diff（最大约 8 MB），存在丢更新与上下文爆量两类问题。

## 2. 调用链

LLM → gateway `doCall`（剥展示名；`_meta.async` 缺省 auto，server 超时取 manifest 300s）→ 插件 MCP `tools/call` → `contract.Register`（`contract/contract.go:106`）→ `filePlugin.call`（`src/main.go:88`）→ `applyDefaults`（`main.go:126-150`）→ `dispatch`（`main.go:97`）→ `checkSandboxWritable`（`sandbox.go:37`，replace 属写工具）→ `toolset.Dispatch`（`toolset/toolset.go:36`）→ `filetool.runReplace`（`edit.go:15`）→ `replaceOne`（`edit.go:33`）→ `loadDecoded`（`edit.go:206`）+ `writeAtomic`（`readwrite.go:162`）。

## 3. 契约面

- YAML：`async: never`、`timeout: 120`、`idempotent: false`（`file_replace.tool.md:6-12`），与 file_write 相同地未进入插件 tools/list `_meta`（框架 `contract/contract.go:72-80`）。
- schema：`targets[1..2000]`，每项必填 `path/find/replace`，可选 `all/expectMd5`，`additionalProperties:false`（`:30-69`）。
- 摘要（`:16-26`）逐条对照：
  - 沙箱越界整次拒绝：成立（`sandbox.go:37-58`）。
  - `find` 为空 → 该项 failed：成立（`edit.go:35-37`，“find is required and must be non-empty”）。
  - `all` 缺省 true，false 只换第一处：成立（`edit.go:56-61`）。
  - 未找到 find → failed（`search not found`）：成立（`edit.go:50-53`）。
  - `expectMd5` “**锁内先校验**，不一致 skipped”：**不成立**——没有锁（`edit.go:45-48,67`）。
  - 原子写：成立（`edit.go:67` → `writeAtomic`）。
- 返回示例（`:72-86`）：失败/skipped 项写作 `{status:"skipped", error:"md5 mismatch"}`，实现返回的是 `{status:"skipped", reason:"md5 mismatch", md5:...}`；成功项字段 `replacements/md5Before/md5After/diff` 与实现一致。
- 契约未声明：源文件编码如何保留、二进制文件如何处理、`diff` 的体积上限。

## 4. 实现面

- 解析：`parseRequest` 仅校验 `targets` 形状（`filetool.go:75-96`）；`find` 必须是字符串且非空；`replace` 可为空串（删除匹配）；`all` 用 `asBool(...,true)`。
- 读取：`loadDecoded` → `os.Stat`（目录报 `is a directory`）→ `os.ReadFile` 整文件入内存 → `autoEncodeNameOf` 全量检测 → `fileops.DecodeText` 自动解码。无大小上限、无二进制拒绝（`edit.go:206-221`）。
- 执行：`strings.Count(text, find)`（非重叠计数）→ `ReplaceAll` 或 `Replace(...,1)` → 以检测到的编码 + 原 BOM 状态回写（`edit.go:50-69`）。
- `expectMd5`：在读出 `oldData` 后另做一次 `md5File(path)` 比较；不一致返回 skipped；比较与 `writeAtomic` 之间有窗口（`edit.go:41-48,67`）。无跨进程锁、无版本校验。
- 幂等：`idempotent:false`；重复执行同一 `find` 第二次会 `search not found`（failed），不会重复替换。
- 原子性：单文件原子；整批非事务；无锁。
- 资源：整文件读 + 整文件 diff 计算（`fileops.RenderFileDiff`，单侧上限 8 MB，`fileops/diff.go:12-21`）；`diff` 错误被静默忽略（`edit.go:75-77`）。
- 副作用：覆盖目标文件；写临时文件；改变文件权限（临时文件默认权限，[推断]）。

## 5. 返回面：LLM 实际看到的所有形态

| 形态 | isError | 完整字段/文本 |
|---|---|---|
| 全部成功 | false | `results[*]={path,status:"ok",replacements,md5Before,md5After,diff?}`；`summary{total,ok,failed,skipped}` |
| 部分成功 | false | 成功项 + `status:"failed",error` / `status:"skipped",reason,md5` |
| 全部失败（运行期） | **false** | 每项 failed；整体 JSON，`summary.failed=N` |
| 参数级失败 | true | `targets is required` / `targets must be an array` / `targets length must be 1..2000` / `targets[i] must be an object`；`参数解析失败: ...` |
| 沙箱越界 | true | 纯文本 `sandbox: 目标路径不在可写白名单内：<path>（本次调用已拒绝、未做任何写入；…）` |
| 单项失败 | false（批次） | `path is required` / 相对路径消息 / `find is required and must be non-empty` / `is a directory` / stat 或读错误 / `search not found` / 编码错误（Encode）/ `write: ...` |
| 跳过（md5 不一致） | false（批次） | `status:"skipped", reason:"md5 mismatch", md5:"<当前值>"`（注意无 `error` 字段） |
| 成功但无 diff | false | `diff` 缺失：内容 >8 MB 导致 `RenderFileDiff` 报错且被忽略（`edit.go:75-77`） |
| 成功但无 md5After | false | 写后 `md5File` 失败时静默省略（`edit.go:72-74`） |
| 部分写入后取消/超时 | true | `context canceled/deadline exceeded` 纯文本；已写 target 不回传（`toolset.go:58-63`） |
| 异步已受理 | —（gateway） | 契约 never，实际缺省 auto：>300s 可能 `pending + task_id`（[待核实] 网关路由） |
| 内部错误 | true | `marshal result: ...`；panic → `tool file_replace panic: ...` |

体积：`diff` 理论上可达单侧 8 MB 的渲染结果（约 8–16 MB 文本）一次性进入 LLM 上下文。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | `expectMd5` 未“锁内校验”：无任何锁；md5 比较与写入分离，TOCTOU | 契约 `:23`；`edit.go:45-48,67`；`fileops/lock.go:36` 无调用方 | 并发写时防覆盖承诺失效，仍会覆盖他人改动 |
| P1 | 成功返回带完整 `diff`，无截断/分页；上限 8 MB/侧 | `edit.go:75-77`；`fileops/diff.go:12-21`；契约 `:81` | 大文件替换可能一次返回数 MB，爆上下文 |
| P2 | skipped 项用 `reason`，契约示例用 `error` | `filetool.go:204-211`；契约 `:83` | 按示例解析的模型取不到字段 |
| P2 | 无二进制拒绝/探测：`DecodeText` 不返回错误，随后整文件按文本回写 | `edit.go:206-221`；`fileops/fileops.go:96-99` | 二进制文件若 `find` 命中，可能被有损重写（[待核实] `encodingx.DecodeAuto` 对非法字节的处理） |
| P2 | `diff` 计算失败被静默忽略，`diff` 字段消失但状态 ok | `edit.go:75-77` | 模型无法得知“为何没有 diff” |
| P2 | 整文件读入 + 全量编码检测，无大小上限 | `edit.go:214-219`、`:218` → `readwrite.go:730-745` | 超大文件内存/耗时风险 |
| P2 | 插件配置 `maxTargets` 默认 200 会静默截断（schema 2000） | `main.go:145-149`；`config.go:42`；契约 `:37` | 批量请求静默缩水 |
| P2 | `async:never/timeout:120` 未透出，实际 auto + 300s | 框架 `contract/contract.go:72-80`；主仓 `src/initdata/capability/plugins/file/manifest.json` | 契约与运行时不一致 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 接入 `acquireLock`：锁内完成 读→md5 校验→替换→写 | 优化 | 兑现 expectMd5、避免丢更新 | 中（锁顺序、批量死锁） | 行为收紧 | 建议现在做 |
| `diff` 加字节上限（如 64 KiB）+ `diff_truncated`，或改为 `diff=false` 默认、按需返回 | 改善 | 防上下文爆量 | 低（edit.go + 契约） | 返回字段语义变化 | 建议现在做 |
| skipped 同时（或统一）返回 `error`，保持与 failed 一致 | 改善 | 模型稳定解析 | 低 | 契约示例需同步 | 建议现在做 |
| 检测二进制（0x00）直接 failed `BINARY_UNSUPPORTED` | 优化 | 防损坏、防误用 | 低 | 与 read 语义对齐 | 建议现在做 |
| 大文件走流式/分块替换，或返回“文件过大，请用 file_patch/file_find”提示 | 优化 | 内存可控、可恢复 | 高 | 无 | 排期做 |
| `expectMd5` 不一致时返回当前 `md5` 便于模型重读后再试 | 增强 | 可恢复 | 低（已返回） | 无 | 已完成（保留） |

## 8. 建议的测试/验收（不执行）

- 并发两个 `file_replace`（均带同一 `expectMd5`）：期望仅一个成功，另一个 skipped/failed（当前可能都成功）。
- `diff` 体积：替换导致 5 MB 文件大面积变化，断言返回体被截断或受控。
- 无 BOM GBK 文件替换：验证编码与 BOM 不变。
- 二进制文件含与 `find` 相同的字节序列：验证拒绝或无损。
- skipped 返回形状：断言 `reason`/`error` 与契约一致。
- 批量部分失败 + 取消：验证已写项与返回。
- 201 个 targets：验证截断可见。

## 9. 基线修正

- 基线 §6（契约 async/timeout 决定 gateway）在本插件不成立：`contract.Parse` 不解析 async/timeout（框架 `contract/contract.go:72-80`），实际缺省 auto + manifest 300s。
- 基线 §3 写工具集合：`file_replace` 属 7 个写工具之一，沙箱生效；读工具不受限。
## 10. 修复记录（2026-11-08）

> **复盘订正**：原 P1「返回完整 diff 可能爆上下文」表述过重。`RenderFileDiff` 有 8 MiB/侧门禁，只有大文件 + 宽匹配 + 全量替换的尾部场景才可能产生数 MB diff；且完整 diff 对 LLM 非必需。该条按 **P2 条件性风险**处理，修复方式改为“成功项不再返回 diff”。

| 原问题 | 修复 | 验证 |
|---|---|---|
| P1 `expectMd5` 无锁（TOCTOU） | `replaceOne` 改为 `lockTarget` 加跨进程锁 → 读 → `expectMd5` → 替换 → 原子写 → 释放；批量逐项加锁 | `TestReplaceReturnsOnlyStatusAndCount`（锁获取/释放）、`TestReplaceSkipHasReasonAndError` |
| P1→P2 成功返回完整 diff | **成功项不再生成/返回 diff**，只回 `status + replacements`；需要 diff 用 `file_diff` | `TestReplaceReturnsOnlyStatusAndCount` |
| P2 skipped 用 `reason`，契约示例用 `error` | `itemSkip` 同时写 `reason` 与 `error`；保留当前 `md5` | `TestReplaceSkipHasReasonAndError` |
| P2 无二进制拒绝 | `loadDecoded` 用 `autoEncodeNameStrict`（头部检测）：二进制 → `failed: binary file (not supported)` | `TestReplacePatchBinaryRejected` |
| P2 `diff` 计算失败被静默忽略 | 已移除 diff 生成，问题消失 | 同上 |
| P2 整文件读入无上限 | 替换必须全文；未加固定 8 MiB 文件上限（`replace` 不再渲染 diff） | `TestReplacePreservesSourceEncoding` |
| P2 解码与回写可能跨编码（同源） | `loadDecoded` 用 `encodingx.AutoName` 取编码名、`decodeWithEncoding` 按同一编码名解码；回写复用该名，保证“同一编码读→改→写” | `TestReplacePreservesSourceEncoding` |
| P2 maxTargets / async 元数据 | 前两批已修 | `TestTargetsLimitSingleSource` 等 |
| 额外：重复 path 未检测 | 与 `file_write` 对齐，后者 `failed: duplicate target` | `TestReplacePatchDuplicateTarget` |

## 11. 独占打开改造（2026-11-09）

- `replaceOne` 删除 `lockTarget` 加锁与 `writeAtomic`：改为 `openExclusiveExisting`（Windows `dwShareMode=0`；共享冲突 50/100/200ms 重试 3 次，总尝试 = 初次 + 3 次）→ `io.ReadAll` 读旧内容 → `expectMd5` 用 `md5.Sum(oldData)` 校验 → 替换 → `writeInPlace`。
- 就地写不具备 rename 的崩溃原子性；写失败时用内存中的 `oldData` 回滚（Seek 0、写回、Truncate、Sync）。回滚成功 → 普通 failed；回滚失败 → 该项 `partial:true` + 明确错误。
- 成功返回不变：只有 `status + replacements`。
- 非 Windows 保留“临时文件 + rename”原子写；`lockTarget` / `acquireTargetLock` 保留给 copy/move 使用。
- 测试：`TestReplaceReturnsOnlyStatusAndCount` 移除锁断言；新增 Windows-only `TestReplaceFileInUse`；原 replace 功能/编码测试继续通过。

## 12. 同路径合并与锁调度（2026-11-09）

- **同路径合并**：`runReplace` 先解析全部路径、按解析后路径分组；同一文件多个 target 只**独占打开一次、读一次、写一次**。组内按 `targets[]` 输入顺序依次应用每项的 `find`/`replace`/`all`，每项返回自己的 `replacements`。
- **文件级全有全无**：组内第 k 项应用失败（`find` 不存在等）→ 第 k 项 `failed`，同组其他项 `skipped`（`reason:"not_applied"`），该文件**不写**；打开/读/解码/编码失败 → 同组所有项 `failed`；写失败用 `oldData` 回滚，回滚失败 → 同组所有项 `partial:true`；组内某 target 参数无效（`find` 为空）→ 该 target `failed`，其余 skipped，不写。
- **expectMd5**：同组多个不同的非空 `expectMd5` → 同文件所有项 `failed`（`reason`/`code` = `conflicting_expectMd5`）；相同值只对**原始内容**校验一次（独占句柄内、所有修改前）；不符 → 同组所有项 `skipped` + 当前 `md5`。
- **批调度（`src/lib/filetool/batchlock.go`）**：第一遍按入参顺序对每个路径组只尝试一次独占打开（`openExclusiveExistingOnce`，内部不再 sleep/重试）；`errFileInUse` 记 pending、继续后面的文件；其余文件处理完后对 pending 做 3 轮 50/100/200ms 重试；仍失败 → 同文件所有项 `failed` + `code:"FILE_IN_USE"`。fatal 立即填该组；每个文件处理完立即释放句柄。
- `replaceOne` 由 `replaceGroup`（实际实现为共享的 `runEditGroup` + `applyReplace`）取代；`editWriteFailure` 改为 group 版本（同组所有项 failed，回滚失败同组 `partial`）。
- **结果顺序**：`results` 长度 = 入参 `targets` 长度，按入参顺序预分配、按索引填充；分组不合并结果。
- **测试**：新增 `TestReplaceSamePathGrouped`、`TestReplaceGroupAtomicFailure`、`TestReplaceGroupExpectMd5`；`TestReplacePatchDuplicateTarget` 改为 grouped/两项 ok 断言；原编码/二进制/skipped 测试继续通过。§10 的“重复 path → duplicate target”和 §11 的“打开内部重试 3 次”由本节取代。

## 13. 成功项返回 encoding（2026-11-11）

- `runEditGroup` 成功项统一附 `encoding`（实际写回的编码）。`file_replace` 仍保持源编码、不使用默认字符集转换，`encoding` 仅供调用方确认。
- 契约 `file_replace.tool.md` 同步：成功项 = `status` + `replacements` + `encoding`。
