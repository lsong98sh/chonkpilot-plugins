---
plugin: web
tool: web_fetch
contract: src/tools/web/web_fetch.tool.md
handler: web/src/fetch.go:21（fetch）
reviewed_at: 2026-11-10
status: verified
---

# web_fetch 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的正文无上限/无截断标记、8 MiB 静默截断、无体积参数、URL 字符/字节歧义、空正文无提示、解析失败不记状态等问题已修复。

## 1. 一句话结论

可用。`maxBytes`（默认 4MiB、硬上限 8MiB）控制正文并返回 `truncated`；上游响应 >8MiB 明确报错并建议 web_download；空正文带 `note/hint`；单次尝试超时上限 55s（总重试 ≤168s，落在宿主 180s 内）。

## 2. 调用链

1. LLM `web_fetch` → gateway → 插件 MCP（契约 `web_fetch.tool.md`，`_meta.timeout=180`）。
2. `searcher.fetch`：URL 校验（绝对值/host/scheme/userinfo/8192 字节）→ Key 校验 → `maxBytes` 校验 → `doAPI`（Bearer + 重试）。
3. 解析 `{title,content}` → 按 `maxBytes` 截断 UTF-8 → 返回 `{provider,url,title,content,bytes,truncated[,note,hint]}`。

## 3. 契约面

- 契约源 = `web_fetch.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`url` required 1..8192（字节）；`maxBytes` 1..8388608 default 4194304；`additionalProperties:false`。
- 摘要：Markdown 正文、不渲染 JS/不过登录墙/不抓二进制 PDF、截断/8MiB/超时/空正文提示。

## 4. 实现面

- **URL**：空、>8192 字节、解析失败、scheme 非 http(s)、host 空、带 userinfo 均拒绝。
- **体积**：`maxBytes` 参数 >0 覆盖配置 `fetch-max-mb`（默认 4MiB），>8MiB 直接报错；解析后按字节截断并回退 UTF-8 字符边界，`truncated` 标记。
- **doAPI**：单次尝试超时 = min(cfg.TimeoutSec, 55s)；3 次重试（1s/2s 退避）；8MiB 读取上限超限明确报错并建议 web_download；401/402/403/404/400 不重试。
- **失败记录**：网络/解析失败都写 `record`，状态页可排障。
- **空正文**：`note:"empty_content"` + hint（登录墙/反爬/纯 JS，可改用 web_download）。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 成功 | false | `{provider:"deepseek",url,title,content,bytes,truncated}` |
| 空正文 | false | 同上 + `content:"",bytes:0,note:"empty_content",hint` |
| 截断 | false | `truncated:true,bytes:<保留字节>` |
| Key 未配置/参数错误/上游错误/超时 | true | 可执行文案或上游状态 |
| 异步 | — | gateway 180s 转后台 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P3 | 无 `final_url`/`content_type`/`status` | 上游只回 `{title,content}`；被重定向/抓到错误页时模型无法判断（需求上游字段）。 |
| P3 | 网络错误透出英文原文 | 已附 web_download 建议；原文保留便于排障。 |
| P3 | 严格参数校验靠 schema | 运行时未拒绝未知键/类型错误（SDK 宽松取值）。 |

已修（2026-11-10）：正文无上限/无截断标记、8MiB 静默截断、无体积参数、URL 字符/字节歧义、空正文无提示、解析失败不记状态、description 未写能力边界。

## 7. 测试/验收

- 既有 `TestEmbeddedToolContracts`：契约/策略/schema。
- 回归：`go test ./...`（web 插件）通过；URL/截断路径由实现与契约覆盖。