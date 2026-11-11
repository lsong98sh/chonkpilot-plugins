---
plugin: file
tool: file_copy
contract: file/src/tools/file/file_copy.tool.md
handler: file/src/lib/filetool/fsops.go:139-365
reviewed_at: 2026-11-08
status: verified
---

# file_copy 审查

## 1. 一句话结论
**有风险（P1）**：沙箱白名单预检与逐文件原子写成立，但整文件 `io.ReadAll` 无上限、目录复制会留下半棵树、逐文件 `fsync` 在 120s 超时后仍继续写，且 `targets` 会被静默截到 200。

## 2. 调用链
`filePlugin.call` → `applyDefaults`（`main.go:126-150`）→ `dispatch` → `checkSandboxWritable`（`sandbox.go:37-58`，`file_copy` 在 `writeTools` 与 `fromToTools` 中，逐项校验 `path/from/to`）→ `toolset.Dispatch` → `runCopy`（`fsops.go:125`）→ 逐项 `resolvePath(from/to)` → `copyOne`（`fsops.go:146`）→ `copyPath`/`copyFile`（`fsops.go:177-215`）→ `writeAtomic`（`filetool.go:162-191`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 120`、`idempotent: false`、`hot: true`（契约 3-12 行）。async/timeout 在插件面不透出（C1）。
- 沙箱规则（契约 18 行）与实现一致：`from`/`to` 均校验，越界**整次调用**拒绝、预检在分派前（`sandbox.go:8-9,45-57`）。
- `overwrite` 缺省 false、`recursive` 缺省 true、`createDirs` 缺省 true（契约 21-22、45-56 行）与 `fsops.go:156-166` 一致。
- “复制失败不留半截目标文件（先写临时文件再 rename）”（契约 22 行）：仅对**单个文件**成立；目录复制中途失败会留下部分目标树（见 P1）。
- 返回示例没有 `type` 字段（契约 68 行），实现总是返回 `type:"file"|"dir"`（`fsops.go:155`）；`bytes` 仅文件有，且等于源文件大小（`fsops.go:170-172`）。
- schema `maxItems: 2000` 与运行时配置默认 200（C2）不一致。
- LLM 可见描述仅首段（C4）：沙箱、覆盖/递归、原子写规则都不在工具描述里，只能从 schema 各字段 description 间接得知。

## 4. 实现面
- 校验：`parseRequest`（`filetool.go:75-96`）→ 每项 `resolvePath`，相对路径 → 该项 `failed`（`fsops.go:132-139`）。
- 自我保护：`isWithin(to, from)` 拒绝目标落在源内部（`fsops.go:148-150`、`fileops`→`safepath.IsWithin`）。
- 覆盖：目标存在且 `overwrite=false` → 该项 failed（`fsops.go:159-161`）；判定与写入之间存在 TOCTOU（P2）。
- 建目录：`createDirs` 缺省 true，`MkdirAll(filepath.Dir(to), 0755)`（`fsops.go:162-166`）。
- 文件复制：`os.Open` → **`io.ReadAll` 全量入内存** → `writeAtomic`（`fsops.go:202-215`）。`writeAtomic` = 同目录 `.ck-tmp-*` + `Write` + `Sync` + `Close` + `Rename`（`filetool.go:162-191`）。
- 目录复制：`filepath.WalkDir` 递归，目录 `MkdirAll(0755)`，文件逐个 `copyFile`（`fsops.go:177-199`）；WalkDir **不跟随符号链接目录**，但符号链接文件会被 `os.Open` 跟随并复制目标内容。
- 硬限制：无单文件/总字节上限，无文件数上限，无并发控制。
- 副作用：写目标路径与临时文件；不改源。
- 原子性：单文件 atomic rename；目录整体非原子。
- 幂等：`idempotent:false` 合理（overwrite=false 时重复调用会 failed）。
- 清理：`defer in.Close()`；`writeAtomic` 失败时删临时文件（`filetool.go:169-174`）。

## 5. 返回面：LLM 实际看到的所有形态
1. **全部成功**：`{results:[{from,to,status:"ok",type,bytes?}],summary{total,ok,failed,skipped}}`，isError=false；`skipped` 恒为 0。
2. **部分成功**：失败项 `status:"failed"` + `error`，可能文案：`target is inside source (self-nested copy)`、`target exists (overwrite=false)`、`source is a directory (set recursive=true)`、`create dir: ...`、`is a directory`/OS 错误、`targets[i].from: <路径规则消息>`。
3. **全部失败**：isError=false，仅 `summary.failed=total`（C3）。
4. **跳过**：本工具不产生 `skipped`。
5. **截断**：不截断；但 `targets` 超配置上限（默认 200）时**被静默丢弃**，结果只含前 200 个（C2）——返回值本身无任何“有目标被丢弃”的标记。
6. **参数级失败**：targets 缺失/非数组/空/超 2000 → 纯文本 isError=true。
7. **沙箱拒绝**：纯文本 `sandbox: 目标路径不在可写白名单内：<raw>（本次调用已拒绝、未做任何写入；…）`，isError=true；整次调用未执行。
8. **异步/超时/取消**：C1 适用时先返回后台受理；`ctx` 结束后 `toolset.Dispatch` 返回 `context canceled`/`context deadline exceeded`（isError=true），但复制 goroutine 继续 `io.ReadAll`+`Sync`+`Rename`，LLM 看到的失败与磁盘实际状态可能不一致（`toolset/toolset.go:40-63`）。
9. **内部错误**：`tool file_copy panic: ...`、`marshal result: ...`（isError=true）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 单文件复制 `io.ReadAll` 全量入内存，无大小上限 | `fsops.go:202-215`（210 行） | 复制大文件/镜像 OOM；跨卷移动也复用该路径 |
| P1 | 目录复制非原子：任一点失败即 WalkDir 返回错误，已创建的目录与已复制文件留在目标 | `fsops.go:182-196`；契约 22 行 | 契约承诺“不留半截”对目录不成立；模型重试会遇到“target exists” |
| P1 | 逐文件 `f.Sync()`；大量小文件时 120s 超时风险高，超时后复制仍继续 | `filetool.go:179`；`toolset/toolset.go:58-63` | 返回失败但文件已复制，模型无法判断真实状态 |
| P1 | 沙箱只做路径字符串前缀判定（`ResolvePath`+`AllowsPath`），不解析 symlink/junction | `sandbox.go:49-55` | 白名单内的 junction/symlink 可把写入重定向到白名单外（**安全**，`AllowsPath` 内部实现[待核实]）。**2026-11-09 更正**：`sandbox.Policy.AllowsPath` 已 `RealPath`/`EvalSymlinks`，绕过不成立，见 §10 |
| P2 | 目录 `to` 已存在且 `overwrite=true` 时按 `MkdirAll` 合并写入，源中已删除的文件不会从目标移除（非镜像语义，契约未说明） | `fsops.go:182-196` | 模型以为“复制=镜像”，得到残留文件 |
| P2 | 复制结果的权限来自 `os.CreateTemp`（0600），不保留源文件 mode；也不保留 mtime | `filetool.go:164,186`；`fsops.go:202-215` | Linux 下可执行位/可读性丢失；审计时间信息丢失 |
| P2 | `overwrite=false` 的存在性检查与最终 `Rename` 之间 TOCTOU，并发时可静默覆盖 | `fsops.go:159-161`；`filetool.go:186` | 并发下丢数据（低概率） |
| P2 | `bytes` 是源文件 `st.Size()`，不是实际写入字节；目录结果无 `bytes` | `fsops.go:170-172` | 字段语义易被误读 |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 改为流式复制（`io.Copy` + 临时文件），并加 `maxFileBytes` 上限与超限 failed | 优化 | 防 OOM，大文件可预期失败 | 低（1 文件） | 新增参数/错误码 | 建议现在做 |
| 目录复制失败时回滚已创建的目标树，或返回 `partial:true` + `copiedFiles` | 优化 | 真原子/可判断的状态 | 中（fsops.go） | 新增字段；回滚有误删风险，需谨慎 | 排期做 |
| 批量去重（同一 from/to 重复项）与结果审计字段（totalBytes） | 改善 | 少踩重复项坑 | 低 | 兼容 | 排期做 |
| 沙箱校验前 `filepath.EvalSymlinks`（或 `Lstat` 逐段）后再比前缀 | 优化 | 堵住 junction 绕过 | 中（sandbox.go；需确认 AllowsPath 语义） | 严格化可能拒绝既有合法路径 | 建议现在做（安全） |
| 契约补充目录合并语义、权限/mtime 不保留、`type/bytes` 字段 | 改善 | 预期一致 | 低 | 仅文档 | 建议现在做 |

## 8. 建议的测试/验收（不执行）
- 复制 2GB 文件：断言内存峰值与超限错误（当前预期 OOM）。
- 目录复制中途制造不可读文件：断言目标树残留情况与返回文案。
- 白名单目录内建 junction 指向白名单外，再 `file_copy` 到该 junction：断言是否被拒（安全用例）。
- `overwrite=false` 且目标存在、`to` 位于 `from` 内、`recursive=false` 且源为目录：断言逐项 failed。
- 201/300 targets：断言截断是否可见。
- 全部 target 失败：断言 isError 与 summary。

## 9. 基线修正
无。基线 3“sandbox 只对写工具生效”与基线 4“写文件才做沙箱白名单”在本工具成立，且 `from`（删除/读取语义）也被纳入白名单（`sandbox.go:34,72-79`），比基线描述更严格。

## 10. 修复记录（2026-11-09）

本次只改 `file_copy` 相关实现/契约/测试；`file_write/replace/patch/read/diff/find/iconv/detect` 行为未变。原 P1/P2 逐条对应：

| 原问题 | 处理 | 证据 |
|---|---|---|
| P1 单文件 `io.ReadAll` 全量入内存，无大小上限 | 改为同目录临时文件 + `io.Copy` + rename 的流式复制，返回实际写入字节；未新增任意 `maxFileBytes`。`copyPath` 同时被 move 跨卷回退复用，签名已同步。 | `fsops.go` `copyFileAtomic` / `copyPath` |
| P1 目录复制非原子：失败留下半棵树 | `copyOne` 记录目标是否原本存在：不存在 → 失败后 `os.RemoveAll(to)` 回滚本次新建的目标树（回滚失败在 error 中说明）；原本存在（合并）→ 不回滚，返回 `failed + partial:true + copiedFiles`。契约明确“目录复制是合并，不是镜像”。 | `fsops.go` `copyOne` / `copyDir`；契约 |
| P1 逐文件 `f.Sync()`，超时后复制仍继续 | 复制路径不再逐文件 fsync（rename 仍原子，不会半截）；`writeAtomic`（write/replace/patch）保持不变。**未处理**：超时/取消后已启动的复制 goroutine 仍会跑完，属 C1/toolset 层问题，本工具内未引入 context 中断。 | `fsops.go` `copyFileAtomic` |
| P1 沙箱只做路径字符串前缀判定，symlink/junction 可绕过 | **原结论更正**：`sandbox.Policy.AllowsPath` 已对目标与白名单两侧调用 `RealPath`（`filepath.EvalSymlinks`；目标不存在时解析父目录后拼回文件名），junction/symlink 前缀绕过不成立，本工具无需改动。 | `chonkpilot-util/sandbox/policy.go:20-57` |
| P2 目录已存在且 `overwrite=true` 时按合并写入，非镜像语义未说明 | 契约补充“目录复制是合并，不是镜像，不删除目标中多出的文件”。 | `file_copy.tool.md` |
| P2 复制不保留源 mode/mtime | `copyFileAtomic` 在临时文件上 `Chmod(src.Mode().Perm())`、`Chtimes(src.mtime)` 后 rename；新建目录按源目录 mode 设置（仅非 Windows）。Windows ACL/额外属性不保留，契约已注明。 | `fsops.go` `copyFileAtomic` / `ensureDir` |
| P2 `overwrite=false` 判定与 rename 之间 TOCTOU | `copyOne` 在 `createDirs` 后、存在性判定前获取 `lockTarget(to)`（锁文件 `<to>.chonk.lock`），锁内复查目标存在性；同批重复 `to` 直接 `duplicate target`。锁只覆盖插件内并发，外部进程竞争仍 best-effort，契约已注明。 | `fsops.go` `copyOne` / `runCopy` |
| P2 `bytes` 取源 `st.Size()`，目录无 `bytes` | 文件 `bytes` 改为实际写入字节；目录返回 `files` + `bytes`（总字节）；目录合并中途失败返回 `partial` + `copiedFiles`。契约返回示例同步。 | `fsops.go`；`file_copy.tool.md` |

测试：新增 `TestCopyStreamsLargeFile`、`TestCopyDirectoryRollbackWhenTargetNew`、`TestCopyDirectoryPartialWhenTargetExists`、`TestCopyPreservesFileModeAndMtime`、`TestCopyDuplicateTarget`、`TestCopyDirectoryCounts`、`TestCopyOverwriteFalse`（`filetool_test.go`；目录失败注入使用 `copyFileAtomicFn` 测试钩子）。

**追加（2026-11-09，copy/move 共用防护）**：

| 原问题 | 处理 | 证据 |
|---|---|---|
| 目录合并时目标树内预置的子目录 symlink/junction 可把写入重定向到白名单外（沙箱 `AllowsPath` 已解析白名单侧，但目标树内部子目录未覆盖） | `ensureDir` 改用 `os.Lstat`：目标路径是 symlink → `target path traverses symlink (refused)`，该项 `failed`、外部目录不被写入；`copyFileAtomic` 的 `overwrite=false` 存在性检查与 `copyOne` 的目标存在性判定改用 `os.Lstat`（broken symlink 视为已存在）。源目录 symlink 仍不递归跟随，源文件 symlink 仍按目标内容复制。新增 `TestCopyRejectsSymlinkedTargetSubdir`（Windows 无 symlink 权限时 skip）。 | `fsops.go` `ensureDir` / `copyOne` / `copyFileAtomic`；`filetool_test.go`；`file_copy.tool.md` |

## 11. 去 lockfile / 目标独占（2026-11-09）

本次只改 `file_copy` / `file_move` 共用实现、平台辅助、契约、测试与审查文档；`file_write/replace/patch/read/delete/diff/find/iconv/detect` 业务行为未变。

| 变更 | 说明 | 证据 |
|---|---|---|
| 删除 `.chonk.lock` | `copyOne` 不再调用 `lockTarget(to)`；`edit.go` 的 `lockTarget` 与 `acquireTargetLock` 在 copy/move 迁走后已无调用方，一并删除。目标旁不再产生 `<to>.chonk.lock`。 | `fsops.go` `copyOne`；`edit.go` |
| Windows 文件目标独占 + 就地复制 | 先 `os.Open(src)`（失败则不创建目标），再 `openExclusiveWriteOnce(dst, overwrite)` 以 `GENERIC_READ\|GENERIC_WRITE` + `dwShareMode=0` 打开目标；`io.Copy` → `Truncate(n)` → `Sync` → `Chmod` → `SetFileTime`（`NsecToFiletime`）→ `Close`。**不 rename**，放弃崩溃原子性，换取复制期间对其他应用的独占。 | `copy_in_place_windows.go` |
| 新建目标失败回滚 | 目标原本不存在且复制失败 → 关闭句柄后 `os.Remove(dst)`，不留空/半截新目标。 | `copy_in_place_windows.go` |
| 已存在目标失败 partial | 目标原本存在且复制已开始后失败 → 返回 `partialCopyError`，`copyOne`/`moveByCopy` 标记 `partial:true`；不删除用户已有数据。 | `copy_in_place_windows.go`；`fsops.go` `copyFailurePartial` |
| `FILE_IN_USE` | `openExclusiveWriteOnce` 返回 `errFileInUse`（共享/锁冲突）→ 该项 `failed` + `code:"FILE_IN_USE"`；copy 不在 write/replace/patch 的批调度重试范围内，直接失败。 | `fsops.go` `copyFailureMessage` |
| 非 Windows 保持原子写 | 保留同目录临时文件 + `io.Copy` + `rename`，保留 mode/mtime；失败不留半截目标（不产生 partial）。 | `copy_in_place_other.go` |
| 目录复制 | `copyDir` 逐文件调用平台 `copyFileAtomic`：Windows 每个目标文件独占，非 Windows 原子写；**无全局目标目录锁**；`ensureDir` 的 symlink 拒绝逻辑保留，目录合并语义不变。 | `fsops.go` `copyDir` / `ensureDir` |
| 删除 `writeAtomic` | `src/lib/filetool/filetool.go` 的 `writeAtomic` 已无调用方，连同 `path/filepath` import 一并删除；`md5File` 等辅助保留。 | `filetool.go` |

测试：新增 `TestCopyTargetInUse`（Windows-only：`os.Open` 持有目标 → `FILE_IN_USE`，内容不变，关闭后成功）、`TestCopyNoLockFileAfterSuccess`、`TestMoveNoLockFileAfterSuccess`、`TestCopyOverwriteInPlacePartial`；原有 `TestCopyStreamsLargeFile` / `TestCopyDirectoryRollbackWhenTargetNew` / `TestCopyDirectoryPartialWhenTargetExists` / `TestCopyPreservesFileModeAndMtime` / `TestCopyDirectoryCounts` / `TestCopyOverwriteFalse` / `TestCopyRejectsSymlinkedTargetSubdir` / `TestCopyAndMove` 继续通过。
