---
plugin: file
tool: file_write
contract: tools/file/file_write.tool.md
handler: src/lib/filetool/readwrite.go:640
reviewed_at: 2026-11-08
status: verified
---

# file_write 审查

## 1. 一句话结论

有风险：单文件原子写可用，但契约承诺的“跨进程文件锁”在实现中完全缺失；“已有文件按原编码回写”在低置信度检测时会静默改成 UTF-8。

## 2. 调用链

LLM → gateway `doCall`（剥离 `tool_call_display_name`，按 manifest `timeout_sec=300` / `_meta.async` 缺省 auto 裁决）→ 插件 MCP `tools/call` → `contract.Register`（`contract/contract.go:106`）→ `filePlugin.call`（`src/main.go:88`）→ `applyDefaults`（`src/main.go:126-150`：注入 `maxBytes`、逐 target 注 `encoding`、超 `MaxTargets` 截断）→ `dispatch`（`src/main.go:97`）→ `checkSandboxWritable`（`sandbox.go:37`，写工具预检）→ `toolset.Dispatch`（`lib/toolset/toolset.go:36`，goroutine + ctx select）→ `filetool.runWrite`（`lib/filetool/readwrite.go:640`）→ `writeOne`（`:665`）→ `writeAtomic`（`:162`：临时文件 + rename）。

## 3. 契约面

- YAML：`async: never`、`timeout: 120`、`idempotent: false`（`file_write.tool.md:6-12`）。`async/timeout` 不被插件契约解析器读取（框架 `contract/contract.go:72-80`），运行时实际为 gateway 缺省 auto + manifest 300s。
- schema：顶层仅 `targets`，数组 1..2000；每项必填 `path/content`，可选 `encoding/bom/createDirs/overwrite`，`additionalProperties:false`（`:30-74`）。`bom` enum 三值、`overwrite` 描述“缺省 true”（`:57-69`）。
- 摘要承诺（`:16-27`）与实现对照：
  - “沙箱白名单，越界整次拒绝（未做任何写入）”——**成立**（`sandbox.go:37-58`）。
  - “原子写，先临时文件再 rename”——**成立**（`readwrite.go:162-191`）。
  - “`overwrite=false` 且目标已存在 → failed”——**部分成立**（存在性由 `os.ReadFile` 是否报错推断，见 §6）。
  - “`createDirs` 默认 true”——成立（`:680-684`）。
  - “`bom=keep` 保持原文件 BOM 状态；新文件无 BOM”——成立（`:697-703`）。
  - “`encoding` 缺省 auto：已有文件按原编码回写；新文件 UTF-8”——**不完全成立**（低置信度回落 UTF-8，见 §6）。
  - “同路径写入自动串行（跨进程文件锁）”——**不成立**（无任何加锁调用）。
  - “批量内重复路径 → failed（duplicate target）”——成立，文案为 `duplicate target in one call`（`:654-657`）。
- 返回示例（`:78-93`）字段 `action/size/md5/encoding/bom` 均存在；`bom` 取值示例只给 `none`，未定义有 BOM 时的值。

## 4. 实现面

