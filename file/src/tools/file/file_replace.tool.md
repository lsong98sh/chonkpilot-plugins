# 基本信息
```yaml
name: file_replace
title: 替换文本
args: file_replace --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: false
output: stdout
timeout: 120
```

# 摘要
按批做精确文本替换。统一参数 `{purpose, targets[]}`。

- `find` 为空 → 该项 `failed`（不做"清空文件"这种隐式动作）。
- `all` 缺省 true：替换全部出现；`false` 只替换第一处。
- 未找到 `find` → 该项 `failed`（`search not found`），便于调用方发现"条件写错了"。
- `expectMd5` 可选：**锁内先校验**，不一致 → 该项 `skipped`（防并发覆盖别人的改动）。
- **原子写**：临时文件 + rename。

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
      "description": "本次替换的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["path", "find", "replace"],
        "additionalProperties": false,
        "properties": {
          "path": {
            "type": "string",
            "description": "目标文件路径（必须为绝对路径、~/ 或 !/ 开头）"
          },
          "find": {
            "type": "string",
            "description": "要查找的原文字符串（非正则；不能为空）"
          },
          "replace": {
            "type": "string",
            "description": "替换成的文本（可为空串 = 删除匹配）"
          },
          "all": {
            "type": "boolean",
            "description": "替换全部出现；缺省 true（false = 只替换第一处）"
          },
          "expectMd5": {
            "type": "string",
            "description": "期望的当前文件 md5；不一致则跳过该项（防并发覆盖）"
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
  "purpose": "改常量",
  "results": [
    {
      "path": "C:\\proj\\a.go",
      "status": "ok",
      "replacements": 3,
      "md5Before": "…",
      "md5After": "…",
      "diff": "--- a/a.go\n+++ b/a.go\n@@ …"
    },
    { "path": "C:\\proj\\b.go", "status": "skipped", "error": "md5 mismatch" }
  ],
  "summary": { "total": 2, "ok": 1, "failed": 0, "skipped": 1 }
}
```
