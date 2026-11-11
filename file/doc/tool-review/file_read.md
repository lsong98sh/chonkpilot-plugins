---
plugin: file
tool: file_read
contract: tools/file/file_read.tool.md
handler: src/lib/filetool/readwrite.go:42
reviewed_at: 2026-11-08
status: verified
---

# file_read 审查（复核）

> 本文只复核“最终落地状态”与已知待定项，不重复《工具审查.md》§共通基线第 104 行起的调用链、展示名、`_meta`、错误外壳结论。

## 1. 一句话结论

可用；批量/预算/编码模型与契约基本一致，但有 2 个 P1：显式 `encoding` 可绕过二进制拒绝；插件配置的 `maxTargets`（默认 200）会静默截断 schema 允许的 2000 项目标。

## 2. 调用链

LLM → gateway `doCall`（注入 `tool_call_display_name`，按 manifest `timeout_sec=300` / 工具 `_meta` 裁决同步异步）→ 插件 MCP `tools/call` → `contract.Register`（`contract/contract.go:106`）→ `filePlugin.call`（`src/main.go:88`）→ `applyDefaults`（`src/main.go:126`）→ `dispatch`（`src/main.go:97`，先 `applyCallMeta`、再 `checkSandboxWritable`）→ `toolset.Dispatch`（`lib/toolset/toolset.go:36`，goroutine + ctx select）→ `filetool.runRead`（`lib/filetool/readwrite.go:42`）→ `finish` 输出 `{results[],summary}`（`lib/filetool/filetool.go:101`）。

## 3. 契约面

- schema 与实际参数一致：`maxBytes`(1..33554432)、`targets[1..2000]`、`path` 必填，`encoding/start/limit/tail/lineNumbers/infoOnly` 可选，`additionalProperties:false`（`file_read.tool.md:28-85`）；运行时**不做** schema 强校验，以 handler 解析为准（[推断] 依 `contract/contract.go:63-92` 仅解析 name/title/hot/schema）。
- 契约 `async: never` / `timeout: 120`（`:6,:12`）未被 `contract.Parse` 读取（框架 `contract/contract.go:72-80` 只取 name/title/hot），插件只上报 `_meta.hot`；gateway 实际缺省 `async=auto`、server 级超时取 manifest `mcp.timeout_sec=300`。→ 见 §6 P2。
- `RANGE_UNSUPPORTED` 仍在摘要（`:24`）但 schema 已无 `ranges`；运行时防御保留（`readwrite.go:101-104`），命中时该项 `status=failed`、`code=RANGE_UNSUPPORTED`，且该分支在 `os.Stat` 之后（不存在的路径先报 stat 错）。

## 4. 实现面（复核结论）

| 基线待定项 | 最终状态 | 证据 |
|---|---|---|
| 显式 `encoding` 绕过二进制探测 | **仍成立**：`want!="auto"` 在 `detectEncoding/Binary` 之前直接返回 | `readwrite.go:379-396` vs `:398-401` |
| `RANGE_UNSUPPORTED` 仅运行时防御 | 仍成立；且只对已 stat 的 target 生效 | `readwrite.go:101-104` |
| `maxBytes` 为跨 target 合计预算 | 仍成立；`remaining` 跨 target 递减，超限后后续全部 `skipped`（含后续 `infoOnly`） | `readwrite.go:47-79` |
| `total`/`file_md5` 始终全文件 | 仍成立：单遍 `TeeReader` + 流式行计数 | `readwrite.go:137-168,343-374` |

其他：单行过大 → `LINE_TOO_LARGE` 且 `status=ok`；`start` 超出 EOF → `code=START_BEYOND_EOF` 仍 `status=ok`（`readwrite.go:359-374`）。无写副作用、幂等。

## 5. 返回面：LLM 实际看到的所有形态

