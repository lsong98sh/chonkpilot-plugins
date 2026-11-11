---
plugin: file
tool: file_patch
contract: tools/file/file_patch.tool.md
handler: src/lib/filetool/edit.go:82
reviewed_at: 2026-11-08
status: verified
---

# file_patch 审查

## 1. 一句话结论

不建议当前形态：`ApplyFileDiff` 每个 hunk 只校验首行上下文，其余上下文按行号盲消费，且以 `+++`/`---` 开头的增删行会被当文件头丢弃；会在“看起来成功”的情况下静默写错内容。

## 2. 调用链

LLM → gateway `doCall`（剥展示名；`_meta.async` 缺省 auto，server 超时取 manifest 300s）→ 插件 MCP `tools/call` → `contract.Register`（`contract/contract.go:106`）→ `filePlugin.call`（`src/main.go:88`）→ `applyDefaults`（`main.go:126-150`）→ `dispatch`（`main.go:97`）→ `checkSandboxWritable`（`sandbox.go:37`，patch 属写工具）→ `toolset.Dispatch`（`toolset/toolset.go:36`）→ `filetool.runPatch`（`edit.go:82`）→ `patchOne`（`edit.go:100`）→ `fileops.ApplyFileDiff`（`edit.go:116` → `fileops/patch.go:9` → `applyUnifiedDiff`，`patch.go:16`）→ `writeAtomic`（`readwrite.go:162`）。

## 3. 契约面

- YAML：`async: never`、`timeout: 120`、`idempotent: false`（`file_patch.tool.md:6-12`）；与其它 file 工具相同，async/timeout 未被插件契约解析器消费（框架 `contract/contract.go:72-80`）。
- schema：`targets[1..2000]`，每项必填 `path/patch`，可选 `expectMd5`，`additionalProperties:false`（`:30-60`）。
- 摘要（`:16-26`）逐条对照：
  - 沙箱越界整次拒绝：成立（`sandbox.go:37-58`）。
  - `patch` 为 unified diff：成立（解析 `@@ -N`、上下文、`-`、`+`）。
  - “**上下文不匹配即整项失败**（不做模糊匹配、不做 fuzz）”：**不成立**——每个 hunk 仅校验第一条上下文/删除行（`patch.go:26,56-78`）。
  - `expectMd5` “锁内先校验”：**不成立**——无锁，先校验后写存在窗口（`edit.go:111-114,116-124`）。
  - 原子写：成立（`edit.go:124`）。
  - “补丁里若出现目标文件之外的文件头 → 该项 failed”：**不成立**——`---`/`+++` 一律忽略，不与 `path` 比对，也不报错（`patch.go:51-52`）。
- 返回示例（`:63-77`）：`hunks/md5Before/md5After` 存在；失败示例文案 `context mismatch at hunk 1` 与实现中文文案不一致。

## 4. 实现面

- 参数：`patch` 去空白后为空 → 该项 failed `patch is required`（`edit.go:101-104`）；`expectMd5` 字符串比较（忽略大小写）。
- 读取：`loadDecoded`（`edit.go:206-221`）整文件入内存，`DecodeText` 自动解码，`autoEncodeNameOf` 决定回写编码；无大小上限、无二进制拒绝。
- 应用算法（`fileops/patch.go:16-92`）：
  - 按 `@@ -oldStart` 跳转 `srcIdx`，回填两 hunk 之间的源行；
  - 每个 hunk 置 `hunkFirst=true`；仅当遇到**第一条** context/`-` 行时做逐字比对，之后关闭校验；
  - 后续 context 行直接取源文件当前位置的行追加，**不与该行 diff 文本比较**；
  - `+` 行无条件插入，忽略 `@@ -N,M +N,M` 的行数声明；
  - `---`/`+++` 前缀一律当文件头跳过（先于 `-`/`+` 分支判断）。
- 编码/换行：源文本按 `\n` 切分并**保留行尾 `\r`**；diff 行先 `TrimSuffix("\r")`（`patch.go:37`），因此 CRLF 源文件的上下文比对必然不一致；输出统一用 `\n` join，新增行不带 `\r`，可能产生混合换行。
- `hunks` 统计：`strings.Count(patch,"\n@@")`（+首行 `@@` 校正），纯文本推导（`edit.go:127-130`）。
- 并发/幂等：无锁、无版本号；不检测同一调用内重复 `path`（与 `file_write` 不同），第二个 target 会在已修改内容上继续打补丁。
- 原子性：单文件原子；整批非事务；取消会留下部分已写文件。
- 资源：整文件读 + 完整解码；无上限。

