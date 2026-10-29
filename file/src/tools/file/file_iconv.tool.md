# 基本信息
```yaml
name: file_iconv
title: 转换文件编码
args: file_iconv --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 300
```

# 摘要
按批转换文件编码（如 GBK → UTF-8）。统一参数 `{purpose, targets[]}`。

## 源编码
- `from` 缺省 `auto`：复用 `file_detect` 的检测链（UTF-16 BOM → UTF-8 → GBK/ShiftJIS/Big5/EUCKR）。
- **低置信度拒绝自动转**：GBK / Big5 / Shift-JIS 互相撞车时 → 该项 `failed`，错误码 `encoding_undetermined`，并附候选；**要求调用方显式指定 `from`**。
- 显式给 `from` 时不做检测，直接按该编码解码（解不通 → `decode_error`）。

## 目标与 BOM
- `to` 必填（如 `utf-8` / `gb18030` / `utf-16le`）。
- `bom = keep`（默认：原文件有 BOM 则保留到目标编码；无则不添加）｜ `add`（目标编码支持则加）｜ `strip`（一律去掉）。
- **中文目标推荐 `gb18030`**（GBK 的超集，覆盖更全）。

## 幂等与原子性
- 源已是目标编码 → `status=skipped` + `reason="already_target"`，**不重写文件**（幂等）。
- **原子写**：同目录临时文件 + rename；失败不留半截文件。
- `expectMd5` 可选：锁内校验，不一致 → `skipped`。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次转换的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["path", "to"],
        "additionalProperties": false,
        "properties": {
          "path": { "type": "string", "description": "目标文件路径" },
          "to": {
            "type": "string",
            "description": "目标编码（utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr）"
          },
          "from": {
            "type": "string",
            "description": "源编码；缺省 auto（低置信度会拒绝，要求显式指定）"
          },
          "bom": {
            "type": "string",
            "enum": ["keep", "add", "strip"],
            "description": "BOM 处理；缺省 keep"
          },
          "expectMd5": {
            "type": "string",
            "description": "期望的当前文件 md5；不一致则跳过该项"
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
  "purpose": "统一为 UTF-8",
  "results": [
    {
      "path": "C:\\proj\\a.txt",
      "status": "ok",
      "from": "gbk",
      "to": "utf-8",
      "bom": "none",
      "bytesBefore": 1000,
      "bytesAfter": 1450,
      "md5Before": "…",
      "md5After": "…"
    },
    {
      "path": "C:\\proj\\b.txt",
      "status": "failed",
      "error": "encoding_undetermined",
      "candidates": ["gbk", "shift_jis", "big5"]
    },
    { "path": "C:\\proj\\c.txt", "status": "skipped", "reason": "already_target" }
  ],
  "summary": { "total": 3, "ok": 1, "failed": 1, "skipped": 1 }
}
```
