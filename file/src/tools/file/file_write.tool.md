# 基本信息
```yaml
name: file_write
title: 写入文件
args: file_write --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
按批写入文件（覆盖或新建）。统一参数 `{purpose, targets[]}`。

- **原子写**：先写同目录临时文件再 rename；失败不留半截文件。
- `overwrite=false` 且目标已存在 → 该项 `failed`（不覆盖）。
- `createDirs` 默认 true：自动创建父目录。
- **BOM 控制**：`bom = keep`（默认，保持原文件 BOM 状态；新文件无 BOM）｜ `add`（写 UTF-8 BOM）｜ `strip`（去 BOM）。
- `encoding` 缺省 `auto`：已有文件按原编码回写；新文件写 UTF-8。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。
**并发**：同路径写入自动串行（跨进程文件锁）；批量内重复路径 → 该项 `failed`（`duplicate target`）。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次写入的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["path", "content"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "目标文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "content": {
            "type": "string",
            "description": "文件内容（完整覆盖）"
          },
          "encoding": {
            "type": "string",
            "description": "目标编码（auto | utf-8 | utf-16le | utf-16be | gbk | gb18030 | shift_jis | big5 | euc-kr）；缺省 auto"
          },
          "bom": {
            "type": "string",
            "enum": ["keep", "add", "strip"],
            "description": "BOM 处理；缺省 keep"
          },
          "createDirs": {
            "type": "boolean",
            "description": "自动创建父目录；缺省 true"
          },
          "overwrite": {
            "type": "boolean",
            "description": "允许覆盖已存在文件；缺省 true"
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
  "purpose": "生成配置",
  "results": [
    {
      "path": "C:\\proj\\a.json",
      "status": "ok",
      "action": "created",
      "size": 234,
      "md5": "…",
      "encoding": "utf-8",
      "bom": "none"
    },
    { "path": "C:\\proj\\b.txt", "status": "failed", "error": "target exists (overwrite=false)" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 1, "skipped": 0 }
}
```
