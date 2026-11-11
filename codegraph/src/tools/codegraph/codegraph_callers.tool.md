# codegraph_callers

```yaml
name: codegraph_callers
title: 调用方查询
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

查「谁调用了它」：在已索引项目内按被调名（精确名或最后一段，忽略大小写，故 pkg.Foo 与 Foo 同目标）找调用方符号。

- 语义为调用点文本的**名字级启发式**：无类型/重载解析，只覆盖已索引文件中的直接调用；**跨语言同名会被一并计入**（可用 file 缩小范围）。
- 目标由 `id` 或 `name` 指定；**id 优先**——同时给出时按 id 定位，name 被忽略。id 未命中返回 `status=not_found`（`note=id_not_found`）。
- name 路径不要求目标已定义（可能是外部/动态符号）：返回 `target_defined:false`、`target_matches:0`；若同时无调用方，`note=target_not_indexed`。
- `callers[]` 是**调用方符号定义**；`line` 是定义起始行，不是调用点行。返回 `target_symbol`（首个定义）与 `target_matches/target_ambiguous`，便于确认问的是哪个同名符号。
- file 只过滤调用方所在文件（路径子串、大小写不敏感）。
- `limit` 缺省 50、上限 500；`offset` 翻页；`total` 是**未截断的真实调用方总数**，`returned` 是本次返回数，`truncated=true` 表示还有下一页。
- 空调用方带 `note`：`no_callers`、`target_not_indexed`、`offset_beyond_total`。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "name": {"type": "string", "description": "被调名（如 Foo 或 pkg.Foo）；id 非空时忽略"},
    "id": {"type": "string", "description": "目标符号 id（file:line:name:kind）；与 name 二选一，id 优先"},
    "file": {"type": "string", "description": "只保留该文件内的调用方（路径子串，大小写不敏感）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "返回上限（缺省 50，硬上限 500）"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "排序后跳过条数（翻页）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "target": "Run",
  "target_defined": true,
  "target_matches": 2,
  "target_ambiguous": true,
  "target_symbol": { "id": "E:/proj/a.go:10:Run:func", "file": "E:/proj/a.go", "kind": "func", "lang": "go", "name": "Run", "line": 10, "endLine": 18, "complexity": 3, "signature": "func Run() error" },
  "total": 7,
  "returned": 2,
  "limit": 50,
  "offset": 0,
  "truncated": false,
  "callers": [
    { "id": "E:/proj/b.go:3:Main:func", "file": "E:/proj/b.go", "kind": "func", "lang": "go", "name": "Main", "line": 3, "endLine": 9, "complexity": 4, "signature": "func Main()", "calls": ["Run"] }
  ]
}
```

空调用方示例：`{ "workdir":"E:/proj", "target":"Foo", "target_defined":true, "target_matches":1, "total":0, "returned":0, "callers":[], "note":"no_callers" }`。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/name 与 id 都缺失/limit·offset 越界）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。