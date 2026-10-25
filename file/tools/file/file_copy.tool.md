# 基本信息
```yaml
name: file_copy
title: 复制文件
args: file_copy --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
按批复制文件或目录（递归）。统一参数 `{purpose, targets[]}`。

- 目录复制默认递归（`recursive` 缺省 true）。
- `overwrite` 缺省 false：目标已存在 → 该项 `failed`。
- 保留源文件不变；复制失败不留半截目标文件（先写临时文件再 rename）。
- 拒绝 `to` 落在 `from` 内部 → 该项 `failed`。

**路径要求**：`from` / `to` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次复制的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["from", "to"],
        "additionalProperties": false,
        "properties": {
          "from": { "type": "string", "description": "源路径" },
          "to": { "type": "string", "description": "目标路径" },
          "overwrite": {
            "type": "boolean",
            "description": "目标已存在时是否覆盖；缺省 false"
          },
          "recursive": {
            "type": "boolean",
            "description": "源为目录时是否递归复制；缺省 true"
          },
          "createDirs": {
            "type": "boolean",
            "description": "自动创建目标父目录；缺省 true"
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
  "purpose": "备份配置",
  "results": [
    { "from": "C:\\proj\\a.json", "to": "C:\\proj\\backup\\a.json", "status": "ok", "bytes": 234 }
  ],
  "summary": { "total": 1, "ok": 1, "failed": 0, "skipped": 0 }
}
```
