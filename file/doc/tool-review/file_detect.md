---
plugin: file
tool: file_detect
contract: file/src/tools/file/file_detect.tool.md
handler: file/src/lib/filetool/query.go:337-380
reviewed_at: 2026-11-08
status: verified
---

# file_detect 审查

## 1. 一句话结论
**基本可用（P1）**：只读、返回候选与样本的设计合理，自动检测链与 `file_iconv` 共用；主要缺陷是 `sampleBytes` **没有上限**会直接按请求字节数分配内存，以及若干返回字段对空文件/二进制文件不满足契约的字面约定。

## 2. 调用链
`filePlugin.call` → `applyDefaults`（`main.go:126-150`；只注入 `maxBytes`/`encoding`，本工具不消费）→ `dispatch` → `checkSandboxWritable`（`file_detect` 不在 `writeTools`，直接放行，`sandbox.go:22-40`）→ `toolset.Dispatch` → `runDetect`（`query.go:338`）→ 逐项 `resolvePath(path)` → `detectOne`（`query.go:356`）→ `readHead(path, sampleBytes)`（`readwrite.go:620-637`）→ `detectEncoding`（`filetool/encoding.go:38-70`）。

## 3. 契约面
- YAML：`async: never`、`timeout: 120`、`idempotent: true`、`hot: true`（契约 3-12 行）。async/timeout/idempotent 在插件面不透出（C1）。
- “四层判定 / LLM 仲裁”（契约 18-22 行）与实现一致：工具只做 BOM、二进制、候选打分，不做最终决定（`encoding.go:36-70`）。
- “`confidence ∈ high|medium|low`、`low` 时不要直接 iconv 先看 sample”（契约 30-33 行）：实现把置信度判定委托 `encodingx.Confidence`（`encoding.go:64`）；空文件/无候选时 `confidence` 为空字符串（`query.go:372`），不在枚举内。
- schema `sampleBytes` 只有 `minimum: 64`，**没有 maximum**（契约 55-58 行）；实现按该值 `make([]byte, n)`（`readwrite.go:626`）。
- 摘要说“返回 `candidates[]` + `scores[]`”（契约 22 行），返回示例却是 `candidates:[{encoding,score}]`（契约 77-81 行）——文档内部不一致。
- 返回示例含 `encoding`（契约 76 行），实现在 `d.Encoding == ""` 时**省略**该字段（`query.go:369-371`）。
- “`sampleBytes` 决定 sample 长度”（契约 57 行）：实际 `sample` 由 `encodingx.Sample(s, 512)` 截到 512 rune（`encoding.go:46,49,66`），与 `sampleBytes` 无线性关系。
- LLM 可见描述仅首段（C4），但首段已包含“由调用方看样本仲裁”的核心定位；四层方法与 low 置信度处置建议不可见。

## 4. 实现面
- 参数：`parseRequest` 校验 targets 形状（`filetool.go:75-96`）；`path` 强校验绝对路径/`~/`/`!/`（`query.go:345`）。
- 取样：`sampleBytes` 缺省 65536，`<64` 强制 64，**无上限**（`query.go:358-362`）；`readHead` 打开文件、按 n 分配缓冲、单次 `Read`（`readwrite.go:620-637`）。
- 判定：`detectEncoding`：
  - BOM 命中 → `bom`=编码名、`encoding`=编码名、`confidence="high"`、`sample`=解码后 512 rune（`encoding.go:40-53`）；
  - `textx.IsBinary`（取样内出现 NUL）→ `binary=true`、`encoding="binary"`、`confidence="high"`、无 sample/candidates（`encoding.go:54-59`）；
  - 否则 `encodingx.Candidates(data)` 打分 → `encoding`=第一名、`confidence`=util 判定、`sample`=按第一名解码（`encoding.go:60-68`）。
- 输出构造：`binary`/`bom` 恒有；`encoding`/`candidates`/`sample` 为空则省略；`confidence` 恒有（可能为空串）（`query.go:366-379`）。
- 硬限制：无文件大小上限（只读头部，按 `sampleBytes` 分配）；无条目/字节总量预算（targets 最多 2000，每项 sample ≤512 rune）。
- 副作用：无（只读，不写盘、不建临时文件）。
- 并发/幂等：纯读，幂等成立。
- 资源：`readHead` defer Close；无 goroutine；单次 Read 在大样本下的峰值 = `sampleBytes`。

