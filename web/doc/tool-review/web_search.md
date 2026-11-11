---
plugin: web
tool: web_search
contract: src/tools/web/web_search.tool.md
handler: web/src/main.go:193（search）
reviewed_at: 2026-11-10
status: verified
---

# web_search 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的正文无预算、count 无 schema 边界/回显、8 MiB 静默截断、最坏耗时超过 180s、空结果无提示、解析失败不记状态等问题已修复。

## 1. 一句话结论

可用。`need_content=true` 时正文按 `max_content_bytes` 总预算（默认 256KiB、上限 8MiB）逐条截断并置 `content_truncated`；`count` 有 schema 边界并回显 `count_requested`；上游响应 >8MiB 明确报错；单次尝试超时上限 55s（3 次 + 退避 ≤168s，落在宿主 180s 内）。剩余主要问题是 `localization` 的上游语义未定义。

## 2. 调用链

1. LLM `web_search` → gateway → 插件 MCP（契约 `src/tools/web/web_search.tool.md`，`_meta.timeout=180`）。
2. `searcher.search`：query 校验 → Key 校验 → count/预算归一 → `doAPI`（Bearer + 重试）→ 解析 `results`/`data`/`web_search`。
3. 正文预算截断 → 返回 `{provider,results,count,count_requested,content_bytes,content_truncated[,note,hint]}`。

## 3. 契约面

- 契约源 = `web_search.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`query` required 1..128；`count` 1..50 default 10；`need_content` default false；`max_content_bytes` 1..8388608 default 262144；`additionalProperties:false`。
- 摘要：默认 10/最多 50、正文预算与截断标记、8MiB 明确报错、空结果 note/hint、未配置 Key 直接报错、单次尝试 55s 上限。
- 返回示例含 `count_requested`/`content_bytes`/`content_truncated`。

## 4. 实现面

- **参数**：query trim 后非空且 ≤128 rune；count 参数 >0 覆盖配置，clamp 1..50；`count_requested` 保留覆盖后的请求值；`max_content_bytes` >0 覆盖默认预算，>8MiB 截到 8MiB。
- **预算**：`applySearchContentBudget` 按条目顺序消耗总预算；超出部分用 `truncateUTF8` 截断并置 `content_truncated`；预算耗尽后剩余条目 `content=""` 且同样标记。`snippet/summary` 不受影响。
- **doAPI**：单次尝试超时 = min(cfg.TimeoutSec, 55s)；重试 3 次（1s/2s 退避）；8MiB 读取上限超限明确报错并提示降低 count/关闭 need_content/改用 web_download；401/402/403/404/400 不重试。
- **解析**：兼容 `results`/`data`/`web_search`；空为 `[]`；解析失败先 `record` 再返回错误。
- **空结果**：`note:"no_results"` + `hint`（放宽/改写 query）。
- **状态**：成功/失败都会更新 `last_at/last_count/last_error`。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 有结果 | false | `{provider:"deepseek",results:[…],count,count_requested,content_bytes,content_truncated}` |
| 空结果 | false | 同上 + `results:[],count:0,note:"no_results",hint` |
| Key 未配置 | true | 可执行文案（插件 → 联网能力） |
| 参数/上游错误/超时 | true | 中文错误或上游状态文案；8MiB 超限明确提示 |
| 门禁/异步 | — | gateway 180s 转后台 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | `localization` 语义未定义 | 契约写“原样转发给上游”，未给格式/示例；模型可能传 `zh-CN`/`CN` 被上游 400 或静默无效。需上游文档确认后改枚举/示例，或删除。 |
| P3 | `score=0` 与“无分”不可区分 | `omitempty` 会丢掉 0 值；影响很小。 |
| P3 | 网络错误仍透出 Go/上游英文原文 | 已带“可稍后重试/改用 web_download”建议，但原文保留。 |
| P3 | 严格参数校验仍靠 schema | 运行时未拒绝未知键/类型错误（SDK 宽松取值）；不影响主路径。 |

已修（2026-11-10）：正文无预算、count 无 schema 边界/回显、8MiB 静默截断、最坏耗时 >180s、空结果无提示、解析失败不记状态、description 未写默认值/体积风险。

## 7. 测试/验收

- `TestApplySearchContentBudget`：总预算按条截断、`content_truncated` 标记、预算耗尽清空、无正文不计。
- `TestAPIAttemptTimeoutUnderGatewayLimit`：3 次尝试 + 退避 <180s。
- 既有 `TestEmbeddedToolContracts`：契约/策略/schema（count 边界）。
- 回归：`go test ./...`（web 插件）通过。