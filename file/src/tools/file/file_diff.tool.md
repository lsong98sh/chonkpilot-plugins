# 基本信息
```yaml
name: file_diff
title: 比较文件
args: file_diff --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 120
```

# 摘要
按批比较两个文件，输出 unified diff（只读）。统一参数 `{purpose, targets[]}`，每个目标是一对 `left` / `right`。

- 两侧内容完全一致 → 该项 `status=ok` + `identical=true` + 空 `diff`。
- 默认 `context=3`；可调到 0（只显示变更行）。
- 大文件（>16MB 任一侧）→ 该项 `failed`，提示改用 `file_find` 的 `meta=hash` 比对指纹。

**路径要求**：`left` / `right` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次比较的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["left", "right"],
        "additionalProperties": false,
        "properties": {
          "left": {
            "type": "string",
            "description": "左侧文件（旧版本）路径"
          },
          "right": {
            "type": "string",
            "description": "右侧文件（新版本）路径"
          },
          "context": {
            "type": "integer",
            "minimum": 0,
            "description": "diff 上下文行数；缺省 3"
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
  "purpose": "确认改动",
  "results": [
    {
      "left": "C:\\proj\\a.old",
      "right": "C:\\proj\\a.new",
      "status": "ok",
      "identical": false,
      "added": 3,
      "removed": 1,
      "diff": "--- a.old\n+++ a.new\n@@ …"
    }
  ],
  "summary": { "total": 1, "ok": 1, "failed": 0, "skipped": 0 }
}
```
