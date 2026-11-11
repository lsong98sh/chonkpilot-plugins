---
plugin: file
tool: file_move
contract: file/src/tools/file/file_move.tool.md
handler: file/src/lib/filetool/fsops.go:94-240
reviewed_at: 2026-11-08
status: verified
---

# file_move 审查

## 1. 一句话结论
**有风险（P1）**：同卷 `rename` 快速路径正常，但**任何** `rename` 失败都会回退成“复制+删源”，跨卷语义与错误归因混在一起；删源失败时目标已完整复制而源可能半删，返回值无法描述这种中间态。

## 2. 调用链
`filePlugin.call` → `applyDefaults`（`main.go:126-150`）→ `dispatch` → `checkSandboxWritable`（`file_move` ∈ `writeTools`/`fromToTools`，`from` + `to` 都校验）→ `toolset.Dispatch` → `runMove`（`fsops.go:72`）→ `moveOne`（`fsops.go:93`）→ `os.Rename` 或 `copyPath`+`os.RemoveAll`（`fsops.go:112-121`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 120`、`idempotent: false`、`hot: true`（契约 3-12 行）。async/timeout 不透出（C1）。
- “跨卷移动自动回退为复制成功后删源；复制失败不删源”（契约 20 行）：实现是对**所有** `os.Rename` 错误回退（`fsops.go:112-117`），错误文案固定为 `cross-volume move failed at copy`，与契约的“跨卷”承诺窄于实现。
- “目录移动 = 先递归复制到目标，成功后再逐文件删源”（契约 21 行）：实现是 `copyPath` 后 `os.RemoveAll`（`fsops.go:115-118`），删除粒度为整树，不是“逐文件”。
- “`overwrite` 缺省 false：目标已存在 → 该项 failed”（契约 22 行）与 `fsops.go:103-105` 一致。
- “拒绝 to 落在 from 内部 → 该项 failed”（契约 23 行）与 `fsops.go:95-97` 一致；但 `to == from` 也会被判为 self-nested（见 P2）。
- schema 无 `recursive` 参数（目录总是递归），与契约一致。
- 返回示例含 `type`（契约 64 行），实现确实写 `type`（`fsops.go:102`），但不返回 `size/bytes`。
- LLM 可见描述仅首段（C4）：跨卷回退、覆盖语义、自我嵌套规则不可见。

## 4. 实现面
- 路径：`from`/`to` 各自 `resolvePath`，任一非法 → 该项 failed（`fsops.go:79-86`）。
- 自我保护：`isWithin(to, from)`（含相等）→ failed（`fsops.go:95-97`）。
- 源检查：`os.Stat(from)`（跟随 symlink；不存在 → failed，文案为 OS 错误）→ 写 `type`（`fsops.go:98-102`）。
- 目标冲突：`os.Stat(to)` 成功且 `overwrite=false` → failed；`overwrite=true` 时不删除旧目标，直接交给 rename/覆盖（`fsops.go:103-105`）。
- 建父目录：`createDirs` 缺省 true → `MkdirAll(Dir(to),0755)`（`fsops.go:106-110`）。
- 执行：`os.Rename` 成功即结束；失败 → `copyPath`（文件全量读内存 + 原子写；目录递归）→ 成功后 `os.RemoveAll(from)`（`fsops.go:112-121`）。
- 硬限制：无文件大小/条目上限；无超时中断；无并发控制。
- 副作用：目标写入、源删除（`RemoveAll`，不可恢复）。
- 幂等：`idempotent:false` 正确（重复调用 from 已不存在 → failed）。
- 资源：复制的 goroutine 在 ctx 取消后继续（`toolset/toolset.go:40-63`）。

## 5. 返回面：LLM 实际看到的所有形态
1. **成功**：`{results:[{from,to,status:"ok",type:"file"|"dir"}],summary{total,ok,failed,skipped}}`，isError=false。
2. **部分成功**：失败项 `status:"failed"` + `error`，文案包括：`target is inside source (self-nested move)`、`target exists (overwrite=false)`、`create dir: ...`、`cross-volume move failed at copy (source kept): ...`、`copied but failed to remove source: ...`、路径规则消息。
3. **全部失败**：isError=false，仅 `summary.failed=total`（C3）。
4. **跳过**：本工具不产生 `skipped`。
5. **截断**：不截断；但 targets 超配置上限（默认 200）时静默丢弃（C2），LLM 无法从结果看出有项未处理。
6. **参数级失败**：targets 形状违规 → 纯文本 isError=true。
7. **沙箱拒绝**：整次调用纯文本 isError=true，未执行任何 rename/delete。
8. **异步/超时/取消**：C1 适用时先返回后台受理；ctx 结束后返回 `context canceled`/`context deadline exceeded`（isError=true），但 `os.Rename` 或“复制+删源”仍在进行，最终磁盘状态可能与返回不符。
9. **中间态（重点）**：复制成功但 `RemoveAll` 失败 → 该项 `status:"failed"` + `copied but failed to remove source`；此时目标完整、源可能部分删除，结果里没有任何 `sourceRemoved`/`leftover` 字段供模型判断。
10. **内部错误**：`tool file_move panic: ...`、`marshal result: ...`（isError=true）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 任意 `os.Rename` 错误都回退为复制+删源，错误一律写成 “cross-volume”；权限/占用/目标为目录等错误被误报，且可能由“失败”变成“成功副本 + 删源” | `fsops.go:112-121`；契约 20 行 | 错误归因错误；模型按“跨卷”推理会误判原因；潜在的重试破坏性 |
| P1 | 复制成功 + 删源失败返回 failed，但目标已完整、源可能半删，无状态字段 | `fsops.go:118-120` | 模型无法判断是否该重试；重试可能再次复制/再次半删 |
| P1 | 目录移动非原子（递归复制 → 整树删除）；超时/取消只丢弃结果，不中断 | `fsops.go:115-118`；`toolset/toolset.go:58-63` | 中断后源与目标双份、或源半删；返回与实际不一致 |
| P1 | 沙箱只做字符串路径前缀校验，不解析 symlink/junction；`from` 的 `RemoveAll` 与 `to` 的写入都可能被重定向 | `sandbox.go:49-55` | 白名单可被 junction 绕过（**安全**，`AllowsPath` 内部[待核实]） |
| P2 | `to == from` 被判 `self-nested move` failed；契约未定义，合理行为应为 skipped/no-op | `fsops.go:95-97` | 幂等重命名（同名）失败 |
| P2 | Windows 上 `os.Rename` 目标为已存在目录时失败 → 落入“跨卷复制”路径，报错误导 | `fsops.go:103-117` | 平台相关误报 |
| P2 | 跨卷移动不保留 mtime（复制路径未 `Chtimes`），同卷 rename 会保留 → 行为随卷变化 | `fsops.go:115-117,202-215` | 时间戳语义不一致 |
| P2 | `overwrite=true` 且目标目录已存在时按合并复制再删源，目标中源没有的文件残留 | `fsops.go:115-118,182-196` | 非镜像语义，契约未说明 |
| P2 | 结果无 `bytes`/`files` 计数，目录移动无法确认搬运量 | `fsops.go:93-121` | 审计能力弱 |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 仅对 `EXDEV`/跨卷错误回退；其他 rename 错误保留原始 errno 文案 | 优化 | 错误归因正确，恢复路径明确 | 低（1 文件，平台判定） | 文案变化 | 建议现在做 |
| 复制+删源中间态显式返回 `source_removed:false` / `partial:true` 与剩余源路径 | 改善 | 模型知道“别重试/需人工确认” | 低 | 新增字段 | 建议现在做 |
| 目录移动失败回滚目标树或改“先 rename 目录再复制” | 优化 | 减少双份/半删 | 中 | 回滚有误删风险 | 排期做 |
| `to == from` 改为 skipped（reason `same_path`） | 改善 | 幂等重命名可用 | 低 | 行为变化 | 排期做 |
| 返回 `files/bytes` 统计；契约补平台差异与 mtime 语义 | 改善 | 审计与预期一致 | 低 | 兼容 | 排期做 |
| 沙箱校验前 EvalSymlinks | 优化 | 堵 junction 绕过 | 中 | 严格化 | 建议现在做（安全） |

## 8. 建议的测试/验收（不执行）
- 目标已存在且 `overwrite=true`（文件/目录两种）：断言平台行为与文案。
- 只读源目录 / 被占用文件 rename 失败：断言是否仍尝试复制、错误文案是否仍写 cross-volume。
- 跨卷移动后 `RemoveAll` 注入失败：断言返回字段能否表达“目标已复制、源未删净”。
- 目录 `to` 已存在 + `overwrite=true`：断言残留文件与文档一致。
- 白名单内 junction 源/目标：断言沙箱是否拒绝。
- 300 targets：断言 C2 截断。

## 9. 基线修正
无。基线 3/4 的写工具沙箱模型在本工具成立，且 `from` 被当作删除语义一并校验（`sandbox.go:34,72-79`），比基线更严格。

## 10. 修复记录（2026-11-09）

本次只改 `file_move` 与 copy/move 共用实现（`copyOne`/`ensureDir`/`copyFileAtomic` 的 symlink 防护）、契约、测试与审查文档；`file_write/replace/patch/read/diff/find/iconv/detect` 行为未变。逐条对应原 §6：

| 原问题 | 处理 | 证据 |
|---|---|---|
| P1 任意 `os.Rename` 错误都回退为复制+删源，错误一律写成 “cross-volume” | **只对 `EXDEV` 回退**：`errors.Is(err, syscall.EXDEV)` 才走「复制 + 删源」；其他 rename 错误返回该项 `failed`，`error` 为 `rename: <原始错误>`，不复制、不删源。新增测试钩子 `renameFn`（生产 = `os.Rename`）注入 EXDEV/EACCES。 | `fsops.go` `moveOne`、`renameFn` |
| P1 复制成功 + 删源失败返回 failed，目标已完整、源可能半删，无状态字段 | 复制完成后删源失败 → 该项 `failed + partial:true + source_removed:false + copiedFiles + bytes`，`error` 为 `copied but failed to remove source: ...`。新增测试钩子 `removeAllFn`（生产 = `os.RemoveAll`）。 | `fsops.go` `moveByCopy`、`removeAllFn` |
| P1 目录移动非原子（递归复制 → 整树删除）；超时/取消只丢弃结果 | 目标不存在时目录先走 `os.Rename`（同卷原子；EXDEV 才复制）；目标已存在 + `overwrite=true` 直接复制合并（rename 无法合并）。跨卷复制失败且目标原本不存在 → 回滚新建目标树，源保持完整；目录合并中途失败 → `partial + copiedFiles`。**未处理**：超时/取消后已启动的复制/删源仍会跑完，属 C1/toolset 层问题。 | `fsops.go` `moveOne` / `moveByCopy`；`file_move.tool.md` |
| P1 沙箱只做字符串路径前缀校验，symlink/junction 可绕过 | **原结论更正（误报）**：`sandbox.Policy.AllowsPath` 已对目标与白名单两侧调用 `RealPath`（`filepath.EvalSymlinks`；目标不存在时解析父目录），绕过不成立。另按纵深防御补上**目标树目录 symlink 拒绝**：`ensureDir` 改用 `os.Lstat`，合并时遇到子目录 symlink → `target path traverses symlink (refused)`。 | `chonkpilot-util/sandbox/policy.go`；`fsops.go` `ensureDir` |
| P2 `to == from` 被判 `self-nested move` failed | 先判 same path（Windows 大小写不敏感、分隔符归一；其他平台精确）→ `itemSkip(out, "same_path")`（同时给 `reason`/`error`），源保持不动；仅 `to` 严格位于 `from` 内部才报 self-nested。 | `fsops.go` `moveOne` / `samePath` |
| P2 Windows 上 rename 目标为已存在目录失败 → 落入“跨卷复制”误报 | 目录源 + 已存在目录目标（`overwrite=true`）不再尝试 rename，直接复制合并；源/目标类型不一致（文件 vs 目录）→ `target type mismatch (source and target must both be file or both be dir)`。 | `fsops.go` `moveOne` |
| P2 跨卷移动不保留 mtime | 跨卷回退复用 `copyPath`/`copyFileAtomic`：临时文件上 `Chmod`/`Chtimes` 后 rename，mtime 与源一致；同卷 rename 由文件系统保留。新增 `TestMoveCrossVolumePreservesMtime` 断言。 | `fsops.go` `copyFileAtomic` / `moveByCopy` |
| P2 目录合并非镜像、契约未说明 | 契约明确“目录移动合并不是镜像，目标中源没有的文件保留”，并说明同卷 rename 与跨卷复制的差异。 | `file_move.tool.md` |
| P2 结果无 `bytes`/`files` 计数 | 文件成功返回 `bytes`（同卷 rename = 源 size；跨卷 = 实际复制字节）；目录成功返回 `files`（同卷 rename 在 rename 前 `countFiles`；跨卷 = `copyStats.files`），跨卷/目录合并另有 `bytes`。 | `fsops.go` `moveOne` / `moveByCopy` |

补充（原 §6 未单列）：

- **锁与去重**：`moveOne` 在 `createDirs` 后、目标存在性检查前复用 `lockTarget(to)`（`<to>.chonk.lock`），锁内 `Lstat` 复查目标，`defer release()`；`runMove` 同批重复 `to` → `duplicate target`，重复 `from` → `duplicate source`。锁只覆盖插件内并发，外部进程 best-effort。
- **目标存在性**：`moveOne`/`copyOne`/`copyFileAtomic` 改用 `os.Lstat`，broken symlink 也算已存在，`overwrite=false` 不会把 symlink 当不存在后替换。

测试：新增 `TestMoveRenameNonEXDEVKeepsSource`、`TestMoveCrossVolumeFallback`、`TestMoveSourceRemoveFailurePartial`、`TestMoveSamePathSkipped`、`TestMoveDirectoryMergeNotMirror`、`TestMoveDuplicateTarget`、`TestMoveDuplicateSource`、`TestMoveCrossVolumePreservesMtime`、`TestCopyRejectsSymlinkedTargetSubdir`（`filetool_test.go`；Windows 无 symlink 权限时 skip）。

## 11. 去 lockfile / 目标独占（2026-11-09）

本次只改 `file_move` 与 copy/move 共用实现、平台辅助、契约、测试与审查文档；`file_write/replace/patch/read/delete/diff/find/iconv/detect` 业务行为未变。

| 变更 | 说明 | 证据 |
|---|---|---|
| 删除 `.chonk.lock` | `moveOne` 不再调用 `lockTarget(to)`；`edit.go` 的 `lockTarget` / `acquireTargetLock` 在 copy/move 迁走后已无调用方，一并删除。移动成功后目标旁不再出现 `<to>.chonk.lock`。 | `fsops.go` `moveOne`；`edit.go` |
| 同卷 rename 无锁 | 同卷文件/目录继续使用 `os.Rename`（OS 原子完成），不持有目标句柄；`overwrite=false` 的 `Lstat` 存在性检查与 `rename` 之间是 **best-effort**（无锁文件，外部进程抢先创建目标可能被覆盖），契约已注明。 | `fsops.go` `moveOne` |
| 跨卷/目录合并复制 | `EXDEV` 或目录目标已存在（`overwrite=true`）时走 `copyPath`：Windows 文件目标独占打开后就地写（不再临时文件 + rename），非 Windows 保留原子写；目录逐文件独占/原子写，无全局目标目录锁。 | `fsops.go` `moveByCopy`；`copy_in_place_*.go` |
| partial 语义 | 复制失败且目标原本不存在 → 回滚新建目标树（Windows 新建文件也会删除）；目标原本存在（目录合并，或 Windows 就地覆盖文件）且复制已开始后失败 → `partial:true`（目录另给 `copiedFiles`）。目标被占用时 `code:"FILE_IN_USE"`，且因未写入目标而不标 partial。 | `fsops.go` `copyFailurePartial` / `copyFailureMessage` |
| 非 Windows 原子写 | 跨卷复制仍为同目录临时文件 + `rename`，保留 mode/mtime，失败不改动目标。 | `copy_in_place_other.go` |

测试：新增 `TestMoveNoLockFileAfterSuccess`、`TestCopyOverwriteInPlacePartial`（与 copy 共用）；`TestCopyTargetInUse` 为 Windows-only；原有 `TestMoveRenameNonEXDEVKeepsSource`、`TestMoveCrossVolumeFallback`、`TestMoveSourceRemoveFailurePartial`、`TestMoveSamePathSkipped`、`TestMoveDirectoryMergeNotMirror`、`TestMoveDuplicateTarget`、`TestMoveDuplicateSource`、`TestMoveCrossVolumePreservesMtime`、`TestCopyAndMove` 继续通过。