- 参数：`parseRequest` 只校验 `targets` 形状（`filetool.go:75-96`）；每项 `content` 必须是字符串，否则该项 failed（`readwrite.go:666-669`）；`bom` 非 enum 值时静默按 keep/strip 处理（无校验）。
- 默认值注入：`applyDefaults` 对**所有工具**注入顶层 `maxBytes`（write 不消费，无副作用）；`DefaultEncoding` 注入每个 target 的 `encoding`；`targets` 超过 `cfg.MaxTargets`（默认 200）直接切片截断（`main.go:126-150`、`config.go:42`）。
- 沙箱：`file_write` 在 7 个写工具集合内（`sandbox.go:23-31`）；只检查 `targets[].path`，预检整次拒绝。
- 执行：先 `os.ReadFile(path)` 取旧内容与存在性 → `overwrite` 判定 → `MkdirAll` → 决定编码/BOM → `encodeWithEncoding` → `writeAtomic`。
- 编码：`auto` 时 `autoEncodeNameOf(oldData)`：UTF-16 BOM → 原编码；严格 UTF-8 → `utf-8`；否则 `detectEncoding` 置信度非 low → 该编码；否则回落 `utf-8`（`readwrite.go:729-745`）。
- BOM：`add` 仅对 UTF-8/UTF-16 生效；legacy 编码 `encodeByName` 恒返回无 BOM（`:753-768`）。
- 并发/幂等：无锁、无版本号；同路径并发为 last-writer-wins；同一调用内按**解析后的绝对路径**去重。
- 原子性：单文件原子（临时文件 + rename），**整批非事务**；中途取消/失败会留下部分已写文件。
- 清理：临时文件 defer 删除；rename 成功后置 `cleanup=false`（`:170-189`）。
- 副作用：创建父目录（`0755`）、覆盖文件、消耗同目录临时文件；重写会丢失原文件权限位/ACL（`os.CreateTemp` 默认权限）[推断，平台相关]。
- 资源：为判定编码/存在性整文件读入内存，无大小上限（`readwrite.go:672-676`）。

## 5. 返回面：LLM 实际看到的所有形态

统一外壳：`cli.Ok(json)` → MCP `{content:[text], isError:false}`；`cli.Err(msg)` → `{text: msg, isError:true}`。

| 形态 | isError | 完整字段/文本 |
|---|---|---|
| 全部成功 | false | `results[*]={path,status:"ok",action:"created|modified",size,encoding,bom,md5?}` + `summary{total,ok,failed,skipped}` |
| 部分成功 | false | 同上，失败项为 `status:"failed",error:...`；`summary` 计数 |
| 全部失败（运行期） | **false** | 每项 failed（如 `target exists (overwrite=false)`），整体仍是 JSON |
| 参数级失败 | true | `targets is required` / `targets must be an array` / `targets length must be 1..2000` / `targets[i] must be an object`（`filetool.go:75-96`）；或 `参数解析失败: ...`（`contract/contract.go:109-111`） |
| 沙箱越界 | true | 纯文本：`sandbox: 目标路径不在可写白名单内：<path>（本次调用已拒绝、未做任何写入；…读取不受限）`（`sandbox.go:54`） |
| 单项失败 | false（批次） | `status:"failed"` + `error`：`path is required`、相对路径消息、`content is required`、`target exists (overwrite=false)`、`duplicate target in one call`、`create dir: ...`、`write: ...`、编码不支持（`encodingx.Encode` 文案）；目标是目录时无专门判词，落到 `write: ...` |
| 成功但无 md5 | false | `md5` 字段缺失（写后 `md5File` 失败时静默省略，`readwrite.go:723-725`） |
| 重复路径 | false（批次） | 后续项 `status:"failed", error:"duplicate target in one call"` |
| 部分写入后取消/超时 | true | `context canceled` / `context deadline exceeded` 纯文本（`toolset.go:58-63`）；**已完成的 target 已落盘**，调用方拿不到 results |
| 异步已受理 | —（gateway 层） | 契约称 never，但实际 `_meta.async` 缺省 auto：超 300s 可能 `status:"pending", task_id` + “任务已转后台执行”，结果经 mcp-tasks-report 后补（[待核实] 网关路由） |
| 内部错误 | true | `marshal result: ...`；panic → `tool file_write panic: ...` |

