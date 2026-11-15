# 基本信息
```yaml
name: session_search
title: 检索历史会话
args: session_search --input={RAW-INPUT-FILE}
async: sync
category: session
hot: true
runtime: plugin
idempotent: true
output: stdout
timeout: 60
touch_files: false
```

# 摘要
在**历史会话的消息正文**里检索（跨会话、可选跨用户）。用于回答"我记得之前说过…"这类回忆：
返回每条命中的 `session_id / turn_id / message_id` 与 `snippet`，可再用
`session_history(session_id=…)` 展开原文。

默认范围由用户设置（`plugins.session.cross-user`）决定：`private` / `llm` 模式只检索**当前用户**，
`public` 模式检索**全项目**（自己的命中优先加权）。`targets` 用于显式点名其他用户
（`private` 下点名他人会返回 `permission_denied`，不会静默变空结果）。`targets: ["*"]` =
本项目全部用户（受 `plugins.session.cross-user.max-users` 上限约束）。

不要用它检索代码或文件内容（那是 `vfts_query`）。命中里的 `self` / `top` 表示该消息属于
你当前所在的会话 / 用户正在看的顶层对话。

# 参数
```json
{
  "type": "object",
  "required": ["query"],
  "properties": {
    "query": {
      "type": "string",
      "description": "检索词（自然语言或关键词；中英文均可）。空格/标点分隔的多个词按 OR 匹配"
    },
    "targets": {
      "type": "array",
      "items": { "type": "string" },
      "description": "可选：额外检索哪些用户（[\"*\"] = 全项目；[\"alice\"] = 指定用户）。省略 = 按服务端模式（private/llm 仅自己；public 全项目，自己优先）"
    },
    "roles": {
      "type": "array",
      "items": { "type": "string", "enum": ["user", "assistant", "tool", "system", "*"] },
      "description": "可选：消息角色过滤；缺省 user+assistant；[\"*\"] = 全部（含工具结果）"
    },
    "session_id": {
      "type": "string",
      "description": "可选：限定某个会话（省略 = 全部会话；\"@top\" = 当前顶层对话）"
    },
    "since": {
      "type": "string",
      "description": "可选：时间下界（RFC3339，如 2026-11-01T00:00:00Z）"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 100,
      "description": "最多返回命中条数（缺省 20，上限 100）"
    },
    "context_runes": {
      "type": "integer",
      "minimum": 20,
      "maximum": 200,
      "description": "可选：snippet 命中位置两侧的字符数（缺省 60）"
    },
    "mode": {
      "type": "string",
      "enum": ["keyword", "semantic", "hybrid"],
      "description": "可选：keyword = 关键词（缺省）；semantic = 向量语义；hybrid = 语义 + 关键词融合。semantic/hybrid 需用户在设置里启用 embedding（semantic_disabled 表示未启用）"
    }
  }
}
```

# 返回值
```json
{
  "hits": [
    {
      "user": "alice",
      "session_id": "s-…",
      "dialog_id": "…",
      "title": "登录重构",
      "turn_id": "t-…",
      "message_id": "M…",
      "role": "user",
      "created_at": "2026-11-01T10:00:00.000000000Z",
      "snippet": "…交付时间定在周五…",
      "score": 3.25,
      "self": true,
      "top": true,
      "cross_user": false
    }
  ],
  "total": 1,
  "scanned_users": 1,
  "scanned_sessions": 3,
  "scanned_messages": 42,
  "truncated": false,
  "partial": false,
  "search_mode": "keyword",
  "approx": false,
  "index": { "state": "ready", "indexed_messages": 120, "total_messages": 120 },
  "elapsed_ms": 12
}
```

`truncated=true` = 命中数超过 `limit`；`partial=true` = 扫描/补索引预算耗尽（只看了部分会话/消息，
或还有消息未建立向量），结论应注明"可能不完整"。`search_mode` 回显实际模式；`indexed` = 本次补写
的向量条数（semantic/hybrid）；`approx=true` = 本次走 HNSW 近似检索（可能漏掉少量低分命中，需要
精确结果时加 `session_id` 过滤或改用 keyword）；`index` = 后台全量索引状态（`indexing` 期间结果可能
不全，`new_vectors` 为本次新增向量数）。
`permission_denied` = 跨用户模式不允许；`unsupported` = 共享数据根形态不支持跨用户读取；
`semantic_disabled` = 未启用 embedding（改用 keyword）；`semantic_unavailable` = embedding 服务不可用。

`mode=semantic` 时 `score` = 余弦相似度（`semantic_score` 同值）；`hybrid` 时 `score` =
0.65×语义 + 0.35×关键词（两项分别以 `semantic_score` / `keyword_score` 返回）。

# 注意
- 结果**只给片段**，不要把 snippet 当成完整原话；引用前用 `session_history` 展开对应轮次。
- 同一条命中可能来自他人：`user` 字段标明来源，回答时必须说清是谁说的。