## 5. 返回面：LLM 实际看到的所有形态

| 形态 | isError | 完整字段/文本 |
|---|---|---|
| 全部成功 | false | `results[*]={path,status:"ok",hunks,md5Before,md5After}`；`summary{total,ok,failed,skipped}` |
| 部分成功 | false | 成功项 + `status:"failed",error` / `status:"skipped",reason,md5` |
| 全部失败（运行期） | **false** | 每项 failed；JSON 外壳仍在 |
| 参数级失败 | true | `targets is required` / `targets must be an array` / `targets length must be 1..2000` / `targets[i] must be an object`；`参数解析失败: ...` |
| 沙箱越界 | true | 纯文本 `sandbox: 目标路径不在可写白名单内：<path>（本次调用已拒绝、未做任何写入；…）` |
| 单项失败 | false（批次） | `patch is required` / 路径错误 / `is a directory` / stat 或读错误 / `no hunks in diff` / `hunk 上下文不匹配：源文件第 N 行缺失（期望 "…"）` / `hunk 上下文不匹配：源文件第 N 行实际 "…"，期望 "…"` / 编码错误 / `write: ...` |
| 跳过（md5 不一致） | false（批次） | `status:"skipped", reason:"md5 mismatch", md5:"<当前值>"`（无 `error` 字段） |
| 成功但内容错误 | false | 无任何异常字段：后续上下文不匹配、`+++`/`---` 增删行被吞都不会体现，`status:"ok"` |
| 成功但无 md5After | false | 写后 md5 失败时静默省略（`edit.go:132-134`） |
| 部分写入后取消/超时 | true | `context canceled/deadline exceeded` 纯文本；已写 target 不回传（`toolset.go:58-63`） |
| 异步已受理 | —（gateway） | 契约 never，实际缺省 auto：>300s 可能 `pending + task_id`（[待核实] 网关路由） |
| 内部错误 | true | `marshal result: ...`；panic → `tool file_patch panic: ...` |

体积：返回不含 diff/正文，仅计数与 md5，体积安全。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| **P0** | 每个 hunk 只校验首条 context/`-` 行，其余上下文不比对、按行号盲消费；与契约“上下文不匹配即整项失败”直接矛盾 | `fileops/patch.go:26,56-78`；契约 `:21` | 陈旧/错位补丁会被“成功”应用到错误位置，静默损坏文件内容 |
| P1 | `---`/`+++` 前缀在所有分支之前被当文件头丢弃：新增行内容以 `++` 开头（diff 行 `+++…`）、删除行内容以 `--` 开头（`---…`）会被静默吞掉 | `fileops/patch.go:51-52` vs `:55` | 合法补丁丢行，写回后内容缺失且状态 ok |
| P1 | CRLF 源文件：源行保留 `\r`，diff 行去掉 `\r`，首行上下文必然不匹配；输出又统一 `\n` | `fileops/patch.go:18-22,37,32` | Windows 常见 CRLF 文件打补丁直接失败；若侥幸通过则换成混合换行 |
| P1 | `expectMd5` 无锁（先校验后写有窗口）；契约称锁内校验 | 契约 `:22`；`edit.go:111-124`；`fileops/lock.go:36` 无调用方 | 并发时防覆盖承诺失效 |
| P2 | 契约称“补丁里出现其它文件头 → failed”，实现直接忽略 `---/+++`，不校验文件名 | 契约 `:24`；`patch.go:51-52` | 承诺的安全检查不存在（不会写到别处，但也不会拒绝异物） |
| P2 | `hunks` 由文本计数推导，忽略 `@@` 行数声明；无 hunk 数校验 | `edit.go:127-130`；`patch.go:39-48` | 返回的 `hunks` 可能与实际应用不符 |
| P2 | 不检测同批重复 `path`（`file_write` 有），第二个补丁作用在已改内容上 | `edit.go:82-97` | 多 target 语义不一致；易产生意外结果 |
| P2 | 失败文案为中文且与契约示例不同；skipped 用 `reason` 非 `error` | `patch.go:29,32,85`；`filetool.go:204-211`；契约 `:74` | 依赖示例/文案的解析失效 |
| P2 | 插件配置 `maxTargets` 默认 200 静默截断；async/timeout 元数据未透出 | `main.go:145-149`；`config.go:42`；框架 `contract/contract.go:72-80`；主仓 `src/initdata/capability/plugins/file/manifest.json` | 与其它 file 工具同源问题 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 重写 `applyUnifiedDiff`：按 `@@ -a,b +c,d` 校验全部上下文/删除行，并校验行数与新起止；不匹配即 failed | 优化 | 消除静默错写（最高优先） | 中（patch.go 单文件 + 测试） | 收紧：此前“能过”的坏补丁会失败（正确行为） | 建议现在做 |
| 先判 `@@` 块内状态再识别 `---/+++`（仅在 hunk 外识别文件头），避免吞 `+++`/`---` 内容行 | 优化 | 不丢合法增删行 | 低 | 无 | 建议现在做 |
| 统一行尾：读入时归一 CRLF 或比对时同时去 `\r`，输出按原文件风格写回 | 优化 | CRLF 文件可用 | 中（需记录原风格） | 行为变化 | 建议现在做 |
| 接入跨进程锁，锁内 读→expectMd5→apply→write | 优化 | 兑现契约 | 中 | 同 replace | 建议现在做 |
| 拒绝二进制文件；对超过阈值（如 8 MB）的文件直接提示改用 `file_replace` | 改善 | 防损坏、内存可控 | 低 | 收紧 | 排期做 |
| 失败信息附“hunk 序号 + 期望/实际行”并示例下一步（重读文件/重新生成 patch） | 改善 | 可恢复 | 低 | 无 | 建议现在做 |
| 同批重复 `path` 直接 failed（与 write 对齐） | 改善 | 语义一致 | 低 | 收紧 | 排期做 |

