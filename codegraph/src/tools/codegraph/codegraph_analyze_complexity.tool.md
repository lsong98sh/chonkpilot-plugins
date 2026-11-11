# codegraph_analyze_complexity

```yaml
name: codegraph_analyze_complexity
title: 圈复杂度分析
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

圈复杂度排名：默认只返回 **func/method/constructor** 中复杂度最高的 `topN` 个符号。

- 复杂度口径 = `1 + 决策节点数`（分支/循环/switch case 等）；**不计 `&&`/`||`**，`switch`/`default` 的 case 各计 1。嵌套函数/闭包内的决策点**不计入外层**（与调用边收集同口径），但口径仍与 gocyclo 不同，用于**相对排序**而非精确对齐。
- `kind` 默认 `function`（= func/method/constructor）；可传 `func`/`method`/`constructor` 单类，或 `all` 查看全部符号（type/class 等复杂度恒为 1）。
- `file` 为**大小写不敏感的子串匹配**（不是精确路径），例如 `a.go` 也会命中 `a_test.go`。
- `topN` 缺省 20、硬上限 200；`minCc` 为复杂度下限（`>=`，缺省 1）。
- 结果按复杂度降序，并以文件/行号/名称/kind 稳定排序；`total` 是过滤后的真实匹配数，`truncated=true` 表示 `topN` 截断。
- `coverage.indexed_files`/`parse_error_files` 说明本次结果基于多少已索引文件、其中多少有语法解析错误；>8MB/不支持/被排除的文件不在索引中，仍无法从结果判断。
- 仅覆盖已索引文件；不支持的语言/被排除的文件不在结果中。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "file": {"type": "string", "description": "文件名/路径子串过滤（大小写不敏感）"},
    "kind": {"type": "string", "enum": ["function", "func", "method", "constructor", "all"], "default": "function", "description": "符号类别；function=func/method/constructor，all=全部"},
    "topN": {"type": "integer", "minimum": 1, "maximum": 200, "default": 20, "description": "返回前 N 个（缺省 20，硬上限 200）"},
    "minCc": {"type": "integer", "minimum": 1, "default": 1, "description": "最低复杂度（>=，缺省 1）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "topN": 20,
  "returned": 2,
  "total": 2,
  "truncated": false,
  "minCc": 5,
  "kind": "function",
  "metric": "cyclomatic",
  "symbols": [
    { "id": "E:/proj/a.go:10:Run:func", "file": "E:/proj/a.go", "kind": "func", "lang": "go", "name": "Run", "line": 10, "endLine": 40, "complexity": 9, "signature": "func Run()", "calls": ["Helper"] }
  ],
  "coverage": {"indexed_files": 42, "parse_error_files": 0}
}
```

空结果：`returned:0, total:0, symbols:[], note:"no_matches"`（`file` 未命中时为 `note:"file_not_indexed"`）。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/topN·minCc 越界/kind 非法）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。