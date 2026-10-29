# 基本信息
```yaml
name: file_patch
title: 应用补丁
args: file_patch --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
按批应用 unified diff 补丁。统一参数 `{purpose, targets[]}`。

- `patch` 为标准 unified diff 文本（`---`/`+++`/`@@` 头）。
- **上下文不匹配即整项失败**（不做模糊匹配、不做 fuzz），避免改错位置。
- `expectMd5` 可选：锁内先校验，不一致 → `skipped`。
- **原子写**：先写临时文件再 rename；失败不动原文件。
- 补丁里若出现目标文件之外的文件头 → 该项 `failed`（不允许借补丁写到别的文件）。

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
      "description": "本次打补丁的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["path", "patch"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "目标文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "patch": {
            "type": "string",
            "description": "unified diff 文本（含 @@ 块）"
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
  "purpose": "应用修复补丁",
  "results": [
    {
      "path": "C:\\proj\\a.go",
      "status": "ok",
      "hunks": 2,
      "md5Before": "…",
      "md5After": "…"
    },
    { "path": "C:\\proj\\b.go", "status": "failed", "error": "context mismatch at hunk 1" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 1, "skipped": 0 }
}
```
