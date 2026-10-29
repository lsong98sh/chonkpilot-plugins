# 基本信息
```yaml
name: file_find
title: 查找文件
args: file_find --input={RAW-INPUT-FILE}
async: never
category: file
hot: true
runtime: builtin
idempotent: true
output: stdout
timeout: 300
```

# 摘要
四合一查找：**列目录 / glob 匹配 / 内容 grep / 树形遍历**。统一参数 `{purpose, targets[]}`，每个目标 = 一次以 `root` 为根的独立查询。

## 四种模式（可组合）
| 模式 | 触发 | 说明 |
|------|------|------|
| `dir` | 只给 `root` + `depth=1` | 只列直接子项（等价 `ls`） |
| `glob` | 给 `glob` | 文件名模式匹配（如 `**/*.go`） |
| `grep` | 给 `grep` / `pattern` | 内容匹配，返回命中行号与行内容 |
| `tree` | `output=tree` | 递归树形（受 `depth` 限制） |

## 条数上限（硬约束）
- `limit` **默认 2000，硬上限 2000**；超出按**相对路径字典序**截断 → `truncated=true`。
- `matched_total`：`dir` / `glob` / `tree` 下为**精确值**（只遍历不读内容）；`grep` 下省略，改给 `scanned`（已扫文件数）。
- 要拿全量：**收窄 `glob` / 调整 `depth` 后多次调用**，不要把 `limit` 调大（硬上限不可越过）。

## 元信息与哈希
- `meta=none`（默认）：只返回路径。
- `meta=basic`：附加 `size` / `mtime`。
- `meta=hash`：附加 `md5`；**只对已返回条目计算**，并受 `maxHashBytes`（默认 512MB）限制；超限的项标 `hash_skipped="exceeded_limit"` 且结果带 `hash_truncated=true`。

## 内容匹配细节
- `grep` 单文件命中行数按 `maxHitsPerFile`（默认 100）截断，避免单个大日志刷屏。
- `regex=true` 时把 `grep` 当作正则（Go RE2 语法）；缺省按字面量匹配。

**路径要求**：`root` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 整体失败。

# 参数
```json
{
  "type": "object",
  "required": ["purpose", "targets"],
  "additionalProperties": false,
  "properties": {
    "purpose": {
      "type": "string",
      "description": "本次查找的意图（用于审计与展示）"
    },
    "targets": {
      "type": "array",
      "minItems": 1,
      "maxItems": 2000,
      "items": {
        "type": "object",
        "required": ["root"],
        "additionalProperties": false,
        "properties": {
          "root": { "type": "string", "description": "查找根目录（绝对路径 / ~/ / !/）" },
          "glob": { "type": "string", "description": "文件名 pattern，如 **/*.go" },
          "grep": { "type": "string", "description": "内容匹配串（regex=true 时为正则）" },
          "regex": { "type": "boolean", "description": "grep 是否按正则解释；缺省 false" },
          "depth": { "type": "integer", "minimum": 0, "description": "最大递归深度；0=不限，1=只直接子项" },
          "output": {
            "type": "string",
            "enum": ["json", "tree", "list"],
            "description": "输出形态；缺省 json"
          },
          "limit": {
            "type": "integer",
            "minimum": 1,
            "maximum": 2000,
            "description": "返回条目上限；缺省 2000，硬上限 2000"
          },
          "meta": {
            "type": "string",
            "enum": ["none", "basic", "hash"],
            "description": "附带的元信息级别；缺省 none"
          },
          "maxHashBytes": {
            "type": "integer",
            "minimum": 1,
            "description": "meta=hash 时允许读取的总字节上限；缺省 512MB"
          },
          "maxHitsPerFile": {
            "type": "integer",
            "minimum": 1,
            "description": "grep 单文件命中行上限；缺省 100"
          },
          "skipDirs": {
            "type": "array",
            "items": { "type": "string" },
            "description": "跳过的目录名（如 .git / node_modules）"
          },
          "ignoreFiles": {
            "type": "array",
            "items": { "type": "string" },
            "description": "忽略的文件 pattern（如 *.log）"
          },
          "from": { "type": "string", "description": "只保留 mtime ≥ 该日期（YYYY-MM-DD）的项" },
          "to": { "type": "string", "description": "只保留 mtime ≤ 该日期（YYYY-MM-DD）的项" }
        }
      }
    }
  }
}
```

# 返回值
```json
{
  "purpose": "找所有 Go 文件",
  "results": [
    {
      "root": "C:\\proj",
      "status": "ok",
      "truncated": false,
      "matched_total": 42,
      "hash_truncated": false,
      "items": [
        { "rel": "cmd/main.go", "is_dir": false, "size": 1024, "mtime": "2026-10-20T09:00:00Z", "md5": "…" },
        { "rel": "internal/x.go", "is_dir": false, "size": 2048, "hits": [{ "line": 12, "text": "func X()" }] }
      ]
    }
  ],
  "summary": { "total": 1, "ok": 1, "failed": 0, "skipped": 0 }
}
```
