---
plugin: file
tool: file_delete
contract: file/src/tools/file/file_delete.tool.md
handler: file/src/lib/filetool/fsops.go:14-69
reviewed_at: 2026-11-08
status: verified
---

# file_delete 审查

## 1. 一句话结论
**可用但有风险（P1）**：沙箱预检、根目录拒绝、非空目录守卫、删除前 md5/size 审计都成立；主要风险是**不可逆删除在超时/取消后仍继续**，以及 `targets` 静默截断会让“整批删除”只删前 200 个。

## 2. 调用链
`filePlugin.call` → `applyDefaults`（`main.go:126-150`）→ `dispatch` → `checkSandboxWritable`（`file_delete` ∈ `writeTools`，按 `targets[].path` 校验，命中即整次拒绝）→ `toolset.Dispatch` → `runDelete`（`fsops.go:15`）→ 逐项 `resolvePath(path)` → `deleteOne`（`fsops.go:33`）→ `os.RemoveAll`（`fsops.go:60`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 120`、**`idempotent: true`**、`hot: true`（契约 3-12 行）。`idempotent` 与本工具默认语义不符（见 P2），且插件面不透出（C1）。
- 沙箱规则（契约 18 行）与实现一致（`sandbox.go:23-31`、`45-57`）。
- “删目录必须显式 `recursive=true`，否则该项 failed（防误删整棵树）”（契约 20 行）：**空目录**不设 `recursive` 也会被删（`fsops.go:45-52` 只在 `len(entries)>0` 时失败）——与“必须”不一致。
- “`missingOk` 缺省 false：不存在 → failed；true → skipped”（契约 21 行）：实现一致，reason=`not found`（`fsops.go:39-44`）。
- “删除前记录 md5（仅文件）与 size”（契约 22 行）：实现一致（`fsops.go:53-59`）。
- “不做回收站：直接删除”（契约 23 行）：实现 `os.RemoveAll`，一致。
- “拒绝删除文件系统根、盘符根、用户主目录根”（契约 26 行）：委托 `safepath.RefuseUnsafeRoot`（`fsops.go:67-69`），具体拒绝集[待核实]。
- 返回示例中目录项有 `removedFiles: 12`（契约 68 行），实现**从不返回**该字段。
- LLM 可见描述仅首段（C4）：不可逆、根目录拒绝、审计字段都不在工具描述里；schema 仅覆盖 `recursive`/`missingOk`。

## 4. 实现面
- 校验：`parseRequest` → 每项 `resolvePath`；相对路径 → 该项 failed（`fsops.go:21-27`）。
- 安全守卫：`unsafeRoot(path)` 先于 `Stat`（`fsops.go:35-37`）。
- 存在性：`os.Stat` 跟随 symlink；`IsNotExist` + `missingOk=true` → skipped `not found`；其他错误 → failed（`fsops.go:38-44`）。
- 目录：`type:"dir"`；`recursive=false` 时 `os.ReadDir` 判空（错误被忽略），非空 → failed `directory is not empty (set recursive=true to delete)`（`fsops.go:45-52`）。
- 文件：`type:"file"`、`size`、尽力计算 `md5`（失败静默省略）（`fsops.go:53-59`）。
- 删除：`os.RemoveAll`（文件/目录统一），错误 → failed（`fsops.go:60-63`）。
- 硬限制：无条目/字节/深度上限；无中断点；无并发控制。
- 副作用：不可逆删除；不改其他文件。
- 幂等：仅在 `missingOk=true` 时幂等；默认 false 时重复删除 → failed。
- 资源：无句柄/临时文件；`md5File` 用 defer Close。
- 超时/取消：删除 goroutine 在 `toolset` ctx 结束后继续执行（`toolset/toolset.go:40-63`）。

## 5. 返回面：LLM 实际看到的所有形态
1. **成功（文件）**：`{results:[{path,status:"ok",type:"file",size,md5?}],summary{...}}`，isError=false。
2. **成功（空目录）**：`{path,status:"ok",type:"dir"}`；无 `removedFiles`。
3. **跳过**：`{path,status:"skipped",reason:"not found"}`（仅 `missingOk=true`）。
4. **部分成功**：越界/根拒绝/非空目录/权限错误 → 对应项 `status:"failed"` + `error`，文案包括 `directory is not empty (set recursive=true to delete)`、`OS 错误`、`targets[i].path: <路径规则消息>`、根拒绝消息（来自 safepath）。
5. **全部失败**：isError=false，仅 `summary.failed=total`（C3）——单文件删除失败时模型必须读 JSON 才能发现。
6. **截断**：不截断；但 targets 超配置上限（默认 200）时静默只处理前 200 个（C2）。对“整批清理”是数据面风险：返回 `summary.total=200` 是唯一线索。
7. **参数级失败**：targets 形状违规 → 纯文本 isError=true。
8. **沙箱拒绝**：整次调用纯文本 isError=true，未删除任何文件。
9. **异步/超时/取消**：C1 适用时先返回后台受理；ctx 结束后返回 `context canceled`/`context deadline exceeded`（isError=true），但 `RemoveAll` 仍在继续——LLM 会以为删除失败，实际可能已完成。
10. **内部错误**：`tool file_delete panic: ...`、`marshal result: ...`（isError=true）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 超时/取消只放弃返回值，`RemoveAll` 继续执行；对不可逆操作尤其危险 | `toolset/toolset.go:58-63`；`fsops.go:60` | 模型看到失败 → 可能重试或改用其他路径，实际文件已删 |
| P1 | 沙箱仅路径字符串前缀校验，不解析 symlink/junction；`recursive=true` 时删除可经 junction 指向白名单外 | `sandbox.go:49-55`；`fsops.go:60` | 权限/沙箱绕过风险（`AllowsPath` 内部[待核实]；Windows junction 的 RemoveAll 行为[待核实]） |
| P2 | 契约“删目录必须 recursive=true”，但空目录不设该参数也会被删 | `fsops.go:45-52`；契约 20 行 | 契约与实现不一致，“必须”并非硬约束 |
| P2 | 契约返回示例的 `removedFiles` 从不产生；目录删除无任何审计计数 | 契约 68 行；`fsops.go:45-63` | 模型无法确认目录内实际删了多少 |
| P2 | `idempotent: true` 与默认 `missingOk=false` 矛盾：重复删除会 failed | 契约 10、21 行；`fsops.go:38-44` | 若将来按 `_meta.idempotent` 自动重试会失败（当前插件面不透出，实际无害） |
| P2 | 非空目录判定忽略 `os.ReadDir` 错误（`entries, _`），不可列目录时可能直接进入 RemoveAll | `fsops.go:47-49` | 守卫在异常场景下失效 |
| P2 | 文件 `md5` 计算失败静默省略（无 `md5_error`） | `fsops.go:56-58` | 审计信息缺失且无提示 |
| P2 | symlink→目录：`Stat` 报 `type:"dir"`，`recursive=true` 时 `RemoveAll` 只删链接不删内容，结果却 ok | `fsops.go:38-60` | 返回类型与真实删除范围不符（模型可能以为目录已清空） |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 返回 `removedFiles`（目录）与 `removedBytes`；`md5` 缺失时给 `md5_skipped` | 改善 | 审计可核对，少一次猜测 | 低（1 文件） | 新增字段 | 建议现在做 |
| 超时/取消时不返回“失败”而返回 `status:"unknown"`/`may_have_completed` 文案 | 改善 | 防止误重试（尤其删除） | 中（跨 toolset 共通） | 返回语义变化 | 建议现在做 |
| `recursive=true` 时增加 `dryRun`/`maxEntries`，先返回将删清单 | 增强 | 不可逆操作可预检 | 中 | 新增参数 | 排期做 |
| 沙箱校验前 EvalSymlinks；删除前用 `Lstat` 判定 symlink 并显式报告 | 优化 | 堵绕过、类型真实 | 中 | 严格化/行为变化 | 建议现在做（安全） |
| `idempotent` 改 false 或把默认 `missingOk` 改 true（二选一） | 改善 | 元数据自洽 | 低 | 契约语义变化 | 排期做 |

## 8. 建议的测试/验收（不执行）
- 空目录不传 `recursive` / 非空目录不传 `recursive`：断言行为与契约一致。
- `missingOk=false/true` 重复删除：断言 failed/skipped 与 `idempotent` 声明。
- 删除 `C:\`、`C:\Users`、用户 home：断言被拒（确认 safepath 拒绝集）。
- symlink→目录、junction：断言 `type`、实际删除范围、沙箱结论。
- `ReadDir` 失败（权限）时的非空目录：断言不会误删。
- 300 targets：断言截断可见性。

## 9. 基线修正
无。基线 3/4 的沙箱模型成立：`file_delete` 属于 `writeTools`，`path` 预检、整次拒绝（`sandbox.go:23-31,37-58`）。

## 10. 修复记录（2026-11-09）

本轮只改 delete 相关实现/平台辅助/契约/测试，未改动 write/replace/patch/copy/move/read/diff/find/iconv/detect 行为。

### 逐条对应原问题

| 原级别 | 原问题 | 处理 | 现状 |
|---|---|---|---|
| P1 | 超时/取消只放弃返回值，`RemoveAll` 继续 | **未改** | 属 toolset 跨工具问题（`toolset/toolset.go`）；契约已注明本工具不承诺可在执行中可靠中断。 |
| P1 | 沙箱仅字符串前缀校验，可经 junction 绕过 | **复核为误报，未改** | `sandbox.Policy.AllowsPath` 已使用 `RealPath`/`EvalSymlinks`（目标不存在时解析父目录）；字符串前缀绕过不成立。 |
| P2 | 空目录不设 `recursive` 也会被删 | 已修 | 任何目录（含空目录）都必须显式 `recursive=true`，否则该项 failed；删除了忽略 `os.ReadDir` 错误的空目录守卫。 |
| P2 | 契约示例的 `removedFiles` 从不返回 | 已修 | 目录成功返回 `removedFiles` + 新增 `removedBytes`（WalkDir 一次统计）。 |
| P2 | `idempotent:true` 与默认 `missingOk=false` 矛盾 | 已修（契约） | 当前契约已是 `idempotent: false`；`missingOk` 语义仅在 `missingOk=true` 时 skipped。 |
| P2 | 非空目录判定忽略 `os.ReadDir` 错误 | 已修 | 旧守卫连同 `ReadDir` 一起删除；改为 `WalkDir`，遍历/读取错误作为该项 failed（`scan: ...`），不会进入删除。 |
| P2 | 文件 `md5` 失败静默省略 | 已修 | 失败返回 `md5_skipped:"unreadable"`。 |
| P2 | symlink→目录被 `Stat` 判为 dir，类型/范围不符 | 已修 | 改用 `Lstat`：symlink 返回 `type:"symlink"` + `link_target`，只删链接本身，不跟随目标、不要求 `recursive`、不算 md5/size；断链 symlink 仍可删。 |

### 本轮新增行为（原审查未覆盖）

- **Windows 持有独占句柄直至 delete-on-close（全有全无）**：目录树收集普通文件后，逐文件 `CreateFileW`（`DELETE` 权限、`dwShareMode=0`、`OPEN_EXISTING`）打开并**保持持有**。第一遍共享/锁冲突记入 pending 并继续处理后续文件；随后按 50/100/200ms 做 3 轮重试。全部拿到后逐个 `SetFileInformationByHandle(FileDispositionInfo, {DeleteFile:1})` 标记 delete-on-close，再关闭句柄完成删除。任一文件最终拿不到 → 关闭全部已持有句柄（不标记 delete），一个都不删；`FILE_IN_USE` + `locked_path`/`locked_files`，`ACCESS_DENIED`/其他 → 对应 code。标记失败 → 对已标记句柄 `DeleteFile:0` 回滚并关闭；清除失败 → `partial:true`（可能已删部分文件）。单文件 md5 从持有的句柄读取（句柄以 `DELETE|GENERIC_READ` 打开）。
- **目录清理在文件删除后**：普通文件经句柄关闭删除；symlink 用 `os.Remove(link)`，空目录自底向上 `os.Remove`（不用 `RemoveAll`），目录非空/被占用 → `partial:true` + 预检计数（文件已删、目录残留）。
- **非 Windows**：保留「预检 no-op + `RemoveAll`」路径，不承诺独占/全有全无。
- **单 target 1000 条目上限**：`maxDeleteFiles = 1000`；超限该项 failed（`too many files (N > 1000); delete subdirectories in batches`），不持有句柄、不删除。
- **重复 target**：同批后项 failed（`duplicate target`）。
- **`missingOk` 与断链 symlink**：`Lstat` 成功即算存在；只有真正不存在才 skipped/failed。

### 测试

- `src/lib/filetool/filetool_test.go`：`TestDeleteEmptyDirRequiresRecursive`、`TestDeleteDirStats`、`TestDeleteDirWithMultipleFiles`、`TestDeleteMaxFiles`、`TestDeleteSymlinkOnly`、`TestDeleteBrokenSymlink`、`TestDeleteDuplicateTarget`。
- `src/lib/filetool/delete_windows_test.go`（windows-only）：`TestDeleteFileInUse`、`TestDeleteFileInUseRetrySucceeds`、`TestDeleteAllOrNone`、`TestHoldExclusive`、`TestHoldExclusiveMarkDelete`。
- 本机（Windows，无 symlink 创建权限）symlink 用例按预期 skip；其余删除用例通过。
