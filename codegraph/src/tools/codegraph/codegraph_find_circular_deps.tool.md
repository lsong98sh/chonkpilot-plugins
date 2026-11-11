# codegraph_find_circular_deps

```yaml
name: codegraph_find_circular_deps
title: 循环依赖检测
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

在可解析为仓库内文件的导入边上枚举**简单环**（JS/TS 相对路径、Go module 前缀、Py/Java 包路径、Rust crate 尽力而为）。

- 路径为 **workdir 相对路径**，闭环在末尾重复起点（如 `["a.go","b.go","a.go"]`）；自环为 `["a.go","a.go"]`。
- 解析不到仓库内文件的导入不参与成环判断；解析是启发式（无类型/别名/包解析），**0 环不代表绝对没有循环依赖**。
- 同一环只报一次（从字典序最小节点出发枚举，不发生旋转重复）；环数潜在指数级，因此提供 `limit/offset` 分页，`truncated=true` 表示还有下一页。
- `cycleCount`/`returned` 是本页数量；不返回 `total`（完整计数可能不可行）。`include_self=true`（默认）包含文件 import 自己的自环。
- 仅覆盖已索引文件；原始导入串请用 `codegraph_get_dependency_graph` 查看。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 50, "description": "本页最多返回环数（缺省 50，硬上限 500）"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "按确定性枚举顺序跳过条数（翻页）"},
    "include_self": {"type": "boolean", "default": true, "description": "是否包含自环（文件 import 自己）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "cycleCount": 2,
  "returned": 2,
  "limit": 50,
  "offset": 0,
  "truncated": false,
  "cycles": [
    ["a.go", "b.go", "a.go"],
    ["pkg/x.go", "pkg/x.go"]
  ],
  "hint": "路径为 workdir 相对路径；只含可解析为仓库内文件的导入边（解析为启发式，0 环不代表绝对没有循环依赖）；原始导入见 codegraph_get_dependency_graph。环数潜在指数级，用 limit/offset 翻页。"
}
```

空结果：`cycleCount:0, returned:0, cycles:[], note:"no_cycles"`；翻页越界为 `note:"offset_beyond_total"`。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/limit·offset 越界）时 `isError=true`；workdir 缺失/越界同样按调用错误处理。