# file 工具审查索引

审查批次：本文件合并两批结果。上表为 file-A（4 个工具）；file-B 的 7 个工具（diff/find/copy/move/delete/iconv/detect）见文末「file-B 批次」章节。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| file_read | 可用；显式编码绕过二进制拒绝、配置上限静默截断 | 0 | 2 | 5 | [file_read.md](file_read.md) |
| file_write | 有风险；契约的跨进程锁不存在，auto 编码可能改源编码 | 0 | 3 | 5 | [file_write.md](file_write.md) |
| file_replace | 有风险；expectMd5 无锁（TOCTOU）、diff 无上限 | 0 | 2 | 6 | [file_replace.md](file_replace.md) |
| file_patch | 不建议当前形态；hunk 上下文未全量校验，可静默写错/丢行 | 1 | 3 | 5 | [file_patch.md](file_patch.md) |
| **小计（file-A）** | | **1** | **10** | **21** | |

> **2026-11-08 修复**：`file_replace` / `file_patch` 已处理锁内 expectMd5、二进制/严格编码、重复路径、skipped 字段；`file_replace` 成功项不再返回 diff（只回 status+replacements），`file_patch` 只回 status+hunks。原 `file_replace` 的“完整 diff”条复盘后按 P2 条件性风险处理。明细见各自文档 §10；上表保留审查时原始计数。


> **2026-11-08 修复**：`file_write` 的 3 个 P1 / 5 个 P2 已处理（跨进程锁、Stat 覆盖判定、auto 低置信度报错、bom 仅 UTF-8 且默认无 BOM、重复路径文案；maxTargets/async 由前批解决），明细见 `file_write.md` §10；上表保留审查时原始计数。


> **2026-11-08 修复**：`file_read` 的 2 个 P1 / 5 个 P2 已全部处理（二进制探测、targets 单一来源+超限报错、infoOnly 预算、START_BEYOND_EOF 语义、错误码文档、顶层 `ok`、`_meta` 断言），修复明细见 `file_read.md` §10；上表保留审查时原始计数。


> **2026-11-09 独占打开改造**：`file_write` / `file_replace` / `file_patch` 的写入路径不再使用“临时文件 + rename”作为 Windows 原子方案，改为独占打开（`dwShareMode=0`）+ 就地读写；共享冲突按 50/100/200ms 重试 3 次（总尝试 = 初次 + 3 次），仍失败不写。就地写放弃 rename 的崩溃原子性：replace/patch 写失败用旧内容回滚，file_write 写失败可能返回 `partial:true`。非 Windows 保留原子写路径。明细见各自文档 §11；上表保留审查时原始计数。


> **2026-11-09 批处理去重/合并与锁调度**：`file_write` 同路径改为 **last-wins**（只执行最后一个 target，前面项 `skipped` + `superseded_by_later_target`）；`file_replace` / `file_patch` 同路径**合并**为一次独占打开/读/写，组内按入参顺序应用、文件级全有全无（第 k 项失败 → 该项 failed、同组其他项 `skipped` + `not_applied`；多个不同非空 `expectMd5` → 全组 failed + `conflicting_expectMd5`）。三个工具共用 `src/lib/filetool/batchlock.go`：第一遍每个文件只尝试一次，`errFileInUse` 记 pending、其他文件先处理；之后对 pending 做 3 轮 50/100/200ms 重试，仍失败 code=`FILE_IN_USE`。`results` 按入参顺序预分配、按索引填充。明细见各自文档 §12；上表保留审查时原始计数。


