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
on_timeout: background
```

# 摘要
四合一查找：**列目录 / glob 匹配 / 内容 grep / 树形遍历**。统一参数 `{targets[]}`，每个目标 = 一次以 `root` 为根的独立查询。

## 四种模式（可组合）
| 模式 | 触发 | 说明 |
|------|------|------|
| `dir` | `output=dir`（缺省 `depth=1`） | 只列直接子项（等价 `ls`；目录也返回，`is_dir:true`；输出形态同 `json` 的 `items`） |
| `glob` | 给 `glob` | 文件名/相对路径模式匹配（如 `**/*.go`；目录命中时也作为 `is_dir:true` 项返回） |
| `grep` | 给 `grep` / `pattern` | 内容匹配，返回命中行号与行内容（只返回命中文件，不返回目录） |
| `tree` | `output=tree` | 递归树形（目录节点与文件节点都渲染；受 `depth` 限制） |

## 条数上限（硬约束）
- `limit` **默认 2000，硬上限 2000**；超出按**相对路径字典序前 N 项**截断 → `truncated=true`（同一棵树重复调用结果稳定，不受文件系统遍历顺序影响）。
- `matched_total`：`dir` / `glob` / `tree` 下为精确值（目录项也计数）；`grep` 下只统计命中文件数，同时给出 `scanned`（已扫描文件数）。
- `grep` 另有**全局命中行预算** `maxTotalHits`（默认 2000，硬上限 20000，超出按上限收敛）：跨文件累计命中达到上限即停止追加，返回 `hits_truncated=true` 且 `truncated=true`；此时 `scanned` / `matched_total` 为**截至截点**的值。
- 要拿全量：**收窄 `glob` / 调整 `depth` 后多次调用**，不要把 `limit` 调大（硬上限不可越过）。

## depth 语义
- **省略 `depth`**：`output=dir` 默认 `1`（只看直接子项）；其他输出模式默认 `0`。
- **显式 `depth` 始终优先**：`depth=0` = 不限层级；`depth=1` = 只列直接子项。深度边界处的目录会被返回但不再下钻。
- `depth<0` → 该项 `failed`（`depth must be >= 0`）。

## 内置跳过目录（始终生效，`skipDirs` 只能追加、不能移除）
`.git` `.svn` `node_modules` `.trae` `.chonkpilot` `__pycache__` `.venv` `venv` `build` `dist` `.next` `.nuxt`

## 元信息与哈希
- `meta=none`（默认）：只返回 `rel` / `is_dir`（目录项不带 size/mtime）。
- `meta=basic`：附加 `size` / `mtime`。
- `meta=hash`：附加 `md5`（仅文件；同时带 `size` / `mtime`）；**只对已返回条目计算**，并受 `maxHashBytes`（默认 512MB）限制；超限的项标 `hash_skipped="exceeded_limit"` 且结果带 `hash_truncated=true`；读取失败标 `hash_skipped="unreadable"`。

## 内容匹配细节
- `grep` 单文件命中行数按 `maxHitsPerFile`（默认 100）截断，避免单个大日志刷屏。
- 每行内容按 **500 rune** 截断（超限追加 `…`）。
- 全局命中行预算 `maxTotalHits`（默认 2000 / 硬上限 20000）：累计命中达到上限后停止追加，返回 `hits_truncated=true`。
- 单文件流式扫描（读头部判二进制/编码后逐行匹配，不整文件入内存）；UTF-16 因按 `\n` 切字节不安全，回退整文件解码。
- `regex=true` 时把 `grep` 当作正则（Go RE2 语法）；缺省按字面量匹配。

## 日期过滤
- `from` / `to` 为 `YYYY-MM-DD`，按**本地时区**解释（与文件 mtime 的本地日期对齐）；`from`/`to` 当天均包含。

## 输出字段
- `output=json`（缺省）或 `output=dir` → `items[]`（含 `rel` / `is_dir`，按 `meta` 附带 `size` / `mtime` / `md5` / `hash_skipped` / `hits`）。
- `output=list` → `lines`：`[rel...]`。
- `output=tree` → `text`：缩进树形文本，目录行以 `/` 结尾。

**路径要求**：`root` 必须是绝对路径、`~/` 或 `!/` 开头；相对路径 → 该项 `failed`。`root` 必须是**真实目录**：普通文件或 symlink → 该项 `failed`（不再静默返回空结果）。

# 参数
```json
{
  "type": "object",
  "required": ["targets"],
  "additionalProperties": false,
  "properties": {
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
          "depth": { "type": "integer", "description": "最大递归深度；省略时 output=dir 默认 1、其他模式默认 0；显式值优先；0=不限，1=只直接子项；<0 → failed" },
          "output": {
            "type": "string",
            "enum": ["json", "tree", "list", "dir"],
            "description": "输出形态；缺省 json。dir=ls 便捷别名（缺省 depth=1，输出 items）"
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
          "maxTotalHits": {
            "type": "integer",
            "minimum": 1,
            "description": "grep 跨文件累计命中行预算；缺省 2000，硬上限 20000（超出按上限收敛）；达到上限 → hits_truncated=true"
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
  "results": [
    {
      "root": "C:\\proj",
      "status": "ok",
      "truncated": false,
      "matched_total": 42,
      "hash_truncated": false,
      "limit": 2000,
      "items": [
        { "rel": "cmd", "is_dir": true },
        { "rel": "cmd/main.go", "is_dir": false, "size": 1024, "mtime": "2026-10-20T09:00:00Z", "md5": "…" },
        { "rel": "internal/x.go", "is_dir": false, "size": 2048 }
      ]
    },
    {
      "root": "C:\\logs",
      "status": "ok",
      "truncated": true,
      "hits_truncated": true,
      "scanned": 128,
      "matched_total": 37,
      "hash_truncated": false,
      "limit": 2000,
      "items": [
        { "rel": "app.log", "is_dir": false, "hits": [{ "line": 12, "text": "…" }] }
      ]
    },
    {
      "root": "C:\\proj",
      "status": "ok",
      "truncated": false,
      "matched_total": 42,
      "hash_truncated": false,
      "limit": 2000,
      "lines": ["cmd", "cmd/main.go", "internal/x.go"]
    },
    {
      "root": "C:\\proj",
      "status": "ok",
      "truncated": false,
      "matched_total": 42,
      "hash_truncated": false,
      "limit": 2000,
      "text": "cmd/\n  main.go\ninternal/\n  x.go\n"
    }
  ],
  "summary": { "total": 4, "ok": 4, "failed": 0, "skipped": 0 }
}
```

## 字段说明
- `truncated`：`matched_total > len(items)`（条目被 `limit` 截断）或 grep 命中被 `maxTotalHits` 截断。
- `scanned` / `matched_total`：仅 `grep` 模式；`scanned` = 截至截点已扫描文件数，`matched_total` = 截至截点命中文件数（二者在全局预算触发时均为截点值）。
- `hits_truncated`：仅 `grep` 模式；`true` = 累计命中达到 `maxTotalHits`，后续命中/文件未再扫描。
- `hash_skipped`：`"exceeded_limit"`（超出 `maxHashBytes` 预算）或 `"unreadable"`（读取/哈希失败）。
- `lines`（`list`）/ `text`（`tree`）与 `items` 互斥；三者均按 `limit` 与确定性字典序规则截断。
