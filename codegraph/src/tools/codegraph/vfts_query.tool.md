# vfts_query

```yaml
name: vfts_query
title: 全文检索
async: auto
timeout: 30
on_timeout: background
category: codegraph
hot: true
```

# 摘要

在 vfts 全文索引中检索：`match` 为自然语言匹配串，`query` 为布尔/高级表达式（**二者互斥，至少给其一**；query 不支持字段前缀）。

- `scope=file`（默认）检索文件：返回命中文件 + 片段/行号，按相关度；`path` 为大小写不敏感的子串过滤。
- `scope=session` 检索**会话正文索引**：按相关度取 `topK` 条，再按 `ts` 升序展示（便于会话内上下文）；`role` 可过滤 user/assistant。
- `topK` 缺省 20、硬上限 100（file=文件数，session=消息条数）；`total` 是截断前的匹配数（file 为候选块内唯一文件数，候选块硬上限 500；session 为本地过滤后条数），`truncated=true` 表示还有更多（候选达到上限时也可能为 true，属保守值）。
- `scope=session` 时 `session_id` 缺省：args → 上下文 `session` → `top_session`；都为空则查全部会话。子会话消息可能未建立会话索引，空结果会带提示。`session_filter_applied=false` 表示服务端 session_id 过滤不可用、已回退为全量候选内本地过滤（可能少召回）。
- `scope=session` 时 `path` 被忽略，并返回 `path_ignored:true`。
- 空结果 `note:"no_hits"`；file 无命中不额外解释，session 附 `hint`（换关键词/缩小范围/子会话可能未索引）。
- `score_kind:"zvec_fts_raw"` 表示分数为 zvec 原始相似度（未归一化）；file 的 `line_kind:"chunk_start_plus_offset"` 表示行号是块起始行加命中偏移，可能不是精确行。
- workdir 缺省取调用上下文的 `_meta.chonkpilot.work_dir`（或已登记实例工作区）；显式传入时必须是当前项目内的绝对路径。

# 参数

```json
{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "workdir": {"type": "string", "description": "项目根目录（可选；缺省取 _meta.chonkpilot.work_dir）"},
    "scope": {"type": "string", "enum": ["file", "session"], "default": "file", "description": "检索范围（默认 file）"},
    "session_id": {"type": "string", "description": "scope=session 时按会话过滤（缺省 = 上下文会话，再回退全部会话）"},
    "role": {"type": "string", "enum": ["user", "assistant"], "description": "scope=session 时按角色过滤（缺省 = 两者）"},
    "match": {"type": "string", "description": "自然语言匹配串（与 query 互斥）"},
    "query": {"type": "string", "description": "布尔/高级表达式（与 match 互斥；不支持 content: 字段前缀）"},
    "topK": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "返回上限（缺省 20，硬上限 100）"},
    "path": {"type": "string", "description": "文件路径子串过滤（仅 scope=file）"}
  }
}
```

# 返回值

file：

```json
{
  "workdir": "E:/proj",
  "scope": "file",
  "topK": 20,
  "returned": 2,
  "total": 7,
  "truncated": true,
  "score_kind": "zvec_fts_raw",
  "line_kind": "chunk_start_plus_offset",
  "hits": [
    { "path": "E:/proj/docs/a.md", "line": 12, "snippet": "…命中词前后约 240 字符…", "score": 0.87, "loc": "p3" }
  ]
}
```

session：

```json
{
  "workdir": "E:/proj",
  "scope": "session",
  "topK": 20,
  "returned": 2,
  "total": 2,
  "truncated": false,
  "score_kind": "zvec_fts_raw",
  "session_filter_applied": true,
  "hits": [
    { "session_id": "s1", "turn_id": "t1", "message_id": "m1", "role": "user", "ts": 1762483200000, "snippet": "…", "score": 0.91 }
  ]
}
```

空结果：`returned:0,total:0,hits:[],note:"no_hits"`（session 另附 `hint`）；session 索引未建立：`{status:"not_initialized",state:"not_initialized",message:...,scope:"session",hits:[]}`（`isError=false`）。

索引门禁：`{ "status":"not_initialized" | "pending" | "error", "state":"…", "message":"…" }`（`isError=false`）。参数非法（未知键/类型错误/scope·role 非法/match 与 query 同给/topK 越界）时 `isError=true`；无法确定 workdir 同样按调用错误处理。