# 基本信息
```yaml
name: file_read
title: 读取文件
args: file_read --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 120
on_timeout: background
```

# 摘要
按批读取文件；统一参数 `{targets[]}`，单文件也写 1 个元素的数组。`maxBytes` 只限制本次调用**所有文件合计的 `content` 字节数**（默认 1MiB），超出后当前文件截断、**需要正文**的后续目标 `skipped`；`infoOnly` 不返回正文、不消耗预算，预算耗尽后仍会读取元信息。`total` 与 `file_md5` 始终是整个文件的结果；内容统一解码为 UTF-8 文本，`encoding` 返回源文件编码；二进制文件不支持（显式 `encoding` 也不能绕过该检查）。`path` 必须是绝对路径、`~/` 或 `!/` 开头，相对路径该项 `failed`。

每个目标支持四种读法：① 全文（默认）；② 行区间 `start`/`limit`；③ 末尾 `tail`；④ 仅元信息 `infoOnly=true`。结果逐项返回；单个文件失败只写在该项 JSON 里（`status=failed`），不中断整批。

**编码**：默认自动识别（BOM → chardet 统计检测 → 可解码候选；二进制先拒绝）。需要精确控制时用 `encoding`；无论源文件是什么编码，`content` 都是解码后的 UTF-8 文本。显式 `encoding` 在读取中转换失败（非法字节序列）时，会自动识别并**二次读取**，结果带 `encoding_requested` 与 `encoding_fallback:true`，`encoding` 为实际识别值。
**全文指标**：`total` / `file_md5` 始终是整个文件的准确值，因此即使 `infoOnly` / 只读区间也会完整扫描文件（超大文件有扫描成本）。
**行号**：`lineNumbers=true` 时每行前缀 `%6d\t`，且最后一行也带换行。
**超限**：单行本身超过本次预算时返回 `status=ok` + `truncated=true` + `code=LINE_TOO_LARGE`（可用的部分正文）；普通预算耗尽时 `truncated_reason=maxBytes`，可用 `next_start` 从下一行继续。
**整体结果**：顶层 `ok` = 本次调用无失败项（`summary.failed==0`），**整体成败以此为准**。逐项运行期失败（含全部 target 失败）不把 MCP `isError` 置真，仍返回结构化 JSON（逐项原因可解析）——这是 file 域的批量约定，避免单文件错误中断整批/放大成 provider 故障；参数级违规（targets 形状/上限、maxBytes 越界）才是 `isError=true` 纯文本。
**错误码**（单项 `code`，均为 `status=failed`）：`BINARY_UNSUPPORTED`（二进制文件不支持）、`ENCODING_UNSUPPORTED`（显式编码不支持）、`START_BEYOND_EOF`（`start` 超出文件行数）、`START_INVALID`（`start` < 1）、`LIMIT_INVALID`（`limit` < 1）、`TAIL_INVALID`（`tail` < 1）、`TAIL_CONFLICT`（`tail` 与 `start/limit` 互斥）、`READ_ERROR`（读取失败）。`LINE_TOO_LARGE` 是唯一可带 `status=ok` 的 code（可恢复截断，见上）。

# 参数
```json
{
  "type": "object",
  "required": ["targets"],
  "additionalProperties": false,
  "properties": {
    "maxBytes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 33554432,
      "default": 1048576,
      "description": "本次调用所有文件返回 content 的合计上限（UTF-8 字节）；只限制 content：超出后当前文件截断，需要正文的后续 targets 标记 skipped；infoOnly 不消耗也不受此限制"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "description": "读取目标；单文件也写成 1 个元素的数组。插件配置 plugins.file.max-targets 可调低（默认 2000），超过配置值将直接参数错误（不静默截断）",
      "items": {
        "type": "object",
        "required": ["path"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "encoding": {
            "type": "string",
            "description": "源编码（auto | utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr）；缺省 auto（chardet 自动识别）；显式值转换失败会自动识别并二次读取，结果带 encoding_fallback"
          },
          "start": {
            "type": "integer",
            "minimum": 1,
            "description": "起始行号（1-based，含）"
          },
          "limit": {
            "type": "integer",
            "minimum": 1,
            "description": "从 start 起读取的行数"
          },
          "tail": {
            "type": "integer",
            "minimum": 1,
            "description": "读取末尾 N 行（与 start/limit 互斥）"
          },
          "lineNumbers": {
            "type": "boolean",
            "description": "输出时给每行加行号前缀"
          },
          "infoOnly": {
            "type": "boolean",
            "description": "只返回 path/size/mtime/encoding/total/file_md5，不返回正文"
          }
        }
      }
    }
  }
}
```

# 返回值
```json
{
  "ok": false,
  "results": [
    {
      "path": "C:\\proj\\a.txt",
      "status": "ok",
      "size": 1234,
      "mtime": "2026-10-21T10:00:00Z",
      "encoding": "gbk",
      "total": 42,
      "file_md5": "…",
      "returned": 42,
      "content": "…",
      "truncated": false
    },
    { "path": "C:\\proj\\b.txt", "status": "failed", "error": "file not found" },
    { "path": "C:\\proj\\c.txt", "status": "skipped", "reason": "maxBytes exhausted" }
  ],
  "summary": { "total": 3, "ok": 1, "failed": 1, "skipped": 1 }
}
```

截断时额外字段：

```json
{ "truncated": true, "truncated_reason": "maxBytes", "next_start": 43 }
```

显式编码转换失败并自动识别二次读取时：

```json
{ "status": "ok", "encoding": "gbk", "encoding_requested": "shift_jis", "encoding_fallback": true }
```

单行本身超过预算时（仍 `status=ok`，正文是该行的可用前缀）：

```json
{ "status": "ok", "truncated": true, "truncated_reason": "line_too_large", "code": "LINE_TOO_LARGE" }
```

预算耗尽后仍可继续读元信息：

```json
{ "path": "…", "status": "ok", "size": 1234, "total": 42, "file_md5": "…" }
```

`total` / `file_md5` 始终是整个文件的统计值；`returned` 是本次实际返回的行数。