体积：返回不含正文，仅元信息；`md5` 为 32 字符。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 契约称“同路径写入自动串行（跨进程文件锁）”，实现没有任何加锁调用；`acquireLock` 只有定义无调用点 | 契约 `:27`；`readwrite.go:640-662,665-727`；`lib/fileops/lock.go:36`（grep 无调用方） | 多 agent/多进程并发写同一文件时 last-writer-wins，丢失更新；不能据契约认为安全 |
| P1 | `overwrite=false` 的“存在”判定 = `os.ReadFile` 成功；文件存在但不可读（权限/被占用）时被判为不存在而直接覆盖 | `readwrite.go:672-679` | 本应拒绝的覆盖变成静默覆盖（数据丢失） |
| P1 | `encoding=auto` 声称“按原编码回写”，但检测置信度 low 时回落 UTF-8 | `readwrite.go:686-692,729-745`；契约 `:24` | GBK/ShiftJIS 等无 BOM 小文件可能被改成 UTF-8，其他读取方乱码 |
| P2 | `bom` 返回字段：有 BOM 时值为编码名（`utf-8`/`utf-16le`），无 BOM 为 `none`；契约未定义取值；未知 `bom` 值静默按无 BOM 处理 | `readwrite.go:697-704,718-722`；契约 `:57-61,:88` | 模型无法稳定判断“是否加/去了 BOM” |
| P2 | 为存在性/编码整文件读入内存，且发生在 `overwrite=false` 拒绝之前 | `readwrite.go:672-679` | 大文件 + 明确拒绝场景仍付出全量 I/O/内存 |
| P2 | 重复路径文案与契约示例不一致（`duplicate target in one call` vs `duplicate target`） | `readwrite.go:654-657`；契约 `:27` | 依赖文案匹配的调用方/提示词失效 |
| P2 | 插件配置 `maxTargets` 默认 200，超过即静默截断（schema 允许 2000） | `main.go:145-149`；`config.go:42,64`；契约 `:43` | 合法批量请求被静默缩水 |
| P2 | `async:never/timeout:120` 未透出到 tools/list，实际 auto + manifest 300s | 框架 `contract/contract.go:72-80`；`contract.go:101-105`；主仓 `src/initdata/capability/plugins/file/manifest.json`（`mcp.timeout_sec=300`） | 契约与运行时的同步/超时语义不一致 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 在 `writeOne`/`writeAtomic` 前后接回 `fileops.acquireLock`（按路径，含批量顺序加锁） | 优化 | 兑现契约、避免并发丢写 | 中（readwrite.go + 锁顺序/死锁处理） | 行为收紧；锁文件可能出现在项目目录 | 建议现在做 |
| 用 `os.Stat`/`Lstat` 判定存在性，`EACCES/EBUSY` 明确 failed | 优化 | 避免不可读文件被覆盖 | 低 | 无 | 建议现在做 |
| `auto` 编码检测低置信度时返回 `failed: encoding ambiguous, pass encoding=...`，而不是猜 UTF-8 | 改善 | 防止静默改编码，给出下一步 | 低 | 行为收紧，需契约说明 | 建议现在做 |
| `bom` 返回布尔/枚举（`present|added|stripped|none`）并在摘要定义 | 改善 | 模型可判断 BOM 结果 | 低 | 字段值变化，需同步契约 | 排期做 |
| 超过 `MaxTargets` 时补 `skipped` 项或 `summary.truncated` | 改善 | 不静默丢任务 | 低 | 新增字段 | 建议现在做 |
| 显式声明批量非事务（或增加 `dryRun`/`atomicBatch`） | 改善 | 模型知道部分写风险 | 中 | 增强项 | 仅记录 |

## 8. 建议的测试/验收（不执行）

- 并发两个 `file_write` 写同一路径：期望串行（当前会交错/覆盖）。
- 写只读文件 + `overwrite=false`：期望 `failed`，不得覆盖。
- 无 BOM 的 GBK/ShiftJIS 小文件 + `encoding:auto`：验证字节编码是否保持。
- `bom` 矩阵：keep/add/strip × utf-8/utf-16le/gbk，验证返回字段与字节。
- 同一批含重复路径（含 `C:\a` 与 `C:/a` 形式）：验证去重。
- 201 个 targets（默认配置）：验证截断可见性。
- 沙箱越界：确认零写入、纯文本 isError。
- 取消/超时发生在第 2 个 target 写入期间：确认已写文件状态与返回。

