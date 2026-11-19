# codegraph_callees

```yaml
name: codegraph_callees
title: 被调方查询
async: auto
timeout: 30
touch_files: false
category: codegraph
hot: true
```

# 摘要

查「它调用了谁」：按 id（优先）或 file+name 定位符号，返回其直接调用的目标名（含 resolved、candidates 与解析到的 file/line）。

- 语义为调用点文本的**名字级启发式**：无类型/重载解析；被调名只保留最后一段（`a.b.Foo()` → `Foo`），receiver/qualifier 丢失。
- **关系增强（无类型解析，按语法结构近似）**：每个 callee 附 `owner`/`ownerKind`（`interface`/`trait`/`abstract_class`/`class`/`type`/`impl`/`trait_impl`/`enum`）与 `dispatch`（`static`/`interface`/`trait`/`abstract`/`ambiguous`/`unknown`）；`candidates>1` 时 `candidateSymbols` **列出全部同名定义**；接口/抽象类/trait 目标另给 `possibleImplementations`（同名具体方法，按 owner 非抽象近似，**不解析 implements/extends**，故为“可能实现”）。
- **递归/环**：`self_recursive=true` 表示目标就是源符号自身；`recursive=true` 表示沿名字级调用边（≤8 层、节点上限 2000）能回到源符号（含自递归）。
- `lang` 可限定候选定义语言（多语言仓库同名跨语言时收窄）；返回回声 `lang`。
- owner/dispatch 依赖索引中记录的方法所属类型：**旧索引（升级前建立）可能为空**，此时 `dispatch=unknown`；重建索引（`codegraph_initialize`）后补齐。
- `id` 优先；file+name 定位时 file 为精确路径（绝对/相对、大小写不敏感），name 大小写不敏感。
- 同一 file+name 多命中：`matches` 为总数，`symbol` 为首条，`symbols` 为全部候选，`ambiguous=true`；`callees` 只针对首条目标。
- 每个 callee 的 `candidates` = 索引内同名定义数：`0` = 未索引/外部符号，`1` = `resolved=true` 并回填 file/line/kind/id，`>1` = 同名多处无法解析。
- `calls` 只对 func/method/constructor 生成；class/type/interface/enum/trait 返回 `note=not_callable`。`calls` 已去重并按字典序排序，不是源码调用顺序，也没有调用点行号；嵌套闭包/箭头函数内的调用不计入外层。
- `limit` 缺省 50、上限 500；`offset` 翻页；`total` 是**未截断的真实调用目标数**，`returned` 是本次返回数，`truncated=true` 表示还有下一页。
- 空结果带 `note`：`no_calls`、`not_callable`、`offset_beyond_total`。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "id": {"type": "string", "description": "符号 id（file:line:name:kind）；提供时忽略 file/name"},
    "file": {"type": "string", "description": "符号所在文件（精确匹配，大小写不敏感；配 name 定位）"},
    "name": {"type": "string", "description": "符号名（大小写不敏感全名；配 file 定位）"},
    "lang": {"type": "string", "enum": ["go", "javascript", "typescript", "tsx", "python", "rust", "java"], "description": "候选定义语言过滤（可选）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "返回上限（缺省 50，硬上限 500）"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "排序后跳过条数（翻页）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "matches": 1,
  "ambiguous": false,
  "symbol": { "id": "E:/proj/a.go:10:Main:func", "file": "E:/proj/a.go", "kind": "func", "lang": "go", "name": "Main", "line": 10, "endLine": 20, "complexity": 3, "signature": "func Main()" },
  "symbols": [ { "id": "E:/proj/a.go:10:Main:func", "file": "E:/proj/a.go", "kind": "func", "lang": "go", "name": "Main", "line": 10, "endLine": 20, "complexity": 3, "signature": "func Main()" } ],
  "total": 3,
  "returned": 2,
  "limit": 50,
  "offset": 0,
  "truncated": false,
  "callees": [
    { "name": "Run", "resolved": false, "candidates": 4, "dispatch": "interface",
      "candidateSymbols": [ { "id": "E:/proj/i.go:3:Run:method", "file": "E:/proj/i.go", "kind": "method", "name": "Run", "line": 3, "owner": "Runner", "ownerKind": "interface" },
                           { "id": "E:/proj/t.go:7:Run:method", "file": "E:/proj/t.go", "kind": "method", "name": "Run", "line": 7, "owner": "Impl", "ownerKind": "type" } ],
      "possibleImplementations": [ { "id": "E:/proj/t.go:7:Run:method", "file": "E:/proj/t.go", "kind": "method", "name": "Run", "line": 7, "owner": "Impl", "ownerKind": "type" } ] },
    { "name": "Helper", "resolved": true, "candidates": 1, "kind": "func", "file": "E:/proj/b.go", "line": 3, "id": "E:/proj/b.go:3:Helper:func" }
  ]
}
```

无调用/非函数示例：`{ "workdir":"E:/proj", "matches":1, "symbol":{…}, "symbols":[…], "total":0, "returned":0, "callees":[], "note":"no_calls" }`（非函数为 `not_callable`）。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/缺定位参数/limit·offset 越界）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。