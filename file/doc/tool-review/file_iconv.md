---
plugin: file
tool: file_iconv
contract: file/src/tools/file/file_iconv.tool.md
handler: file/src/lib/filetool/query.go:382-473
reviewed_at: 2026-11-08
status: verified
---

# file_iconv 审查

## 1. 一句话结论
**有风险（P1）**：自动检测 + 低置信度拒绝 + 原子写方向正确，但契约承诺的“`expectMd5` 锁内校验”**没有锁**（`fileops/lock.go` 全仓无调用方），且 `idempotent: true` 在显式 `from` 时不成立——超时重试可能把 UTF-8 当 GBK 解码后写回，**损坏文件**。

## 2. 调用链
`filePlugin.call` → `applyDefaults`（`main.go:126-150`；配置了默认编码时会向每个 target 注入 `encoding`，本工具不消费）→ `dispatch` → `checkSandboxWritable`（`file_iconv` ∈ `writeTools`，按 `targets[].path` 校验）→ `toolset.Dispatch` → `runIconv`（`query.go:383`）→ 逐项 `resolvePath(path)` → `iconvOne`（`query.go:401`）→ `writeAtomic`（`filetool.go:162-191`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 300`、`idempotent: true`、`hot: true`（契约 3-12 行）。async/timeout/idempotent 在插件面不透出（C1）。
- “`from` 缺省 auto：复用 `file_detect` 检测链；低置信度 → failed `encoding_undetermined` 并附候选”（契约 21-22 行）：实现一致（`query.go:418-431`），但错误文案带后缀 `encoding_undetermined (specify from explicitly)`，与返回示例的精确字符串不同。
- “源已是目标编码 → skipped + reason=already_target，不重写”（契约 31 行）：实现只比较**声明/检测到的 `from` 与 `to`**（`query.go:433-435`），不校验文件真实内容；显式 `from` 时因此可能误跳或误转（见 P1）。
- “`expectMd5` 可选：**锁内校验**，不一致 → skipped”（契约 33 行）：实现只做一次读后比较（`query.go:411-414`），**无任何锁**。
- “`bom=keep` 原文件有 BOM 则保留到目标编码”（契约 27 行）：legacy 目标编码（gbk/gb18030/shift_jis/big5/euc-kr）实际不写 BOM，keep 退化为 strip（`query.go:442-452`、`readwrite.go:753-768`）。
- 返回示例 `bom: "none"`；实现在有 BOM 时把 `bom` 写成**目标编码名**（`query.go:461-465`），示例未覆盖该取值。
- schema `to` 的 description 列出 8 种编码，而 `normalizeEncoding` 接受更多别名；不支持的名字会得到 `to is required`（`query.go:403-405`）。
- LLM 可见描述仅首段（C4）：低置信度拒绝、BOM 三态、幂等规则不在工具描述；`from`/`bom`/`expectMd5` 的 description 部分补偿。

## 4. 实现面
- 参数：`to` 归一化后为空或 auto → item failed `to is required`（`query.go:403-405`）。
- 读取：`os.ReadFile(path)` 整文件入内存（`query.go:407`）；随后 `md5File(path)` **再次整读**（`query.go:411`）。
- 并发保护：**无**；`fileops/lock.go` 的 `acquireLock` 系列在整个插件内无调用方（`grep` 无命中）。
- 期望校验：`expectMd5` 非空且与当前 md5 不等 → skipped，附当前 `md5`（`query.go:412-414`）。
- 源编码：auto → `detectEncoding(oldData)`（BOM/UTF-8/GBK/ShiftJIS/Big5/EUC-KR + 常用字打分，`filetool/encoding.go:36-70`）；binary → failed；`confidence=="low"` → failed 并附 `candidates`（编码名数组）；显式 `from` 不做检测，直接 `decodeByName`（`query.go:416-439`）。
- 目标/BOM：`bom` 缺省 keep → `wantBOM=hasBOM(oldData)`；`add` → true；`strip` → false（`query.go:442-451`）。
- 编码：`encodeWithEncoding(text, to, wantBOM)`；utf-8/utf-16 支持 BOM，legacy 恒 false（`readwrite.go:753-768`）。
- 写入：`writeAtomic`（同目录临时文件 + `Sync` + `Rename`），单文件原子（`query.go:456-458`）。
- 硬限制：无文件大小上限（内存 ≈ 原文 + 解码字符串 + 编码后字节）。
- 副作用：原地覆盖目标文件；不改其他文件。
- 幂等：auto 模式下成立；显式 `from` 模式下**不成立**（见 P1）。
- 清理：无临时文件残留（writeAtomic defer 删除）。

## 5. 返回面：LLM 实际看到的所有形态
1. **成功**：`{results:[{path,status:"ok",from,to,bom:"none"|to,bytesBefore,bytesAfter,md5Before,md5After?}],summary{...}}`，isError=false。
2. **跳过（已目标编码）**：`{path,status:"skipped",from,to,reason:"already_target"}`，文件不重写。
3. **跳过（md5 不符）**：`{path,status:"skipped",md5:<当前值>,reason:"md5 mismatch"}`；注意字段名是 `md5` 不是 `md5Before`。
4. **失败（自动检测低置信）**：`{path,status:"failed",error:"encoding_undetermined (specify from explicitly)",candidates:["gbk","shift_jis","big5"]}`——恢复路径明确：显式指定 `from`。
5. **失败（其他）**：`binary file (not a text encoding)`、`decode_error: ...`、`encode_error: ...`、`write: ...`、`to is required`、OS 读错误、路径规则消息。
6. **部分成功 / 全部失败**：逐项结果；全失败仍 isError=false（C3）。
7. **截断**：不截断；targets 超配置上限（默认 200）静默丢弃（C2）——批量转码会“少转”，返回无提示。
8. **参数级失败**：targets 形状违规 → 纯文本 isError=true。
9. **沙箱拒绝**：整次调用纯文本 isError=true，未写入任何文件。
10. **异步/超时/取消**：C1 适用时先返回后台受理；ctx 结束后返回 `context canceled`/`context deadline exceeded`（isError=true），但 `writeAtomic` 仍会完成——**结合 P1 的幂等缺陷，模型重试可能二次转码损坏文件**。
11. **内部错误**：`tool file_iconv panic: ...`、`marshal result: ...`（isError=true）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 契约“锁内校验”不成立：全插件无锁调用，`expectMd5` 是 read-then-write 的 TOCTOU | 契约 33 行；`query.go:411-456`；`fileops/lock.go:31-40`（无调用方，grep 无命中） | 并发编辑被静默覆盖；契约给出的安全承诺是假的 |
| P1 | 显式 `from` 时幂等不成立且危险：第二次调用把已是 UTF-8 的文件按 GBK 解码（替换非法字节）后写回，内容被破坏 | `query.go:416-435`（只比 from/to，不检测真实编码）；契约 10、31 行 | 超时/取消后重试即数据损坏；`idempotent:true` 误导重试逻辑 |
| P1 | 整文件多次入内存（ReadFile + md5File 再读 + 解码字符串 + 编码结果），无大小上限 | `query.go:407-452` | 大文件 OOM；两次 I/O 放大耗时 |
| P1 | 超时/取消后 `writeAtomic` 继续，返回失败但文件已转换 | `toolset/toolset.go:58-63`；`query.go:456` | 与 P1 幂等缺陷叠加 → 重试损坏 |
| P1 | 沙箱仅路径前缀校验，不解析 symlink/junction | `sandbox.go:49-55` | 可把写入重定向到白名单外（**安全**，`AllowsPath`[待核实]） |
| P2 | 不支持的 `to`（如 utf-32）报 `to is required`，误导模型以为漏传 | `query.go:403-405` | 恢复路径错误（应报 unsupported encoding） |
| P2 | `from==to` 直接 skipped，不验证文件真实编码；若声明错误则“幂等跳过”掩盖问题 | `query.go:433-435` | 模型以为无需处理，实际文件不是该编码 |
| P2 | `bom=keep` 对 legacy 目标编码静默丢 BOM（能力限制未在契约说明） | `query.go:442-452`；`readwrite.go:753-768` | BOM 语义与文档不符 |
| P2 | `bom` 成功返回目标编码名（如 `"utf-8"`），失败/无 BOM 返回 `"none"`，取值域未文档化 | `query.go:461-465`；契约 87 行 | 模型无法稳定解释该字段 |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 显式 `from` 时也做一次轻量校验（解码后重编码是否 round-trip 一致），不一致则 failed `from_mismatch` | 优化 | 修复幂等/重试破坏，模型可恢复 | 中（1 文件） | 新增错误码；可能拒绝既有宽松用法 | 建议现在做（正确性优先） |
| 真正使用 `fileops` 锁（或删除契约“锁内校验”表述） | 优化/改善 | 契约与实现一致；防并发覆盖 | 低（调用 lock.go 或改文案） | 锁会引入等待/超时 | 建议现在做（二选一） |
| 改为流式转码 + `maxFileBytes` | 优化 | 防 OOM、可预期失败 | 中 | 新增参数 | 排期做 |
| 不支持编码返回 `unsupported_encoding` + 支持列表 | 改善 | 恢复路径明确 | 低 | 文案变化 | 建议现在做 |
| 返回增加 `alreadyTargetVerified`/真实检测编码，BOM 取值改 `"none"|"utf-8"|"utf-16le"` 明确化 | 增强 | 结果可解释 | 低 | 新增字段 | 排期做 |

## 8. 建议的测试/验收（不执行）
- 显式 `from=gbk` 对 UTF-8 文件连续调用两次：断言第二次不会破坏内容（当前预期会）。
- auto 模式对 GBK 文件连续两次：断言第二次 skipped。
- 并发两个调用（不同 `expectMd5`）同一文件：断言锁语义（当前预期无保护）。
- 8MB/1GB 文件：断言内存与失败行为。
- `bom=keep/add/strip` × `utf-8/utf-16le/gbk`：断言 BOM 字节与返回 `bom` 字段。
- `to=utf-32`：断言错误码可恢复。
- 300 targets：断言 C2 截断。

## 9. 基线修正
无。基线 3/4 成立：`file_iconv` 属于 `writeTools`，按 `path` 做沙箱预检（`sandbox.go:23-31`）。仅需注意：`applyDefaults` 会把配置的默认编码注入 `targets[].encoding`（`main.go:132-143`），而本工具读取的是 `from`，该注入对本工具无副作用。
## 10. 修复记录（2026-11-09）

本次只改 `file_iconv`（`query.go` 的 `runIconv` / `iconvOneOnce`）及其契约/测试；write/replace/patch 的批调度与独占打开复用、未改其业务行为。

| 原条目 | 处理 | 实现/证据 |
|---|---|---|
| P1 `expectMd5` “锁内校验”不成立（read-then-write TOCTOU） | **已修** | `openExclusiveExistingOnce` 独占打开 → `io.ReadAll` 一次读入 → 在句柄内、任何修改前比较 `md5Hex(oldData)`；不符 → `skipped` + 当前 `md5`。文件名 `md5File` 二次读已移除。 |
| P1 显式 `from` 时幂等不成立，重试把 UTF-8 当 GBK 解码写回 | **已修** | 显式 `from` 先做 actual detection：置信度非 `low` 且 actual==`to` → `skipped already_target`；否则 decode(from)→encode(from) round-trip，必须与原字节完全一致（含编码失败也算不成立）→ 否则 `failed from_mismatch` 并附 `detected`/`candidates`，**绝不写**。`from == to` 同样按该规则判定。 |
| P1 整文件多次入内存、无大小上限 | **部分处理** | 已去掉 `os.ReadFile` + `md5File` 双读，改为独占句柄 `io.ReadAll` 单次读入、md5 用 `md5Hex(oldData)`；仍为整文件入内存，**未新增文件大小上限**。 |
| P1 超时/取消后写继续，返回失败但文件已转换 | **未在本次范围** | 属 toolset 跨工具问题；本次幂等修复消除了“重试再转一次”的破坏路径，但仍不承诺取消后可回滚。 |
| P1 沙箱仅路径前缀校验 | **复核为误报、未改** | 见 `00-index.md`：`sandbox.Policy.AllowsPath` 已使用 `RealPath`/`EvalSymlinks`。 |
| P2 不支持的 `to`（如 utf-32）报 `to is required` | **已修** | 空/`auto` → `to is required`；不支持的名字 → `failed` `unsupported_encoding: <to>`；`from` 同理报 `unsupported_encoding: <from>`。 |
| P2 `from==to` 直接 skipped、不验证真实编码 | **已修** | actual 可靠等于目标，或 round-trip 一致 → `skipped already_target`；否则 `from_mismatch`。 |
| P2 `bom=keep` 对 legacy 目标静默丢 BOM | **已修（契约化）** | legacy 目标（gbk/gb18030/shift_jis/big5/euc-kr）`keep`/`add` 实际 `bom:"none"`；显式请求了 BOM 时额外返回 `bom_dropped:true`，契约已注明。 |
| P2 `bom` 成功返回目标编码名、取值域未文档化 | **已修（契约化）** | 契约明确 `bom` ∈ `"none"` 或目标编码名，并说明 add 仅 UTF-8/UTF-16 生效。 |

**额外修复（本次改造引入的配套项）**
- 二进制拒绝只看 `oldData` 头部（`encodingProbeBytes`）：`failed binary file (not a text encoding)`，显式 `from` 不能绕过。
- 批调度与 write/replace/patch 一致：先解析全部路径；重复 `path` → 后项 `failed duplicate target`（不合并/last-wins）；每个唯一路径第一遍单次独占打开，`errFileInUse` 记 pending 并继续其他文件，之后 3 轮 50/100/200ms 重试，仍失败 → `failed` + `code:"FILE_IN_USE"`；`results` 按入参顺序预分配、按索引填充，`summary.total` = 入参数量。
- 写入改为 `writeInPlace`：Windows 独占句柄就地写，失败用 `oldData` 回滚、回滚失败 `partial:true`；非 Windows 仍为 temp+rename 原子写（契约注明）。
- 成功返回 `md5After=md5Hex(data)`（写后不再二次打开文件）；不再返回 diff。
- 测试：`TestIconvFromMismatchNoCorruption`、`TestIconvIdempotentSecondCall`、`TestIconvAutoTwice`、`TestIconvBOMSource`、`TestIconvExpectMd5`、`TestIconvUnsupportedTo/From`、`TestIconvDuplicateTarget`、Windows-only `TestIconvFileInUse`。

## 11. 流式改造记录（2026-11-09）

目标：把 `file_iconv` 从“整文件 `io.ReadAll` 入内存”改为**流式转码**；保持既有语义（独占打开、批调度、expectMd5、already_target、from_mismatch、BOM 规则、返回字段）不变，仅替换大数据路径。**O(1) 内存，不新增任意 `maxFileBytes`**；允许使用目标同目录临时文件。

### 11.1 实现文件
- `src/lib/filetool/streamcodec.go`（新增）：`newStreamDecoder` / `newStreamEncoder`（utf-8 原样、utf-16 用 `xunicode`、legacy 复用 `legacyDecoder` 并新增 `legacyEncoder`）、`streamConvert`（`io.SectionReader` → `transform.Reader` 链 → 64 KiB `io.CopyBuffer`）、`filesEqual`（64 KiB 分段比较）、`skipBOMFor`、临时文件 `iconvTemp`、提交结果类型与测试钩子 `commitStreamHook`。
- `src/lib/filetool/streamcommit_windows.go`（新增，`//go:build windows`）：就地提交 `Seek(0)` → `io.Copy` → `Truncate` → `Sync`；失败用 backup 回滚，回滚失败 `partial:true`。
- `src/lib/filetool/streamcommit_other.go`（新增，`//go:build !windows`）：`Sync` → `Close` → `Chmod(目标 mode)` → `Rename`；失败目标不变、不 `partial`。
- `src/lib/filetool/query.go`：只重写 `runIconv` / `iconvOneOnce` 的数据路径；`runIconv` 的解析、去重、批调度与结果填充未改。新增 `iconvFromMismatch` / `iconvStreamError` 两个局部映射辅助。
- `src/lib/filetool/iconv_stream_test.go`、`iconv_stream_windows_test.go`（新增）：大文件流式、from_mismatch、提交失败回滚、Windows partial。
- 契约 `src/tools/file/file_iconv.tool.md` 与本文档同步更新；`00-index.md` 追加说明。