| 形态 | isError | 关键字段/文本 |
|---|---|---|
| 全部成功 | false | `results[*].status=ok` + `size/mtime/encoding/total/file_md5/returned/content/truncated`；`summary{total,ok,failed,skipped}` |
| 部分成功 | false | 成功项 + `status=failed`（`error`，部分含 `code`）/`status=skipped`（`reason`） |
| 全部失败（运行期） | **false** | 每项 `status=failed`，`summary.failed=N`；整体仍 JSON |
| 参数级失败 | true | 纯文本：`targets is required` / `targets must be an array` / `targets length must be 1..2000` / `maxBytes must be <= 33554432`（`filetool.go:75-96`、`readwrite.go:47-53`） |
| 沙箱越界（读工具不触发） | — | 读不受可写白名单限制（`sandbox.go:23-31`） |
| 二进制 | false（批量） | 该项 `status=failed, code=BINARY_UNSUPPORTED, binary=true, error="binary file (not supported)"` |
| 编码不支持 | false（批量） | 该项 `status=failed, code=ENCODING_UNSUPPORTED` |
| 截断 | false | `truncated=true, truncated_reason=maxBytes|line_too_large, code, next_start`；`content` 为部分内容 |
| 跳过 | false | `status=skipped, reason="maxBytes exhausted"` |
| 目录 | false（批量） | `status=failed, error="is a directory (use file_find to list)"` |
| 取消/超时 | true | `toolset.Dispatch` 返回 `context canceled/deadline exceeded` 纯文本；底层读取结果丢弃（`toolset.go:58-63`） |
| 内部错误 | true | `marshal result: ...`，或 panic 收口 `tool X panic: ...`（`toolset.go:42-45`） |

体积：`content` 合计默认 1 MiB、硬上限 32 MiB；`infoOnly` 只返回元信息。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | 显式 `encoding` 时二进制文件不再被拒绝，`content` 可能是乱码/含 NUL 的文本，而约定称“二进制文件不支持” | `readwrite.go:379-396`；契约 `:16,:24` | 模型把二进制当文本继续处理/编辑 |
| P1 | 插件配置 `maxTargets` 默认 200，`applyDefaults` 对超过的 targets **静默截断**，不产生 `skipped` 项；schema 允许 2000 | `main.go:145-149`；`config.go:42,64`；契约 `:43` | 请求 500 个只执行 200 个，模型只能靠 `summary.total` 察觉 |
| P2 | `LINE_TOO_LARGE`、`START_BEYOND_EOF` 以 `status=ok` + `code` 返回，与其它 `code`（failed）语义不一致 | `readwrite.go:359-374`；契约 `:24` | 模型需额外判断 `code` 才能识别异常 |
| P2 | 预算耗尽后连不占预算的 `infoOnly` 目标也 `skipped` | `readwrite.go:57-61` | 同一批里元信息读取被无谓丢弃 |
| P2 | 运行时还有 `START_INVALID/LIMIT_INVALID/TAIL_INVALID/TAIL_CONFLICT`，契约错误码表未列 | `readwrite.go:111-125` | schema 不生效时模型无文档可查 |
| P2 | `async:never/timeout:120` 未进入 tools/list `_meta`；实际缺省 async=auto、server 超时 300s | 框架 `contract/contract.go:72-80`；插件 `contract.go:101-105`；`manifest.json mcp.timeout_sec=300`；契约 `:6,:12` | 长读取可能转后台，契约的同步承诺不成立（[待核实] 网关对 builtin 插件路由的具体覆盖） |
| P2 | 全部 target 失败时 `isError=false`（file 域约定） | `filetool.go:98-127` | 只看 isError 的模型会误判成功 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 显式 `encoding` 时先做二进制探测（0x00），或契约明确“显式编码跳过探测” | 优化 | 避免乱码误判 | 低（readwrite.go 一处） | 行为收紧，需同步测试/契约 | 建议现在做 |
| 截断后补 `summary.truncated_targets` / 被丢弃的 target 条目，而非静默截断 | 改善 | 模型可发现漏读 | 低（main.go + 契约） | 新增字段，向后兼容 | 建议现在做 |
| 预算耗尽仅跳过需内容的 target，允许 `infoOnly` 继续 | 改善 | 元信息仍可得 | 低 | 行为变化小 | 排期做 |
| 把 `code` 语义统一（非 ok 的 code 一律 failed），或契约明示 ok+code 组合 | 改善 | 少一次猜测 | 低 | 可能 breaking（调用方看 status） | 仅记录 |
| 把 async/timeout 通过 `_meta` 透出（插件 `contract.Register` 补 async/timeout） | 优化 | 契约与运行时一致 | 中（框架 + 插件） | 影响全部 file 工具 | 排期做 |

