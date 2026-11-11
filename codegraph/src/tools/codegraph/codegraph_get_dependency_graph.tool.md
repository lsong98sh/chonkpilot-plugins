# codegraph_get_dependency_graph

```yaml
name: codegraph_get_dependency_graph
title: 文件依赖图
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

返回文件级依赖：文件 → 静态导入原始串（import / use；未解析为项目内路径）。

- 默认**只返回有导入的文件**（`include_empty=false`），并按文件绝对路径排序；`total` 是符合条件的真实文件数，`returned` 是本页数量，`truncated=true` 表示还有下一页。
- `file` 可传 workdir 相对路径或 workdir 内绝对路径（大小写不敏感）；也接受路径段后缀（`main.go` 命中 `src/main.go`）。命中多个文件时全部返回并置 `ambiguous=true`。
- `limit` 缺省 50、上限 500；`offset` 翻页。`edges` 是本页导入条数，`totalEdges` 是全部符合条件的导入条数。
- `deps` 键为文件绝对路径，值为原始导入串（按源码出现顺序，未去重）。原始串可能是外部模块、相对路径或包名；能否解析为仓库内文件见 `codegraph_find_circular_deps`。
- 仅覆盖**已索引文件**的**静态**导入：JS/TS 的 `require()` / 动态 `import()` 不提取；Go 的分组 `import ( ... )` 已支持；Rust `pub use` 已支持。
- 空结果带 `note`：`file_not_indexed`（file 未命中）、`no_dependencies`（有已索引文件但无导入）、`no_indexed_files`（索引为空）、`offset_beyond_total`。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "file": {"type": "string", "description": "只返回该文件的依赖（相对/绝对路径，大小写不敏感；也接受路径段后缀）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "返回文件上限（缺省 50，硬上限 500）"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "按文件路径排序后跳过条数（翻页）"},
    "include_empty": {"type": "boolean", "default": false, "description": "是否包含没有导入的文件（默认 false，节省返回体积）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "total": 12,
  "returned": 2,
  "files": 2,
  "limit": 50,
  "offset": 0,
  "truncated": false,
  "edges": 5,
  "totalEdges": 31,
  "deps": {
    "E:/proj/a.go": ["fmt", "os"],
    "E:/proj/b.go": ["example.com/x/y"]
  }
}
```

单文件过滤命中：额外返回 `"file":"src/a.go","file_matched":true`；命中多个时 `"ambiguous":true`。

空结果示例：

- `{ "workdir":"E:/proj", "total":0, "returned":0, "files":0, "deps":{}, "note":"file_not_indexed" }`（file 未命中；文件可能未索引/被排除/语言不支持）
- `{ "workdir":"E:/proj", "total":0, ..., "note":"no_dependencies" }`（有已索引文件，但都没有导入；或 file 命中但无导入）
- `{ "workdir":"E:/proj", "total":0, ..., "note":"no_indexed_files" }`（`include_empty=true` 但索引为空）
- 翻页越界：`returned:0, note:"offset_beyond_total"`

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/limit·offset 越界）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。