## 5. 返回面：LLM 实际看到的所有形态
1. **正常文本（有 BOM / 高置信）**：`{path,status:"ok",binary:false,bom:"utf-8",encoding:"utf-8",confidence:"high",sample:"..."}`，isError=false。
2. **正常文本（多候选）**：追加 `candidates:[{encoding,score}...]`；`confidence` 可能为 `low`，`sample` 供仲裁。
3. **二进制**：`{path,status:"ok",binary:true,bom:"none",encoding:"binary",confidence:"high"}`；无 `sample`、无 `candidates`。
4. **空文件 / 无法判定**：`{path,status:"ok",binary:false,bom:"none",confidence:""}`；**无 `encoding`、无 `candidates`、无 `sample`**。模型若按“必须读取 encoding”解析会踩空。
5. **部分成功**：读失败/是目录/路径非法 → 该项 `{path,status:"failed",error:"..."}`，isError=false。
6. **全部失败**：isError=false，仅 `summary.failed=total`（C3）。
7. **跳过**：本工具不产生 `skipped`。
8. **截断**：不截断；`targets` 超配置上限（默认 200）时静默丢弃（C2）。
9. **参数级失败**：targets 形状违规 → 纯文本 isError=true。
10. **沙箱**：不适用（只读，永不被沙箱拒绝）。
11. **异步/超时/取消**：C1 适用时先返回后台受理；ctx 结束后 `context canceled`/`context deadline exceeded`（isError=true）；只读操作，丢弃结果无副作用。
12. **内部错误**：超大 `sampleBytes` 导致分配失败 → panic 被收口为 `tool file_detect panic: ...`（isError=true）或进程级 OOM；`marshal result: ...`。

## 6. 一致性与问题
| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P1 | `sampleBytes` 无上限：`make([]byte, n)` 直接按 LLM 提供的整数分配 | `query.go:358-362`；`readwrite.go:626`；契约 54-59 行 | 传入超大值即内存耗尽/进程 OOM（DoS 面） |
| P2 | 空文件/无候选时 `confidence` 为空串，不在 `high|medium|low` 枚举内；`encoding` 字段被省略 | `query.go:369-373`；契约 31、76 行 | 模型按契约解析会得到空值/缺字段 |
| P2 | `sample` 恒被 `encodingx.Sample(...,512)` 截到 512 rune，与 `sampleBytes` 语义脱节 | `encoding.go:46,49,66`；契约 57 行 | 模型以为加大 `sampleBytes` 能拿到更长样本 |
| P2 | 文档内部不一致：摘要写 `scores[]`，示例写 `candidates[].score` | 契约 22、77-81 行 | 模型对返回结构预期摇摆 |
| P2 | 二进制时 `encoding="binary"`、`confidence="high"` 未在契约说明 | `encoding.go:54-59` | 字段取值超出文档，模型可能把 “binary/high” 误当文本高置信 |
| P2 | 只取样头部：NUL 在 `sampleBytes` 之后时 `binary=false`，交给 `file_iconv` 会二次失败或按文本转换 | `readwrite.go:620-637`；契约 19 行 | 检测结论对“头文本+后二进制”的混合文件不稳 |

跨工具共通问题 C1–C5 同样适用，见 `00-index.md`。

## 7. LLM 视角优化 / 改善 / 增强
| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| `sampleBytes` 加硬上限（如 4MiB）与 `maximum` schema；超限 clamp 或报错 | 优化 | 防 OOM，行为可预期 | 低（1 文件） | schema 收紧，需契约同步 | 建议现在做（安全） |
| 空文件/无法判定也返回 `encoding:""`、`confidence:"unknown"` 等显式枚举 | 改善 | 模型稳定解析，不再踩空 | 低 | 新增枚举值，契约同步 | 建议现在做 |
| 返回 `sample_bytes_used` 与 `sample_truncated`，说明样本来自头部 | 增强 | 模型知道检测覆盖范围 | 低 | 新增字段 | 排期做 |
| `binary` 时补 `reason:"nul_bytes"`、`nul_ratio` | 增强 | 可解释 | 低 | 新增字段 | 排期做 |
| 契约统一 `candidates[].score` 说法，补二进制字段取值 | 改善 | 文档自洽 | 低 | 仅文档 | 建议现在做 |

