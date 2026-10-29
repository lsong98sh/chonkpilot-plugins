# 基本信息
```yaml
name: file_delete
title: 删除文件
args: file_delete --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 120
```

# 摘要
按批删除文件或目录。统一参数 `{purpose, targets[]}`。

- 删目录必须显式 `recursive=true`，否则该项 `failed`（防误删整棵树）。
- `missingOk` 缺省 false：目标不存在 → `failed`；`true` → `skipped`（幂等重试友好）。
- 删除前记录 `md5`（仅文件）与 `size`，随结果返回，便于审计与事后恢复判断。
- **不做回收站**：直接删除；调用方需自行负责（重要数据先 `file_copy` 备份）。

**路径要求**：`path` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。
**安全**：拒绝删除文件系统根、盘符根、用户主目录根（`C:\`、`C:\Users` 等），该项 `failed`。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次删除的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["path"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "要删除的文件或目录路径"
          },
          "recursive": {
            "type": "boolean",
            "description": "允许删除非空目录；缺省 false"
          },
          "missingOk": {
            "type": "boolean",
            "description": "目标不存在时视为 skipped 而非 failed；缺省 false"
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
  "purpose": "清理临时产物",
  "results": [
    { "path": "C:\\proj\\tmp.log", "status": "ok", "type": "file", "size": 88, "md5": "…" },
    { "path": "C:\\proj\\dist", "status": "ok", "type": "dir", "removedFiles": 12 }
  ],
  "summary": { "total": 2, "ok": 2, "failed": 0, "skipped": 0 }
}
```
