---
plugin: codegraph
tool: vfts_query
contract: src/tools/codegraph/vfts_query.tool.md
handler: src/vfts_query.go:169（vftsQuery）→ src/engines/vfts/server/server.go:546（toolQuery）/ :592（toolSessionQuery）
reviewed_at: 2026-11-10
status: verified
---

# vfts_query 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 topK 无上限、未就绪文案指向不可见 `vfts_index`、scope 非法静默降级、match/query 语义未定义、会话截断顺序错误、path 静默忽略、total 仅返回数、topK 字符串静默回落等问题已修复。

## 1. 一句话结论

可用。插件层严格校验参数（scope/role 归一化、match/query 互斥、topK 1..100），引擎返回 `scope/total/returned/truncated`；会话命中改为**先按相关度取 topK 再按 ts 升序展示**。未就绪文案不再指向不可见工具；`session_filter_applied` 暴露服务端会话过滤是否生效，`score_kind`/`line_kind` 说明原始分数与行号口径。剩余风险集中在双注册维护性。

## 2. 调用链

1. LLM `vfts_query` → gateway（注入 `_meta.chonkpilot`）→ 插件 MCP。
2. `registerVftsQuery` 闭包 → `cfg.vftsQuery`：
   - `validateVftsQueryArgs`（未知键/类型/scope/role/match·query/topK）→
   - workdir：`ResolveWorkdirFromMeta`（args 显式 → meta → 已登记实例 → 唯一工作区）；
   - session_id：args → meta.session → meta.top_session；
   - 透传 scope/match/query/topK/path/role → `m.engine`。
3. 引擎 `toolQuery`：scope 校验（file/session）→ file 走 `readyGate` + `QueryPage`；session 走 `needWSReadOnly` + `QuerySessionsPage`。
4. 参数/查询错误 `isError=true` 文本；门禁/会话未初始化 `isError=false` 的 `status` 对象。

## 3. 契约面

- 契约源 = `vfts_query.tool.md`；schema/description/`_meta` 均来自 md（插件注册时使用）。
- schema：`additionalProperties:false`；`scope` enum file/session default file；`role` enum user/assistant；`topK` 1..100 default 20。
- 摘要：match/query 互斥且至少其一、topK 上限、`total`/`returned`/`truncated` 语义（file 为候选块内唯一文件数，候选块上限 500）、session_id 回退链、会话先相关度后 ts、`path` 仅 file、`path_ignored`、空结果 note/hint、workdir 规则。
- 返回示例（file/session）、未初始化、门禁与参数错误。

## 4. 实现面

- **插件校验**：`validateVftsQueryArgs` 归一化 scope/role（小写）、trim 字符串；未知键/类型错误；match 与 query 互斥；topK 缺省 20、<1 报错、>100 截到 100；返回规范化 args。
- **file**：`toolQuery` 校验 scope；`readyGate`（只读）；`QueryPage`：
  - `rawTopK = topK*5`，上限 500；文件级去重；
  - `matched` = 候选内唯一文件数；`truncated = matched > returned` 或候选块已达 500（保守值）；
  - path 为大小写不敏感子串；结果拼绝对路径；
  - 响应带 `score_kind:"zvec_fts_raw"`、`line_kind:"chunk_start_plus_offset"`。
- **session**：`QuerySessionsPage`：
  - `rawTopK = topK*20`，上限 1000；本地 session_id/role 过滤；
  - `matched` = 过滤后条数；先按相关度保留 topK，**再按 ts 升序**展示；`truncated = matched > returned` 或候选达到 1000（保守值）；
  - `session_filter_applied`：服务端 session filter 是否生效；false 时附 `hint`（已回退为全量候选 + 本地过滤，可能少召回）；
  - `path` 非空时返回 `path_ignored:true`；
  - 空结果 `note:"no_hits"` + `hint`（换关键词/缩小范围/子会话可能未索引）。
- **会话未初始化**：结构化 `status=not_initialized`（`isError=false`），含 `scope/topK/returned/total/truncated/hits:[]`。
- **readiness 文案**：不再出现 `vfts_index`，改为等待自动建索引或到「代码知识库」设置页开启。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| file 命中 | false | `{workdir,scope:"file",topK,returned,total,truncated,score_kind,line_kind,hits:[{path,line,snippet,score,loc?}]}` |
| file 空 | false | 同上 + `returned:0,total:0,note:"no_hits"` |
| session 命中 | false | `{workdir,scope:"session",topK,returned,total,truncated,score_kind,session_filter_applied,hits:[{session_id,dialog_id?,turn_id?,message_id,role,ts,snippet,score}],path_ignored?,hint?}` |
| session 空 | false | 同上 + `note:"no_hits",hint` |
| session 未初始化 | false | `{status:"not_initialized",state:"not_initialized",message,scope:"session",hits:[]}` |
| 文件索引未初始化/构建中/错误 | false | `{status:"not_initialized|pending|error",state,message}` |
| 参数非法/无法确定 workdir | **true** | 错误文本 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | 双注册点维护性 | 插件用 `RegisterManagementTools` + 自注册；引擎 `RegisterTools` 仍含同名 `vfts_query`。当前不会重复注册，但将来误用会静默替换 schema。 |
| P3 | 会话 filter 回退可能少召回 | `session_filter_applied=false` 已显式暴露并附 hint；根因是 zvec 服务端 filter 能力，删掉回退会降低兼容性。 |
| P3 | `score` 原始相似度、`line` 块偏移 | 已用 `score_kind`/`line_kind` 显式标注；无归一化/阈值属 zvec 口径。 |
| P3 | `total` 是候选上限内统计 | file 为 500 块内唯一文件数、session 为 1000 候选内过滤数；不是全库精确总数；`truncated` 在候选达上限时可能为保守 true。契约已声明。 |
| P3 | 子会话消息可能未索引 | 空结果已带 hint；若要精确原因需引擎暴露会话清单。 |
| P3 | handler 忽略 ctx | 取消后仍会跑完当前查询；topK 上限限制了输出与耗时。 |
| P3 | 门禁状态 `isError=false` | 契约按 `status` 处理；跨查询工具统一取舍。 |
| P3 | `enabled=false` 不阻止查询 | 与配置页文案不一致；跨工具统一问题。 |

已修（2026-11-10）：topK 无上限、未就绪文案指向不可见 `vfts_index`、scope 非法静默降级、match/query 语义未定义、会话先 ts 排序再截断（丢高分晚消息）、path 在 session 静默忽略、`total` 仅返回数、topK 字符串/小数静默回落、缺 scope/truncated、空会话结果无提示、filter 回退不可见（已加 `session_filter_applied`）、score/line 口径未声明（已加 `score_kind`/`line_kind`）。

## 7. 测试/验收

- `TestValidateVftsQueryArgs`（插件）：默认 scope/topK、session 归一化、topK 上限 100、未知键/都缺/同给/非法 scope/role/topK 0/小数/字符串/类型错误。
- `TestQueryPageMatchedAndTruncated`（引擎）：topK=2 时 matched=3/truncated、按相关度取前 2；topK=10 全量不截断。
- `TestQuerySessionsPageMatchedAndTruncated`：topK=1 时 matched=3/truncated；topK=20 全量；展示按 ts 升序。
- 既有 Query/QuerySessions 全部回归（相关度、去重、path、会话过滤/role/删除/清理）。
- 回归：vfts `go test ./...`、插件 `go test ./...` 通过。