## 8. 建议的测试/验收（不执行）

- 构造两 hunk 补丁，第二 hunk 首行匹配但后续 context 被改动：期望整项 failed（当前会 ok 并写错）。
- 新增一行内容为 `++i;`（diff 行 `+++i;`）：验证该行是否保留。
- 删除一行内容为 `--flag`：验证是否真的删除。
- CRLF 文件 + 标准 `\n` 补丁：记录当前失败/混合换行行为，验收目标为可用且行尾保持。
- 补丁头写另一个文件路径：按契约应 failed（当前 ok）。
- `@@ -1,3 +1,3 @@` 行数与实际不符：验证是否拒绝。
- 同一批两次 patch 同一文件：验证重复检测/顺序语义。
- 并发两个 patch + 同一 `expectMd5`：验证锁语义。
- 二进制文件 patch：验证拒绝。
- 取消发生在第 2 个 target：验证已写文件与返回。

## 9. 基线修正

- 基线 §6（契约 async/timeout 决定 gateway）在本插件不成立：`contract.Parse` 只取 name/title/hot/schema，实际缺省 auto + manifest 300s。
- 基线 §3 写工具集合：`file_patch` 属 7 个写工具之一，沙箱预检生效；`file_diff/find/detect/read` 只读。
## 10. 修复记录（2026-11-08）

| 原问题 | 修复 | 验证 |
|---|---|---|
| P1 `expectMd5` 无锁 | `patchOne` 改为 `lockTarget` 加锁 → 读 → `expectMd5` → 应用补丁 → 原子写 → 释放；批量逐项加锁 | `TestPatchReturnsOnlyStatusAndCount` |
| P2 skipped 用 `reason`，契约示例用 `error` | `itemSkip` 同时写 `reason` 与 `error`；保留当前 `md5` | `TestReplaceSkipHasReasonAndError` |
| P2 无二进制拒绝 | `loadDecoded` strict 检测：二进制 → `failed: binary file (not supported)` | `TestReplacePatchBinaryRejected` |
| P2 解码与回写可能跨编码 | `loadDecoded` 用 `encodingx.AutoName` 取编码名、`decodeWithEncoding` 按同一编码名解码；回写复用该名，保证“同一编码读→改→写” | `TestPatchPreservesSourceEncoding` |
| P2 不检测同批重复 path | 与 `file_write` 对齐，后者 `failed: duplicate target` | `TestReplacePatchDuplicateTarget` |
| P2 返回 md5Before/md5After 冗余 | 成功项只回 `status + hunks`；不返回 md5/diff | `TestPatchReturnsOnlyStatusAndCount` |
| P2 maxTargets / async 元数据 | 前两批已修 | `TestTargetsLimitSingleSource` 等 |

P0/P1（hunk 全量校验、`+++`/`---` 内容行、CRLF）已在 2026-11-08 前一批修复；本批回归覆盖 `TestDiffPatchRoundTrip` 等既有用例。

## 11. 独占打开改造（2026-11-09）