## 8. 建议的测试/验收（不执行）

- `encoding="utf-8"` 打开含 `\x00` 的文件：期望 `BINARY_UNSUPPORTED`（当前会返回文本）。
- `targets` 长度 201（默认配置）：期望出现 `skipped` 或显式截断标记，而非静默丢弃。
- 首个文件被 `maxBytes` 截断后跟一个 `infoOnly` 目标：确认是否应返回元信息。
- `start` 超出 EOF / `LINE_TOO_LARGE`：断言 `isError/status/code` 组合符合契约。
- gateway 层 `async`：确认长读取是否可能返回 pending（契约要求 never）。

## 9. 基线修正

- 基线 §6“契约 YAML 的 async/timeout 决定 gateway 行为”在 file 插件**不成立**：插件侧 `contract.Parse` 只消费 name/title/hot/schema，`async/timeout` 未进入 tools/list；实际由 gateway 缺省（auto）+ manifest `timeout_sec` 决定。
- 基线 §8 的四项 file_read 结论均仍成立（见 §4）。
## 10. 修复记录（2026-11-08）

已按审查结论修复并补测试：

| 原问题 | 修复 | 验证 |
|---|---|---|
| P1 显式 `encoding` 绕过二进制探测 | `chooseEncoding` 在非 UTF-16 的显式编码分支前先做二进制探测（UTF-16 合法含 NUL，例外） | `TestReadExplicitEncodingStillRejectsBinary` |
| P1 `maxTargets` 默认 200 vs schema 2000、超限静默截断 | 单一来源 = `filetool.TargetsHardLimit`（2000）：配置默认/上限、10 个契约 `targets.maxItems`、硬上限全部相等；超配置值改为**参数错误**，不再切片 | `TestTargetsLimitSingleSource`、`TestApplyDefaultsUsesSettings` |
| P2 预算耗尽后 `infoOnly` 也被 skipped | `maxBytes` 只限制 `content`；`remaining<=0` 时仅跳过需要正文的目标，`infoOnly` 继续读取元信息 | `TestReadInfoOnlyAfterBudgetExhausted` |
| P2 `START_BEYOND_EOF` 以 `status=ok` 返回 | 改为 `status=failed` + `code` + `error`；`LINE_TOO_LARGE` 保留 `status=ok + truncated` 并在契约明示 | `TestReadStartBeyondEOFIsFailed` |
| P2 `START_INVALID/LIMIT_INVALID/TAIL_INVALID/TAIL_CONFLICT` 未文档化 | 契约错误码表补齐；移除已下架 `ranges` 的 `RANGE_UNSUPPORTED` 公开说明 | 契约核对 |
| P2 全部 target 失败 `isError=false`，只看 isError 会误判 | 返回信封新增顶层 `ok`（`failN==0`）；契约明确“整体看 `ok`，`isError` 只表示参数级失败” | `TestReadAllFailedEnvelopeOkFalse` |
| P2 契约 `async/timeout` 不进入 `_meta` | 平台批次已修（`contract.Parse + MetaFor`）；本工具补 `_meta` 断言 | `TestFileReadContractMeta` |

## 11. 编码识别与显式编码回退（2026-11-11）

- **检测改用 chardet**：`detectEncoding` 顺序 = BOM → NUL 二进制判定 → 严格 UTF-8 快判 → `joshtechnologygroup/chardet` 主结果/候选 → 不支持/短样本回落 `encodingx` 候选打分。极短输入（<8 字节）跳过 chardet（库会 panic）；`chardetSafe` 仍包 recover。
- **二进制**：`textx.IsBinary` 先行，二进制返回 `BINARY_UNSUPPORTED`；显式编码也不能绕过（UTF-16 例外）。
- **显式编码转换失败回退**：`encoding` 非 auto 时，读取/逐行校验失败（非法字节序列；显式 UTF-8 按 `utf8.Valid` 逐行校验）→ 用 `auto` **二次读取**；结果保留实际 `encoding`，并附 `encoding_requested`、`encoding_fallback:true`。
- 测试：`TestDetectEncodingChardetAndBinary`、`TestReadExplicitEncodingFallback`。
