---
plugin: file
tool: file_find
contract: file/src/tools/file/file_find.tool.md
handler: file/src/lib/filetool/query.go:46-336
reviewed_at: 2026-11-08
status: verified
---

# file_find 审查

## 1. 一句话结论
**有风险（P0）**：`dir` / `tree` 两种文档化模式**永远不返回目录**（`is_dir` 恒为 false、`matched_total` 只计文件），“等价 ls / 递归树”的承诺不成立；grep 单文件整读无上限，另有若干静默语义偏差。

## 2. 调用链
`filePlugin.call` → `dispatch`（`main.go:88-108`，只读工具跳过沙箱）→ `toolset.Dispatch`（`toolset/toolset.go:36-63`）→ `runFind`（`query.go:46`）→ 逐 target `resolvePath(root)` → `findOne`（`query.go:64`）→ `filepath.WalkDir`（`query.go:108`）→ 结果 `finish`（`filetool.go:101-127`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 300`、`idempotent: true`、`hot: true`（契约 3-13 行）。async/timeout/idempotent 在插件面不透出（C1）。
- “四种模式”表（契约 18-24 行）：`dir` = 只给 root + depth=1；`tree` = output=tree。实现里 **目录在 walk 中被 `return nil` 直接丢弃**（`query.go:120-128`），故 dir 模式只列“第一层文件”，tree 只渲染文件、缩进靠 rel 的斜杠数（`query.go:315-327`）。
- “`limit` 硬上限 2000，超出按相对路径字典序截断”（契约 27-29 行）：实现是 **walk 顺序先到先得**，达到 limit 后仍继续遍历只计数，最终对已保留的子集排序（`query.go:157-167`）——被保留的集合与字典序无关，且文件系统遍历顺序不同则结果不同。
- “`matched_total` 在 dir/glob/tree 下为精确值”：只统计文件，目录不可见，故对“目录项数”不精确。
- “grep 下省略 `matched_total`，改给 `scanned`”（契约 28 行）：实现两者都返回（`query.go:189-194`）。
- “`meta=none` 只返回路径”（契约 32 行）：`entry.Size` 在遍历时无条件赋值（`query.go:146`），`json:"size,omitempty"` 只对 0 字节文件生效（结构体 `query.go:35-43`），所以非空文件在 meta=none 下仍返回 `size`。
- schema `limit.maximum: 2000`，运行时 `>2000` 静默截到 2000（`query.go:71-73`），与“硬上限不可越过”一致但无错误提示。
- 摘要未披露的内置跳过目录：`.git/.svn/node_modules/.trae/.chonkpilot/__pycache__/.venv/venv/build/dist/.next/.nuxt`（`fileops/fileops.go:31-44`），契约只举例 `.git / node_modules`。
- LLM 可见描述仅首段（C4），四模式表、硬上限说明、哈希规则均不在工具描述里。

## 4. 实现面
- 参数解析：`parseRequest`（`filetool.go:75-96`）；`root` 强校验绝对路径/`~/`/`!/`（`query.go:53`）。`limit` 缺省/≤0 → 2000，>2000 → 2000；`maxHitsPerFile` ≤0 → 100；`maxHashBytes` ≤0 → 512MiB（`query.go:67-89`）。
- 遍历：`WalkDir` 单点错误被吞掉并继续（`query.go:109-111`）；目录名按内置集 + 调用方集过滤（`query.go:120-123`）；`depth` 用 `rel` 的 `/` 数 +1 判层（`query.go:124-131`）。
- grep：`os.ReadFile` 整文件读入（`query.go:215`）→ 二进制拒绝（`query.go:219-221`）→ `DecodeText` → 按行匹配、每文件最多 `maxHitsPerFile` 条、每行 `truncRunes(...,500)`（`query.go:222-239`）。
- 哈希：仅对已入 `entries` 的项计算；累计 `used+size > maxHashBytes` → `hash_skipped="exceeded_limit"`，读失败 → `"unreadable"`（`query.go:169-186`）。
- 输出形态：`output=json`（默认）→ `items`；`list` → `lines`；`tree` → `text`（`query.go:198-209`）。
- 时间过滤：`YYYY-MM-DD` 用 `time.Parse`（UTC 零点），`to` 加 24h-1ns（`query.go:284-301`），与本地 `ModTime` 比较。
- 硬限制：无单文件读取上限、无总输出字节上限、无文件数上限（limit 只管返回条目，grep 命中总量可达 2000×100×500 rune）。
- 副作用：无（只读）。资源：`os.ReadFile` 自管句柄；无 goroutine/临时文件。
- 幂等：是（但遍历结果受文件系统顺序影响）。

## 5. 返回面：LLM 实际看到的所有形态
1. **成功（json）**：`{results:[{root,status:"ok",truncated,matched_total,hash_truncated,limit,items:[{rel,is_dir:false,size,mtime?,md5?,hash_skipped?,hits?}]}],summary{total,ok,failed,skipped}}`，isError=false。
2. **成功（list）**：同壳，`items` 换为 `lines:[rel...]`；**tree**：换为 `text`（文件行 + 2 空格缩进，目录节点缺失）。
3. **grep 命中**：额外 `scanned`（已尝试读的文件数）；`items[].hits=[{line,text}]`；无命中的文件不出现。
4. **截断**：`truncated:true` + `matched_total`（仍为全量计数，除目录缺失外）；无 `next` 游标，只能自行收窄 glob/depth 重调。
5. **哈希截断**：`hash_truncated:true`，部分项 `hash_skipped:"exceeded_limit"|"unreadable"`。
6. **部分成功**：某 target 路径非法/日期格式错/regex 编译失败 → 该项 `status:"failed"` + `error`（`bad regex: ...`、`bad from (want YYYY-MM-DD)` 等），isError=false。
7. **全部失败**：isError=false，仅 `summary.failed=total`（C3）。
8. **空结果（陷阱）**：`root` 指向普通文件或 symlink → WalkDir 只回调 root 且被跳过（`query.go:112-114`），返回 `status:"ok"`、`items:[]`、`matched_total:0`，不报错。
9. **skipped**：本工具从不产生。
10. **参数级整体失败**：targets 形状违规 → 纯文本 isError=true。
11. **异步/取消/内部错误**：同 C1；`context canceled`/`context deadline exceeded`；panic → `tool file_find panic: ...`（均 isError=true）。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | 目录永不出现在结果中：walk 对目录只做过滤后 `return nil`；`is_dir` 恒 false、tree 渲染的目录分支是死代码 | `query.go:120-128,146-161,315-327`；契约 20-24 行 | `dir`/“ls”与 `tree` 模式承诺不成立；模型看不到子目录，无法据此继续探索 |
| P1 | grep 用 `os.ReadFile` 整读每个候选文件，无大小上限 | `query.go:215` | 大文件/大日志 OOM；`maxHitsPerFile` 只减少输出不减少读取 |
| P1 | 截断集合 = walk 顺序前 N 个（之后排序），与契约“相对路径字典序截断”不符；且到限后仍遍历全树 | `query.go:157-167`；契约 27 行 | 同一查询跨机器/跨运行返回不同子集，模型无法可靠分页 |
| P1 | 省略 `depth` 时 `depth=0` 表示不限层级；契约把 “只给 root + depth=1” 描述为 dir 模式，模型省略 depth 会拿到整棵树（最多 2000 项） | `query.go:81,124-131`；契约 21 行 | 误用即 token 爆量；无“默认只看一层”的安全默认 |
| P1 | grep 输出无总量预算（2000 项 × 100 命中 × 500 rune ≈ 100MB 上限） | `query.go:74-77,235-238` | 可爆上下文且无截断提示（每文件命中数不触发全局 `truncated`） |
| P1 | root 为普通文件或 symlink 时静默返回 ok + 0 项 | `query.go:108-114` | 模型会把“路径给错/是文件”误判为“无匹配” |
| P2 | `size` 在 `meta=none` 也返回（契约说只返回路径） | `query.go:35-43,146` | 契约与实现不一致，轻度 token 浪费 |
| P2 | grep 模式同时返回 `scanned` 与 `matched_total`（契约说省略后者） | `query.go:189-194` | 字段语义未文档化 |
| P2 | 内置跳过目录未列入契约（build/dist/.next/.trae/.chonkpilot 等） | `fileops/fileops.go:31-44`；契约 89-92 行 | 文件“凭空消失”，模型无法解释 |
| P2 | 日期过滤用 UTC 零点 vs 本地 mtime，跨时区偏移一天 | `query.go:287-298` | 边界日期漏结果 |
| P2 | `hash_skipped="unreadable"` 未在契约说明；`meta=hash` 同时附带 mtime | `query.go:148,182-184`；契约 31-34 行 | 返回字段超出文档 |
| P2 | 命中行被截到 500 rune（未文档化） | `query.go:235` | grep 文本可能被静默截断 |
| P2 | `list`/`tree` 的字段名 `lines`/`text` 与 `limit` 未在契约返回示例中出现 | `query.go:196-209`；契约 108-126 行 | 模型难以稳定解析非 json 形态 |
| P2 | `depth<0` 静默当作不限层级 | `query.go:81,124` | 非法值不报错 |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 目录也作为 item 返回（`is_dir:true`，dir 模式只列直接子项；tree 用真实目录节点） | 优化 | 恢复 ls/tree 语义，可逐层探索 | 中（query.go 遍历与渲染） | 返回项增多，`limit` 语义需重定义；契约/测试同步 | 建议现在做 |
| 截断改为确定性的“字典序前 N”或显式 `next_after` 游标 | 优化 | 可分页、可复现 | 中 | 新增字段兼容，行为变化 | 排期做 |
| `depth` 缺省改为 1（并保留 `depth=0` 显式表示不限） | 改善 | 防误用整树遍历 | 低 | 默认值变化，需契约同步（可能 breaking） | 排期做 |
| grep 增加 `maxFileBytes`（默认如 8MB）并对超限文件标 `skipped:"too_large"` | 优化 | 防 OOM，且结果可解释 | 低 | 新增字段 | 建议现在做 |
| 全局输出预算（总命中条数/字节）+ `hits_truncated` | 改善 | 防爆上下文 | 中 | 新增字段 | 建议现在做 |
| `root` 非目录/不可达直接 failed，并提示改用 `file_read`/`file_find` 父目录 | 改善 | 明确失败，可恢复 | 低 | 行为收紧 | 建议现在做 |
| 契约补齐内置 skipDirs、`lines`/`text` 字段、UTC 语义 | 改善 | 模型可预期 | 低 | 仅文档 | 建议现在做 |

## 8. 建议的测试/验收（不执行）
- 目录树（含子目录、空目录）下 `depth=1` 与 `output=tree`：断言目录项存在且 `is_dir=true`。
- 同一树重复调用 5 次、limit 小于总数：断言返回子集稳定（当前预期不稳定）。
- 只传 `root` 不传 `depth`：断言是否递归整树（记录现状风险）。
- 2GB 稀疏文件 + grep：断言内存受控与 `too_large` 语义。
- `root` 指向文件、指向 symlink：断言 failed 而非 ok+空。
- `from/to` 在 UTC+8 本地日期边界：断言包含当天文件。
- 200/201 targets：断言 C2 截断可见性。

## 9. 基线修正
基线 4“读文件不做 symlink 解析”在 file_find 成立，但需补充：`WalkDir` 不跟随 symlink，`root` 本身为 symlink 时 `d.IsDir()==false`，直接跳过 → 静默空结果（`query.go:112-114`）。这是基线未覆盖的行为，不是沙箱绕过。
## 10. 修复记录（2026-11-09）

| 原问题 | 修复 | 验证 |
|---|---|---|
| P0 目录永不出现在结果中（`is_dir` 恒 false、tree 无目录节点） | 遍历时目录也作为 `findEntry{IsDir:true}` 返回；`dir` 模式缺省 `depth=1` 实现 ls 语义，`tree` 使用真实目录节点；grep 模式仍只返回命中文件 | `TestFindDirModeDefaultDepth`、`TestFindTreeHasDirectoryNode` |
| P1 grep `os.ReadFile` 整文件入内存 | 读 64KiB 头部判二进制/编码后流式逐行扫描；UTF-8 不逐行解码、首行剥 BOM；legacy 逐行 `encodingx.Decode`；UTF-16 因按字节切行不安全回退整文件解码 | `TestFindGrepStreamsLargeFile`、`TestFindGrepUTF8HeadBoundary`、`TestFindGrepUTF8BOM` |
| P1 截断为 walk 顺序前 N | 最大堆只保留字典序最小的 `limit` 项，结束排序输出；`matched` 仍全量统计 | `TestFindDeterministicTruncation` |
| P1 省略 depth 默认不限层级 | `output=dir` 省略时默认 1；其他模式默认 0；显式 depth 优先；`depth<0` → failed | `TestFindDirModeDefaultDepth`、`TestFindDepthNegativeFails` |
| P1 grep 无全局输出预算 | 新增 `maxTotalHits`（默认 2000 / 硬上限 20000）；达到预算停止并标 `hits_truncated=true`、`truncated=true` | `TestFindMaxTotalHits` |
| P1 root 为普通文件/symlink 静默空结果 | `Lstat` 校验 root 必须是真实目录，否则该项 failed | 代码路径 |
| P2 `meta=none` 仍返回 size | 仅 `meta!=none` 才填 size/mtime | 代码路径 |
| P2 `scanned`/`matched_total` 未文档化 | 契约补齐：两者同时返回，预算触发时为截点值 | 契约 |
| P2 内置 skipDirs 未列入契约 | 契约补齐完整列表 | 契约 |
| P2 日期 UTC 零点 vs 本地 mtime | `ParseInLocation(..., time.Local)`，与本地日期对齐 | `TestFindDateBoundaryLocalTimezone` |
| P2 `hash_skipped="unreadable"` / 500 rune 截断 / `lines`/`text` 未文档化 | 契约补齐字段语义与示例 | 契约 |
| P2 `depth<0` 静默 | 显式 failed（`depth must be >= 0`） | `TestFindDepthNegativeFails` |

> 说明：UTF-16 仍回退整文件解码；单行超长时按行分配（上限为最长单行）。这两点是流式实现的已知边界，契约已注明。