- `patchOne` 删除 `lockTarget` 加锁与 `writeAtomic`：改为 `openExclusiveExisting`（Windows `dwShareMode=0`；共享冲突 50/100/200ms 重试 3 次，总尝试 = 初次 + 3 次）→ `io.ReadAll` 读旧内容 → `expectMd5` 用 `md5.Sum(oldData)` 校验 → 应用补丁 → `writeInPlace`。
- 就地写不具备 rename 的崩溃原子性；写失败时用内存中的 `oldData` 回滚（Seek 0、写回、Truncate、Sync）。回滚成功 → 普通 failed；回滚失败 → 该项 `partial:true` + 明确错误。
- 成功返回不变：只有 `status + hunks`。
- 非 Windows 保留“临时文件 + rename”原子写；`lockTarget` / `acquireTargetLock` 保留给 copy/move 使用。
- 测试：`TestPatchReturnsOnlyStatusAndCount` 移除锁断言；新增 Windows-only `TestPatchFileInUse`；原 patch 功能/编码/CRLF 测试继续通过。

## 12. 同路径合并与锁调度（2026-11-09）

- **同路径合并**：`runPatch` 先解析全部路径、按解析后路径分组；同一文件多个 target 只**独占打开一次、读一次、写一次**。组内按 `targets[]` 输入顺序依次 `ApplyFileDiff`（每项各自返回 `hunks`）。
- **文件级全有全无**：组内第 k 项 patch 应用失败 → 第 k 项 `failed`，同组其他项 `skipped`（`reason:"not_applied"`），该文件**不写**；打开/读/解码/编码失败 → 同组所有项 `failed`；写失败用 `oldData` 回滚，回滚失败 → 同组所有项 `partial:true`；`patch` 为空的 target → 该 target `failed`，其余 skipped，不写。
- **expectMd5**：同组多个不同的非空 `expectMd5` → 同文件所有项 `failed`（`reason`/`code` = `conflicting_expectMd5`）；相同值只对**原始内容**校验一次（所有修改前）；不符 → 同组所有项 `skipped` + 当前 `md5`。
- **批调度（`src/lib/filetool/batchlock.go`）**：第一遍按入参顺序对每个路径组只尝试一次独占打开（`openExclusiveExistingOnce`，内部不再 sleep/重试）；`errFileInUse` 记 pending、继续后面的文件；其余文件处理完后对 pending 做 3 轮 50/100/200ms 重试；仍失败 → 同文件所有项 `failed` + `code:"FILE_IN_USE"`。fatal 立即填该组；每个文件处理完立即释放句柄。
- `patchOne` 由 `patchGroup`（实际实现为共享的 `runEditGroup` + `applyPatch`）取代；`editWriteFailure` 改为 group 版本（同组所有项 failed，回滚失败同组 `partial`）。
- **结果顺序**：`results` 长度 = 入参 `targets` 长度，按入参顺序预分配、按索引填充；分组不合并结果。
- **测试**：新增 `TestPatchSamePathSequential`（两次 patch 依赖前一次结果）、`TestReplaceGroupExpectMd5`（replace/patch 共用分组实现）；`TestReplacePatchDuplicateTarget` 改为 grouped/两项 ok 断言；原编码/CRLF/上下文校验测试继续通过。§10 的“重复 path → duplicate target”和 §11 的“打开内部重试 3 次”由本节取代。

## 13. 默认字符集与 ANSI 转换（2026-11-11）

- **自动识别**：`decodeSource` 改为 BOM → 严格 UTF-8 → chardet（`autoEncodeNameStrict`）→ 低置信度时若配置了默认字符集且 `looksLikeANSI`（按 GBK 解码可读且像中文）按 GBK → 最后才回落 `encodingx.AutoName`；二进制仍先拒绝。
- **ANSI → 默认字符集**：源文件识别为 Windows 中文 ANSI（GBK/GB18030）且插件配置页设置了默认字符集时，写回统一转为默认字符集；其他编码保持原编码。`file_replace` 不走该转换（保持源编码）。
- **返回**：成功项新增 `encoding` = 实际写回的编码（`patch` 可能是转换后的默认字符集）。
- **配置**：只消费顶层 `defaultEncoding`（由 `file_patch` 入口校验，非法值参数错误），不再由 `applyDefaults` 逐 target 注入 `encoding`。
- 测试：`TestPatchAnsiToDefaultEncoding`（GBK 源 → 默认 UTF-8 写回；无默认字符集时保持 GBK）。
