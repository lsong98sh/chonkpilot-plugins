# codegraph_symbol_search

```yaml
name: codegraph_symbol_search
title: 符号搜索
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

在已索引的项目内按符号名搜索（子串匹配、忽略大小写），可选 kind 与文件过滤。

- kind 为**精确类别**（func / method / class / type / interface / enum / trait / constructor），大小写不敏感但拼写须一致；非法类别直接报错。Go 的 interface 记作 type。
- file 为路径子串过滤（相对或绝对路径任一命中即可）。
- 索引只覆盖 go / javascript / typescript / tsx / python / rust / java；其他语言文件永远不会出现在结果中。
- `limit` 缺省 50、上限 500；`offset` 用于翻页。结果先按 `file/line/name` 排序再切片。
- `total` 是**未截断的真实命中总数**；`returned` 是本次返回条数，`truncated=true` 表示还有下一页。
- 空结果会带 `note`：`no_match`（无匹配）、`file_not_indexed`（file 过滤未命中已索引文件 / 语言不支持）、`index_empty`（索引为空）、`offset_beyond_total`（翻页越界）。
- 索引由系统自动构建/维护；返回 `status=not_initialized|pending|error` 时请按 `message` 处理，不要反复猜测路径。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "query": {"type": "string", "description": "符号名关键字；省略 = 不按名字过滤（返回排序后的符号）"},
    "kind": {"type": "string", "description": "符号类别过滤（大小写不敏感）：func / method / class / type / interface / enum / trait / constructor"},
    "file": {"type": "string", "description": "文件路径子串过滤"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "返回条数上限（缺省 50，硬上限 500）"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "排序后跳过的条数（翻页）"}
  }
}
```

# 返回值

```json
{
  "total": 12,
  "returned": 2,
  "limit": 50,
  "offset": 0,
  "truncated": false,
  "indexed_files": 37,
  "symbols": [
    { "id": "E:/proj/a.go:3:Helper:func", "file": "E:/proj/a.go", "kind": "func", "lang": "go",
      "name": "Helper", "line": 3, "endLine": 8, "complexity": 4,
      "signature": "func Helper(x int) error", "calls": ["Run"] }
  ]
}
```

空结果示例：`{ "total":0, "returned":0, "limit":50, "offset":0, "truncated":false, "indexed_files":0, "symbols":[], "note":"index_empty" }`。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`，按 `status/message` 处理）。参数非法（未知键、类型错误、非法 kind、limit/offset 越界）时 `isError=true`，正文为错误文本；`workdir` 缺失/越界同样按调用错误处理。