## 9. 基线修正

- 基线 §6（契约 async/timeout 决定 gateway）在本插件不成立，理由同 `file_read.md` §9。
- 基线 §3 称“沙箱只对写工具生效”：本插件写工具集合经核实为 7 个（`file_write/file_replace/file_patch/file_copy/file_move/file_delete/file_iconv`），`file_read/find/diff/detect` 为只读，与 `sandbox.go:22-34` 一致。
## 10. 修复记录（2026-11-08）

已按审查建议 + 用户口径修复并补测试：

| 原问题 | 修复 | 验证 |
|---|---|---|
| P1 契约称跨进程文件锁，实现无调用 | 导出 `fileops.AcquireLock`；`writeOne` 顺序改为「建父目录 → 加锁 → Stat/覆盖判定 → 读旧文件头 → 编码/BOM → 原子写 → 释放锁」；批量逐项加锁/释放 | `fileops/lock_test.go`、`TestWriteLockFileCleaned` |
| P1 `overwrite=false` 用 `ReadFile` 失败当“不存在” | 改用 `os.Stat` 判存在与目录；`err` 非 `IsNotExist` → `failed`；存在且 `overwrite=false` → 在读内容前拒绝 | `TestWriteStatAndParentGuards`、`TestWriteOverwriteGuard` |
| P1 `encoding=auto` 低置信度回落 UTF-8 | 新增 `autoEncodeNameStrict`：低置信度/二进制 → 该项 `failed`（`encoding ambiguous…`），要求显式 `encoding` | `TestWriteAutoAmbiguousEncodingFails` |
| P2 `bom` 返回取值未定义、未知值静默 | 运行时校验 `keep/add/strip`；返回枚举 `none|utf-8`；**BOM 仅对 UTF-8 输出生效，默认 `strip` 无 BOM**；`keep` 仅旧文件本身 UTF-8 且带 BOM 时保留 | `TestWriteBOMOnlyForUTF8` |
| P2 存在性/编码整文件读入且先于覆盖拒绝 | `Stat` 先判存在/覆盖，之后只为编码/BOM 读 64KiB 头（`readHead`），不再整文件读入 | `TestWriteOverwriteGuard`、`TestWriteAutoAmbiguousEncodingFails` |
| P2 重复路径文案与契约不一致 | 统一为 `duplicate target` | `TestWriteDuplicateTargetMessage` |
| P2 `maxTargets` 默认 200 / 静默截断 | 已在 `file_read` 批次修复（单一来源 2000 + 超限参数错误） | `TestTargetsLimitSingleSource` |
| P2 `async/timeout` 不进 `_meta` | 平台批次已修（`contract.Parse + MetaFor`） | `TestFileReadContractMeta` 同链路 |

## 11. 独占打开改造（2026-11-09）

按用户要求，写入路径不再使用“临时文件 + rename”，Windows 上改为独占打开后**就地读写**：

- 删除 `fileops.AcquireLock` / `<path>.chonk.lock` 与 `writeAtomic` 调用。`writeOne` 改为 `openExclusiveWrite`（已存在则独占打开、不存在则独占创建）→ 从同一句柄读旧文件头判编码/BOM → `writeInPlace`（Seek 0 → 循环写全 → Truncate → Sync）。
- Windows：`CreateFile(..., GENERIC_READ|GENERIC_WRITE, dwShareMode=0, ...)`；仅 `ERROR_SHARING_VIOLATION` / `ERROR_LOCK_VIOLATION` 按 50/100/200ms 重试 3 次（总尝试 = 初次 + 3 次）；`ACCESS_DENIED`、路径错误立即 failed。
- 原子性代价：就地写不具备 rename 的崩溃原子性，进程写一半崩溃可能留下部分内容；`file_write` 无旧内容快照，写失败且已写入部分字节 → 该项 `partial:true`（目标可能不完整）。
- 非 Windows：保留“同目录临时文件 + rename”原子写（实现分离在 `write_in_place_other.go`）；`openExclusive*` 用 `flock(LOCK_EX|LOCK_NB)` 协作锁，保证可编译与串行语义。
- md5 改为 `md5.Sum(data)` 直接计算，不再写后 `md5File`（独占句柄会挡住自己第二次打开）。
- 测试：删除依赖 `.chonk.lock` 的 `TestWriteLockFileCleaned`；新增 `TestWriteInPlaceTruncates` 与 Windows-only `TestWriteFileInUse` / `TestWriteFileInUseRetrySucceeds`；原 write 功能测试继续通过。

