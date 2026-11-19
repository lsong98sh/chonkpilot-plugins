# codegraph_status

```yaml
name: codegraph_status
title: 索引状态与进度
async: auto
timeout: 30
touch_files: false
category: codegraph
hot: true
```

# 摘要

查询当前项目的 codegraph 索引状态与规模（只读）：`state`（空串 = 未初始化 / `indexing` / `ready` / `error`）、`enabled`、构建进度（`progressDone`/`progressTotal`）、已索引文件数 `indexedFiles` 与符号数 `indexedSymbols`、最近索引时间 `lastIndexedAt`（Unix 秒）、错误信息 `err`（若有）。

- 用于查询工具返回 `status=not_initialized|pending|error` 后判断“是否还在建、是否出错、规模多大”。
- **不触发建索引**；需要初始化/重建请由用户或管理面执行 `codegraph_initialize`。
- `workdir` 缺省取调用上下文的 `_meta.chonkpilot.work_dir`；显式传入时必须是当前项目内绝对路径。

# 参数

```json
{
  "type": "object",
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"}
  }
}
```

# 返回值

```json
{
  "workdir": "E:/proj",
  "state": "ready",
  "enabled": true,
  "indexedFiles": 128,
  "indexedSymbols": 2048,
  "progressDone": 128,
  "progressTotal": 128,
  "lastIndexedAt": 1762827600,
  "loaded": true
}
```

`state=indexing` 时 `progressDone/progressTotal` 为当前进度；`state=error` 时附 `err`；未初始化时 `state=""`（无索引规模）。