## 8. 建议的测试/验收（不执行）
- `sampleBytes=64/65536/10^9`：断言分配上限与错误形态。
- 空文件、仅 BOM 文件、纯 NUL 文件：断言 `encoding/confidence/candidates/sample` 字段集合符合契约。
- 头部文本 + 尾部 NUL（NUL 在 64KiB 之后）：断言检测结论（当前预期 binary=false）。
- 多候选 GBK/Shift-JIS/Big5 样本：断言 `confidence=low`、`candidates` 排序、`sample` 可读。
- 目录路径：断言 failed 文案。
- 300 targets：断言 C2 截断。

## 9. 基线修正
无。基线 4 成立：`file_detect` 不做 symlink 解析、不受写白名单限制（`sandbox.go:22-31` 不含 detect）。
## 10. 修复记录（2026-11-09）

本次只改 `file_detect`（`query.go` 的 `detectOne`）与其复用的 `readHead`、契约/测试；检测链 `detectEncoding` 未改。

| 原条目 | 处理 | 实现/证据 |
|---|---|---|
| P1 `sampleBytes` 无上限，`make([]byte, n)` 直接按请求分配 | **已修** | `maxSampleBytes = 4 << 20`；`n > maxSampleBytes` → 该项 `failed` + `code:"SAMPLE_TOO_LARGE"`、错误 `sampleBytes too large (max 4MiB)`，**先于读取与分配**返回；schema 增加 `maximum: 4194304`。 |
| P2 空文件/无候选时 `confidence` 为空串且省略 `encoding` | **已修** | 空文件（0 字节）显式返回 `encoding:"unknown"`、`confidence:"low"`、`binary:false`、`bom:"none"`，不返回 sample/candidates；`confidence`/`encoding` 恒存在（二进制为 `binary`/`high`）。 |
| P2 `sample` 恒被截到 512 rune，与 `sampleBytes` 语义脱节 | **已契约化并测试** | 契约明确：`sampleBytes` 只控制**检测窗口**，`sample` 最多 512 rune；`TestDetectSampleBytesFullHead` 验证小文件 + 大窗口时头部完整读入、sample 内容正确。 |
| P2 文档内部不一致：摘要 `scores[]` vs 示例 `candidates[].score` | **已修** | 契约统一为 `candidates: [{encoding,score}]`，删除 `scores[]` 说法。 |
| P2 二进制时 `encoding="binary"`、`confidence="high"` 未在契约说明 | **已修（契约化）并测试** | 契约补二进制字段取值与“无 sample/candidates”；`TestDetectBinaryFields` 覆盖。 |
| P2 只取样头部：NUL 在窗口之后时 `binary=false` | **保留为已知边界，已注明** | 契约写明“只看头部”；`sampleBytes` 上限不改变该边界。 |

**额外修复**
- `readHead` 改用 `io.ReadFull`（接受 `EOF`/`ErrUnexpectedEOF` 并返回实际字节），避免单次 `Read` 短读导致 sample/检测窗口不完整。
- 测试：`TestDetectSampleBytesTooLarge`、`TestDetectEmptyFileFields`、`TestDetectBinaryFields`、`TestDetectSampleBytesFullHead`。

## 11. chardet 检测（2026-11-11）

- `detectEncoding` 主判据换为 `joshtechnologygroup/chardet`：BOM → NUL 二进制 → 严格 UTF-8 快判 → chardet 主结果/候选 → 不支持编码/极短样本（<8 字节，库会 panic）回落内置候选打分。`chardetSafe` 包 recover，检测失败不 panic。
- `file_detect` 的 `binary` / `confidence` / `candidates` 字段与上限不变；`file_read` 的显式编码回退见 `file_read.md` §11。
- 测试：`TestDetectEncodingChardetAndBinary`（GB18030/Big5/UTF-8/含 NUL 二进制）。