## 12. 批处理 last-wins 与锁调度（2026-11-09）

- **同路径 last-wins**：`runWrite` 先解析全部路径、按解析后路径分组；同一路径只执行**最后一个** target（last wins），前面的重复项 `skipped`（`reason`/`error` = `superseded_by_later_target`）。最后一个 target 自身校验/打开/写失败 → 只该项 `failed`，前面的 skipped **不回退执行**。
- **批调度（新文件 `src/lib/filetool/batchlock.go`）**：第一遍按入参顺序对每个 winner **只尝试一次**独占打开（`openExclusiveWriteOnce`，内部不再 sleep/重试）；`errFileInUse` 记 pending、继续处理后面的文件；其余文件全部处理完后对 pending 做 3 轮 50/100/200ms 重试（每轮重试所有仍 pending 的文件）；仍失败 → winner `failed` + `code:"FILE_IN_USE"`。fatal（ACCESS_DENIED、目录、不存在等）立即填该 winner，不进入 pending。
- `writeOne` → `writeOneOnce(path, t) (result, inUse)`：内容校验/建目录/编码/BOM/就地写仍在本函数；独占打开改为 `openExclusiveWriteOnce`，锁失败返回 pending（不填结果）。`md5` 仍用内存 `data` 计算；`partial:true` 语义不变。每个文件处理完立即关闭句柄，同一时刻最多一个文件句柄。
- **结果顺序**：`results` 长度 = 入参 `targets` 长度，按入参顺序预分配、按索引填充（pending 重试不打乱顺序）；skipped/failed 都计入 `summary.total`。
- **测试**：新增 `TestWriteDuplicateLastWins`、`TestResultsInputOrder` 与 Windows-only `TestBatchLockSkipThenRetry`（持有 A 时 A `FILE_IN_USE`、B 仍成功；关闭后可重试成功）；`TestWriteDuplicateTargetMessage` 改为 last-wins/skipped 断言。§10 的“重复路径统一 `duplicate target`”和 §11 的“打开内部按 50/100/200ms 重试 3 次”由本节取代。

## 13. 默认字符集与 ANSI 转换（2026-11-11）

- **配置注入语义变更**：插件不再把 `plugins.file.default-encoding` 逐 target 注入 `targets[i].encoding`（那会让 `file_read` 也失去自动识别）；改为顶层 `defaultEncoding`，只由 `file_write` / `file_patch` 消费。
- **file_write auto 策略**：
  - 新文件：使用 `defaultEncoding`；未配置 → UTF-8；
  - 已有文件：保持原编码；仅原编码为 Windows 中文 ANSI（GBK/GB18030）且配置了默认字符集时，转为默认字符集；
  - 显式 `targets[].encoding` 覆盖以上策略；
  - 旧文件检测低置信度时，若配置了默认字符集且 `looksLikeANSI`（按 GBK 解码可读且像中文）→ 按 GBK 处理；否则仍 `failed` 要求显式 encoding。
- 测试：`TestWriteDefaultEncodingNewAndExisting`（新文件 GBK、UTF-8 旧文件保持、GBK 旧文件转 UTF-8、显式覆盖）。