### 11.2 流式流程（iconvOneOnce）
1. 参数 / `to` / `from` 校验、独占打开、`ReadAt` 读头部（`encodingProbeBytes`）做二进制与 actual detection：与旧实现一致；**不再整文件读入**。
2. `already_target` 快判（auto from==to；显式 actual reliable==to）在**创建临时文件之前**完成。
3. `backup`：`Seek(0)` → `io.Copy(io.MultiWriter(backup, md5), f)`，得到 `bytesBefore` / `md5Before`；`expectMd5` 在 backup 之后、修改之前校验（锁内）。
4. 显式 `from`：`roundTrip` 临时文件 ← `decode(from)` → `encode(from, 源 BOM 策略)`；用 `filesEqual` 与 backup **分段比较**。不一致或编码失败 → `from_mismatch`（附 `detected` / `candidates`），文件绝不被修改。
5. `out` 临时文件 ← `decode(from)` → `encode(to, wantBOM)`，同时用 `MultiWriter` 计算 `bytesAfter` / `md5After`。
6. 提交：Windows 就地写入并可用 backup 回滚；非 Windows 原子 `rename`。所有临时文件 `defer` 关闭并删除；`out` 被 `rename` 后删除为 no-op。

### 11.3 保持的语义与 BOM 细节
- `bytesBefore` / `bytesAfter` / `md5Before` / `md5After` / `from` / `to` / `bom`（`"none"` 或目标编码名）/ `bom_dropped`、`skipped already_target`、`skipped md5 mismatch`、`duplicate target`、`FILE_IN_USE`、`encoding_undetermined`、`unsupported_encoding`、`from_mismatch`、`decode_error` / `encode_error` / `write` / `partial` 全部保留。
- `sourceBOM` 来自头部；`skipBOMFor` 仅在与解码方向一致时剥 BOM：utf-8 剥 3 字节，utf-16le/be 各剥 2 字节；显式 `from` 与源 BOM 不一致时不跳，交给 round-trip 暴露。
- `round-trip` 的 BOM 策略与旧实现一致：源有 BOM（`sourceBOM != ""`）即按 `from` 重新输出 BOM；源 BOM 与 `from` 不一致时输出与原字节不同，从而稳定得到 `from_mismatch`。目标转换的 `wantBOM` 仍由 `bom=keep/add/strip` 决定：utf-8 由调用方写 BOM、utf-16 由 encoder `UseBOM`、legacy 忽略并 `bom_dropped`。
- 内存：所有路径仅使用固定大小缓冲；代码中不再出现整文件 `io.ReadAll` / `make([]byte, len(oldData))`。

### 11.4 测试与验收
- 新增 `TestIconvLargeStreaming`（>4MiB GBK→UTF-8、UTF-8→UTF-16LE；断言内容、`bytesBefore/After`、`md5Before/After`、临时文件清理）、`TestIconvStreamingFromMismatch`（>4MiB UTF-8 显式 `from=gbk` → `from_mismatch`、文件不变、临时文件清理）、`TestIconvStreamingAutoLarge`（>4MiB UTF-16LE BOM auto→UTF-8）、`TestIconvStreamingCommitFailure`（注入提交失败 → 目标恢复、非 partial、临时文件清理）、Windows-only `TestIconvStreamingRollbackPartial`（提交+回滚均失败 → `partial:true`）。
- 既有 iconv 测试（BOM、already_target、expectMd5、in-use、duplicate、unsupported、from_mismatch）全部继续通过。
- `src/lib` 与 `src` 两个模块的 `gofmt -l .`、`go vet ./...`、`go test -count=1 ./...` 通过；`GOOS=linux go build ./...` 交叉编译通过（非 Windows 提交路径）。
- 未改 read/write/replace/patch/copy/move/delete/diff/find/detect 的业务行为；未 commit/push、未重建 exe。
