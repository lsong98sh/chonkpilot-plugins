# codegraph_get_module_summary

```yaml
name: codegraph_get_module_summary
title: 模块摘要
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

目录/项目摘要：文件数、语言分布、符号数、最高复杂度函数（全部基于索引统计）。

- `path` 为目录前缀，按**路径段边界**且大小写不敏感匹配（`src` 命中 `src/a.go`，不命中 `srcfoo/a.go`）；可传相对或 workdir 内绝对路径；省略时统计整个项目，`scope` 返回 workdir。
- `files` = 范围内**已索引文件数**（≤8MB、受 exts/skip/gitignore 限制；不包含未索引/不支持语言/被排除文件，也不是仓库文件总数）。
- `symbols` = 范围内全部符号数（含 func/method/class/type/interface/enum/trait/constructor，不只是函数）。
- `topComplexity5` 只含 **func/method/constructor**，按复杂度降序 → 文件 → 行号 → 名称 → kind 稳定排序，最多 5 条。
- `langCounts` 以语言名为键（`go/javascript/typescript/tsx/python/rust/java`，不是扩展名；`tsx` 单列）。
- `parseErrorFiles` = 范围内语法解析有错（`HasErr`）但仍入索引的文件数；`lastIndexedAt` = 最近索引时间（Unix 毫秒，0=未知）。
- 返回体较小，适合先了解项目结构，再深入具体符号或依赖。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "path": {"type": "string", "description": "目录前缀（相对/绝对，大小写不敏感；空=全项目）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "scope": "E:/proj/src",
  "files": 12,
  "symbols": 87,
  "langCounts": {"go": 10, "typescript": 2},
  "topComplexity5": [
    { "id": "E:/proj/src/a.go:10:Run:func", "file": "E:/proj/src/a.go", "kind": "func", "lang": "go", "name": "Run", "line": 10, "endLine": 40, "complexity": 9, "signature": "func Run()", "calls": ["Helper"] }
  ],
  "parseErrorFiles": 0,
  "lastIndexedAt": 1762483200000
}
```

范围内无文件：`files:0, symbols:0, langCounts:{}, topComplexity5:[], note:"path_not_indexed"`（仅 path 非空时）。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。