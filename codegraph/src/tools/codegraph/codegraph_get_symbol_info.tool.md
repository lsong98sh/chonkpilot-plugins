# codegraph_get_symbol_info

```yaml
name: codegraph_get_symbol_info
title: 符号详情
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

查询单个符号的详情（id 或 file+name 定位）：kind、文件、起止行号、复杂度、签名与调用列表。

- id 来自 `codegraph_symbol_search`（file:line:name:kind）。**id 非空时只按 id 匹配**，file/name 被忽略。
- 未传 id 时必须同时提供 `file` + `name`：file 为精确路径（绝对/相对均可，大小写不敏感），name 为大小写不敏感的符号全名。
- 同一 file+name 可能命中多个符号（重载/同名）：`matches` 为总数，`symbol` 为首条，`symbols` 为全部候选，`ambiguous=true`。
- 未命中返回 `status=not_found`，并带 `note`：`file_not_indexed`（文件未索引/语言不支持）、`id_not_found`、`name_not_found`。
- `symbol.calls` 只对 func/method/constructor 存在；`signature` 是源码行签名（可能截断到 200 字符）。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "id": {"type": "string", "description": "symbol_search 返回的 id（file:line:name:kind）；提供时忽略 file/name"},
    "file": {"type": "string", "description": "文件路径（精确匹配，大小写不敏感；绝对或相对）"},
    "name": {"type": "string", "description": "符号名（大小写不敏感全名）"}
  }
}
```

# 返回值

```json
{
  "matches": 2,
  "ambiguous": true,
  "workdir": "E:/proj",
  "symbol": {
    "id": "E:/proj/pkg/helper.go:3:Helper:func",
    "file": "E:/proj/pkg/helper.go",
    "kind": "func", "lang": "go", "name": "Helper",
    "line": 3, "endLine": 8, "complexity": 4,
    "signature": "func Helper(x int) error", "calls": ["Run"]
  },
  "symbols": [
    { "id": "E:/proj/pkg/helper.go:3:Helper:func", "file": "E:/proj/pkg/helper.go", "kind": "func", "lang": "go", "name": "Helper", "line": 3, "endLine": 8, "complexity": 4, "signature": "func Helper(x int) error", "calls": ["Run"] },
    { "id": "E:/proj/pkg/helper.go:10:Helper:method", "file": "E:/proj/pkg/helper.go", "kind": "method", "lang": "go", "name": "Helper", "line": 10, "endLine": 14, "complexity": 7, "signature": "func (s *Svc) Helper()" }
  ]
}
```

未命中示例：`{ "status":"not_found", "note":"file_not_indexed", "workdir":"E:/proj", "message":"…" }`（`isError=false`，按 `status/note` 处理）。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/缺少 id 且 file+name 不全）时 `isError=true`，正文为错误文本；workdir 缺失/越界同样按调用错误处理。