> **2026-11-09 修复（file_iconv / file_detect）**：`file_iconv` 改为**独占打开 + 就地写 + 批调度**（复用 `batchlock.go`；重复 `path` → 后项 `duplicate target`，`FILE_IN_USE` 3 轮 50/100/200ms 重试），`expectMd5` 移入独占句柄内（消除 TOCTOU），显式 `from` 增加 actual detection + round-trip（`from_mismatch` 时绝不写，修复显式 from 重试破坏幂等），`to` 空/auto 与不支持编码分开报错（`to is required` vs `unsupported_encoding`），BOM legacy 限制/`bom_dropped` 入契约，单次 `io.ReadAll` 替代 `os.ReadFile`+`md5File` 双读；`file_detect` 增加 4MiB `sampleBytes` 上限（超限 `SAMPLE_TOO_LARGE`，不读不分配）、空文件显式 `unknown/low`、二进制字段与 `candidates` 结构契约化、`readHead` 改 `io.ReadFull`（避免短读）。明细见各自文档 §10；上表保留审查时原始计数。

> **2026-11-09 流式改造（file_iconv）**：`file_iconv` 的整文件 `io.ReadAll` 路径改为**流式转码**（`streamcodec.go` + `streamcommit_*.go`）：目标同目录 backup / round-trip / out 临时文件，源经 `io.SectionReader` → `transform.Reader` 链 → 64 KiB `io.CopyBuffer`；显式 `from` 的 round-trip 改为分段（64 KiB）比较；`expectMd5`、already_target、`from_mismatch`、BOM、批调度与返回字段全部保持。**O(1) 内存，不新增 `maxFileBytes`**；Windows 就地提交 + backup 回滚（回滚失败 `partial:true`），非 Windows 临时输出 `rename` 原子提交并保留目标 mode。临时文件成功后删除。明细见 `file_iconv.md` §11；上表保留审查时原始计数。

> **2026-11-11 编码改造（chardet / 默认字符集）**：`detectEncoding` 改用 `joshtechnologygroup/chardet`（BOM → NUL 二进制 → 严格 UTF-8 → chardet → 内置候选回落），`file_read/file_detect` 共用；`file_read` 显式 `encoding` 转换失败时标记 `encoding_requested`/`encoding_fallback` 并自动识别二次读取；`file_write` 新文件用配置页默认字符集、旧文件保持原编码（ANSI/GBK 可转默认）、显式 `encoding` 覆盖；`file_patch` 自动识别且 ANSI 源可转默认字符集，成功项返回实际 `encoding`；`file_replace` 仅新增返回 `encoding`、不改编码。插件配置 `plugins.file.default-encoding` 由“逐 target 注入 encoding”改为顶层 `defaultEncoding`（只被 write/patch 消费，read 保持自动识别），配置页新增可选默认字符集（空=自动识别）。明细见各文档 §11/§13；上表保留审查时原始计数。


## 跨工具发现（同一证据影响多个工具）

1. **`maxTargets` 契约 2000 vs 插件配置默认 200（2026-11-08 已修）**：原 `applyDefaults` 对 `targets` 直接切片截断；现默认值/配置上限/契约 `targets.maxItems`/硬上限统一为 `filetool.TargetsHardLimit=2000`，配置调低后超限改为参数错误。`TestTargetsLimitSingleSource` 守卫四处一致。
2. **契约 `async`/`timeout` 未被插件消费**：框架 `contract.Parse` 只读 name/title/hot，schema 原样注册（`chonkpilot-framework/plugin/src/contract/contract.go:63-92`，尤其 `:72-80`）；插件 `contract.Register` 只写 `_meta.hot`（同文件 `:101-105`）。因此 `.tool.md` 的 `async: never` / `timeout: 120` 不进入 tools/list，gateway 缺省 `async=auto`、server 级超时取 manifest `mcp.timeout_sec=300`（`E:/BizWorks/chonkpilot/src/initdata/capability/plugins/file/manifest.json`）。→ 基线 §6 在 file 插件不成立（[待核实] gateway 对 builtin 插件路由的最终覆盖）。
3. **写工具集合核实（不照搬说法）**：7 个写工具 = `file_write/file_replace/file_patch/file_copy/file_move/file_delete/file_iconv`（`src/sandbox.go:22-34`）；`file_read/file_find/file_diff/file_detect` 为只读、不受可写白名单限制。与实现副作用一致，无范围差异。
4. **跨进程锁只有定义没有调用**：`fileops.acquireLock` 仅定义（`src/lib/fileops/lock.go:36`），全仓无调用点；契约在 file_write/replace/patch 均声称加锁。影响本批 3 个写工具。
5. **返回外壳**：本插件的 4 个工具统一 `{results[], summary{total,ok,failed,skipped}}`（`src/lib/filetool/filetool.go:101-127`）；参数级错误与沙箱拒绝是纯文本 `isError=true`，不带 JSON 外壳。与共通基线的 file 域约定一致。

