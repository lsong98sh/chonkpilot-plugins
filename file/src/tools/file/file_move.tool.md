# 基本信息
```yaml
name: file_move
title: 移动或重命名
args: file_move --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
按批移动 / 重命名文件或目录。统一参数 `{purpose, targets[]}`。

- 跨卷移动自动回退为「复制成功后删源」；复制失败不删源（宁可留下副本，也不丢数据）。
- 目录移动 = 先递归复制到目标，成功后再逐文件删源。
- `overwrite` 缺省 false：目标已存在 → 该项 `failed`。
- 拒绝 `to` 落在 `from` 内部（自我嵌套）→ 该项 `failed`。

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
      "description": "本次移动的意图（用于审计与展示）"
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
  "purpose": "整理目录",
  "results": [
    { "from": "C:\\proj\\a.txt", "to": "C:\\proj\\docs\\a.txt", "status": "ok", "type": "file" },
    { "from": "C:\\proj\\b.txt", "to": "C:\\proj\\docs\\b.txt", "status": "failed", "error": "target exists" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 1, "skipped": 0 }
}
```
