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
```

# 摘要
按批读取一个或多个文件。**所有参数统一为 `{purpose, targets[]}`**：单文件也写成只有 1 个元素的数组（不设两套形状）。

每个目标支持三种读法：① 全文（默认）；② 区间——`start`/`limit`/`tail`/`ranges`；③ 仅元信息 `infoOnly=true`（path/size/mtime/md5/encoding/lines）。结果逐项返回，**参数级违规整体失败，运行期错误逐项 failed**。

**路径要求**：`path` 必须是绝对路径、以 `~/` 开头的用户目录路径或以 `!/` 开头的临时目录路径；相对路径 → 整体失败。
**大文件**：超过 32MB 时只允许 `infoOnly` / `tail` / `limit`，全文读取会被拒绝并提示改用区间。
**编码**：默认自动识别（UTF-16 BOM → UTF-8 → GBK/ShiftJIS/Big5/EUCKR）；需要精确控制时用 `encoding`。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次读取的意图（用于审计与展示，不影响执行）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "description": "读取目标；单文件也写成 1 个元素的数组",
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
            "description": "源编码（auto | utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr）；缺省 auto"
          },
          "maxBytes": {
            "type": "integer",
            "minimum": 1,
            "description": "最多读取的字节数；超出部分截断并标记 truncated=true"
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
          "ranges": {
            "type": "string",
            "description": "行区间列表，如 \"1:10,20:30\"（与 start/limit/tail 互斥）"
          },
          "lineNumbers": {
            "type": "boolean",
            "description": "输出时给每行加行号前缀"
          },
          "infoOnly": {
            "type": "boolean",
            "description": "只返回元信息（path/size/mtime/md5/encoding/lines），不返回正文"
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
  "purpose": "查看配置",
  "results": [
    {
      "path": "C:\\proj\\a.txt",
      "status": "ok",
      "size": 1234,
      "mtime": "2026-10-21T10:00:00Z",
      "md5": "…",
      "encoding": "utf-8",
      "lines": 42,
      "content": "…",
      "truncated": false
    },
    { "path": "C:\\proj\\b.txt", "status": "failed", "error": "file not found" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 1, "skipped": 0 }
}
```