## 契约/源码核对结果

- 4 个工具的 `.tool.md` 均存在且与 `manifest.json` 的 `hot_tools`（11 个）一致：`file_read/file_write/file_replace/file_patch` 都在 hot 列表。
- `handler` 映射无误（`src/lib/filetool/filetool.go:37-49` → `readwrite.go` / `edit.go`）。
- 未发现契约缺失的工具名或程序化注册工具（本批全部为 `.tool.md` + `contract.Register`）。
- 本批全部结论基于源码最终状态；未执行 build/test/插件进程。

## 范围差异

- 分配清单要求的写工具集合已自行核实为 7 个，结论见上。
- `file_read` 按“复核已知待定项”处理，未重复导出共通基线；四项待定项全部仍成立（见 `file_read.md` §4）。
- 未覆盖 file_copy/file_move/file_delete/file_diff/file_find/file_iconv/file_detect。

---

# file-B 批次：file_diff / file_find / file_copy / file_move / file_delete / file_iconv / file_detect

审查批次：插件工具全量审查（file-B）。覆盖 7 个工具，均出现在 `file/src/tools/file/*.tool.md` 与 `chonkpilot/src/initdata/capability/plugins/file/manifest.json:21` 的 hot_tools 中。每个工具的 md 见下表链接；§6 只列该工具**专有**问题，跨工具共通问题单列于 C1–C5。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| file_diff | 不建议当前形态（输出不是可用 unified diff） | 2 | 2 | 2 | [file_diff.md](file_diff.md) |
| file_find | 有风险（dir/tree 模式目录缺失） | 1 | 5 | 8 | [file_find.md](file_find.md) |
| file_copy | 有风险 | 0 | 4 | 4 | [file_copy.md](file_copy.md) |
| file_move | 有风险 | 0 | 4 | 5 | [file_move.md](file_move.md) |
| file_delete | 可用但有风险 | 0 | 2 | 6 | [file_delete.md](file_delete.md) |
| file_iconv | 有风险（无锁 + 显式 from 破坏幂等） | 0 | 5 | 4 | [file_iconv.md](file_iconv.md) |
| file_detect | 基本可用 | 0 | 1 | 5 | [file_detect.md](file_detect.md) |
| **小计（工具专有）** |  | **3** | **23** | **34** |  |

> 计数口径：上表只计各工具 md §6 中专有问题行；C1–C5 另计 **P1 4 / P2 1**。含共通问题的 file-B 全局计数为 **P0 3 / P1 27 / P2 35**。与 file-A 的定级差异：`maxTargets` 静默截断被 file-A 记为 P2；本批对 copy/move/delete 记为 P1（批量写/删项被静默丢弃，数据面影响），read 类仅 token 影响时 P2 合理。

> **2026-11-09 修复**：`file_find` 的 P0/P1/P2 已处理（目录项返回与 dir/tree 语义、grep 流式、字典序截断、depth 方案 B、`maxTotalHits`/`hits_truncated`、root 校验、日期本地时区、契约补齐），明细见 `file_find.md` §10；上表保留审查时原始计数。`file_diff` 也已处理（见 `file_diff.md` §10）。

> **2026-11-09 修复（file_copy）**：流式复制（临时文件 + `io.Copy` + rename）、单文件原子且复制路径不逐文件 fsync、目录失败回滚/合并 `partial`+`copiedFiles`、保留文件 mode/mtime、锁内复查 `overwrite=false`、同批重复 `to` 报 `duplicate target`、契约/返回字段补齐；明细见 `file_copy.md` §10。上表保留审查时原始计数。

> **2026-11-09 修复（file_move）**：`file_move` 已处理仅 `EXDEV` 回退（其他 rename 错误返回 `rename: ...`，不复制不删源）、目录合并（非镜像）、`partial`/`source_removed`/`copiedFiles` 中间态、`to==from` → skipped `same_path`、重复 source/target、锁内 `Lstat` 复查、`bytes`/`files` 计数、目标树目录 symlink 拒绝（copy/move 共用）；明细见 `file_move.md` §10。原列 4 个 P1 / 5 个 P2 中的**沙箱 P1 经复核为误报**：`sandbox.Policy.AllowsPath` 已使用 `RealPath`/`EvalSymlinks`，字符串前缀绕过不成立。上表保留审查时原始计数。

> **2026-11-09 修复（file_delete）**：任何目录（含空目录）必须显式 `recursive=true`；symlink 用 `Lstat` 判定且只删链接本身（`type:"symlink"` + `link_target`，断链可删）；目录一次 WalkDir 统计 `removedFiles`/`removedBytes`、单 target 上限 1000（超限不持有不删）；Windows 对每个普通文件持有 `DELETE`+`share=0` 独占句柄直至 delete-on-close（第一遍冲突跳过 + 3 轮 50/100/200ms 重试，全部拿到才标记删除，任一失败一个不删；`FILE_IN_USE`/`ACCESS_DENIED`/`HOLD_FAILED` + `locked_path`/`locked_files`）；文件全有全无，目录在文件删除后以 `os.Remove` 清理、残留 → `partial:true`；`md5_skipped:"unreadable"`；重复 `path` → `duplicate target`；非 Windows 保留 `RemoveAll` 路径且不承诺独占。原 P1 沙箱前缀问题经复核为误报（`AllowsPath` 已 `RealPath`/`EvalSymlinks`）；超时/取消中断仍属 toolset 跨工具问题，契约已注明不承诺。明细见 `file_delete.md` §10。上表保留审查时原始计数。

> **2026-11-09 去 lockfile / 目标独占（file_copy / file_move）**：`copyOne` / `moveOne` 不再获取 `<to>.chonk.lock`；`edit.go` 的 `lockTarget` / `acquireTargetLock` 与 `filetool.go` 的 `writeAtomic`（已无调用方）一并删除。`file_copy` 的 Windows 文件目标改为**独占打开（`dwShareMode=0`）+ 就地复制**（`io.Copy` → `Truncate` → `Sync`，句柄 `SetFileTime` 设 mtime），新建目标失败删除、已存在目标失败 `partial:true`、目标被占用 `code:"FILE_IN_USE"`；非 Windows 保留临时文件 + `rename` 原子写。`file_move` 同卷 `rename` 保持原子且无锁文件，`overwrite=false` 存在性检查为 best-effort；跨卷/目录合并复制复用目标文件独占与相同 partial 语义。明细见 `file_copy.md` / `file_move.md` §11；上表保留审查时原始计数。

## 跨工具共通问题（file-B，C1–C5；各工具 md 中引用）

| ID | 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|---|
| C1 | P1 | 契约 YAML 的 `async/timeout/idempotent/category` **不进入插件 MCP 工具面**：`contract.Parse` 只提取 `name/title/hot`，`Register` 只写 `_meta.hot`。llm-server 对无 `_meta.timeout` 的工具统一注入 `async=always`，因此 7 个契约声明的 `async: never` 在插件链路不生效，每次调用先收到“已转后台/运行中”，终态经 mcp-tasks-report | `chonkpilot-framework/plugin/src/contract/contract.go:72-79,101-105`；`chonkpilot/src/lib/llm/server/turn.go:342-357`；`gwclient.go:240-248` | 返回面从“同步结果”变为“后台受理 + 终态回执 + 超时/取消裁决”；契约元数据与真实行为脱节。[待核实] gateway 是否用 manifest `timeout_sec`（`manifest.json:22`）补齐工具 `_meta.timeout` |
| C2 | P1 | 插件默认注入 `MaxTargets`（默认 200）并在 `targets` 超限时**静默截断**，无 skipped 项、无 `dropped` 标记；契约 schema 写 `maxItems: 2000` | `file/src/main.go:145-148`；`file/src/config.go:42`；各契约 `maxItems` | 批量 copy/move/delete/iconv 少做最多 1800 项，模型只能从 `summary.total` 反推；数据面风险（尤其删除/移动） |
| C3 | P1 | `finish()` 恒返回 `cli.Ok`，逐项 failed 只体现在 `summary.failed`；**全部失败时 isError=false** | `file/src/lib/filetool/filetool.go:98-127` | 单目标写/删调用失败时模型易当成功；恢复路径依赖模型主动解析 JSON |
| C4 | P1 | LLM 可见工具描述只取 `# 摘要` **首段**，后续规则段落（沙箱、硬上限、低置信度拒绝、仲裁建议）不进入 description | `chonkpilot-framework/plugin/src/contract/contract.go:174-193` | 模型只能从 schema 字段 description 获得部分约束；find 的 skipDirs/模式表、copy/move 的沙箱规则、delete 的根目录拒绝均不可见 |
| C5 | P2 | schema 仅作为 tools/list 的 InputSchema 透出，handler **不做运行时校验**；`asInt/asBool` 对错误类型静默回退默认值 | `contract.go:106-115`；`filetool.go:229-247` | 类型错误（如 `"limit":"5"`）不报错而是落回 2000/默认，模型难以察觉 |

## 范围与差异（file-B）

- 分配清单 7 个工具全部存在、契约与 handler 均核对完毕；无缺失、无额外工具。
- 契约 ↔ 实现的主要差异：
  - `file_diff`：`context` 参数无实现；16MB 阈值实际 8MiB；输出无 `@@` hunk 且头部内嵌全文（无法喂给 `file_patch`）。
  - `file_find`：`dir`/`tree` 不返回目录；截断非字典序；grep 模式多返回 `matched_total`；`meta=none` 仍返回 `size`。
  - `file_delete`：空目录无需 `recursive`；`removedFiles` 从不返回；`idempotent:true` 与默认 `missingOk=false` 矛盾。
  - `file_iconv`：“锁内校验”无锁；`bom=keep` 对 legacy 目标丢 BOM；`bom` 字段取值域未文档化。
  - `file_detect`：`sampleBytes` 无上限；空/未知文件 `confidence` 为空串且缺 `encoding`；摘要 `scores[]` 与示例 `score` 不一致。
  - `file_copy/file_move`：契约未说明目录合并/权限/mtime 语义；move 对任意 rename 错误都按“跨卷”回退。
- 写工具沙箱（本组 4 个写工具均核实）：`file_copy/file_move/file_delete/file_iconv` 全部在 `writeTools`（`file/src/sandbox.go:23-31`），调用前预检、命中即整次拒绝、未做任何写入；`copy/move` 额外校验 `from`（删除语义）。**原“只做路径字符串前缀判定”已更正**：`sandbox.Policy.AllowsPath` 已对目标与白名单两侧执行 `RealPath`（`filepath.EvalSymlinks`；目标不存在时解析父目录），junction/symlink 前缀绕过不成立。
- 读工具（`file_diff/file_find/file_detect`）：确认不进入沙箱、不做 symlink 解析；`file_find` 的 `root` 为 symlink 时静默返回空结果。
- 未检查/未修改 `dist` 或 `build/plugin-apps`（`file/` 仓内无构建产物可比对）；全部结论基于源码最终状态，未执行 build/test/插件进程。