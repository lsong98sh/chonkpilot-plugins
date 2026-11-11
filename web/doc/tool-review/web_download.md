---
plugin: web
tool: web_download
contract: src/tools/web/web_download.tool.md
handler: web/src/download.go:44（download）
reviewed_at: 2026-11-10
status: verified
---

# web_download 审查

> 2026-11-08 首审；2026-11-10 复核。旧审查中的 P0 SSRF、maxBytes 无上限、4xx/5xx 当成功、GBK 文本变 base64、并发 `.part` 冲突、headers 静默丢弃、maxRedirects null 当 0、URL 无 host/长度校验、落盘不记状态等问题已修复。

## 1. 一句话结论

可用。默认拒绝 loopback/私网/链路本地并逐跳校验重定向；4xx/5xx 一律 `isError`；**落盘模式流式写盘、默认单文件上限 2048MB（不受 8MiB 内容上限限制）**；内容模式 `maxBytes` 硬上限 8MiB 且截断置 `truncated`（回显 `limit_bytes`，有 Content-Length 时给 `content_length`）；文本按 charset 解码，二进制才 base64；落盘用唯一 `.part` 临时文件；参数/状态/返回口径已在契约声明。

## 2. 调用链

1. LLM `web_download` → gateway → 插件 MCP（契约 `web_download.tool.md`，`_meta.timeout=180`）。
2. `searcher.download`：URL/SSRF 校验 → method/headers 校验 → 计算 maxBytes/maxMB/timeoutSec/maxRedirects → `http.Client`（逐跳 SSRF + 重定向上限）。
3. 4xx/5xx 先返回错误；否则内容模式读入并解码/截断，或落盘模式流式写 `.part` → rename。
4. `defer` 统一写 status（成功 1 条 / 失败错误）。

## 3. 契约面

- 契约源 = `web_download.tool.md`；schema/description/`_meta` 均来自 md。
- schema：`url` required 1..8192；`method` GET/POST/PUT/DELETE/HEAD；`headers` 值为字符串；`maxBytes` 1..8388608 default 4194304；`maxMB` 1..2048 default 2048；`timeoutSec` 5..3600；`maxRedirects` 0..20；`additionalProperties:false`。
- 摘要：SSRF 默认拒绝 + 每跳校验、4xx/5xx 错误、8MiB 内容硬上限、charset 解码/base64、唯一 `.part`、沙箱白名单、重定向/超时语义。

## 4. 实现面

- **URL**：空/超 8192 字节/非 http(s)/host 空拒绝；`allow-private=false` 时 `checkURLNotPrivate` 拒绝私网目标。
- **请求**：method 白名单；`headers` 必须是对象且每值为字符串，`Host` 覆盖直接拒绝；body 按传入原样发送。
- **重定向**：`maxRedirects=0` 不跟随；`len(via)>maxRedirects` 报错；每跳重新做 SSRF 校验。
- **状态码**：≥400 直接错误（错误页不落盘/不返回内容）。
- **内容模式**：`maxBytes` 参数 >0 覆盖配置，>8MiB 直接报错；`LimitReader(maxBytes+1)` 读取，超限截断并 `truncated=true`；回显 `limit_bytes`，响应头有 Content-Length 时另给 `content_length`；文本按 charset 解码，失败走 base64。
- **落盘模式**：`saveAs` > `dir`+`filename`；`saveAs` 指向已存在目录按 URL Base（经 sanitize）拼名，不存在路径按文件名处理；扩展名白名单；`overwrite=false` 时 `-N` 唯一名；沙箱按最终路径校验；`os.CreateTemp(dir, base+".part.*")` 唯一临时文件 → `io.Copy` 限流 → rename，失败/超限删除。
- **状态**：handler `defer` 统一 `record`，下载也会出现在 status 页。
- **超时**：`timeoutSec` 5..3600；宿主 180s 会转后台，契约已声明。

## 5. 返回面

| 形态 | isError | LLM 可见 |
|---|---|---|
| 内容·文本 | false | `{url,final_url,status,content_type,redirects[,content_length],bytes,limit_bytes,truncated,encoding,content}` |
| 内容·二进制 | false | `{…,content_base64,encoding:"base64"}` |
| 内容·截断 | false | `truncated:true,limit_bytes:<上限>`；有 Content-Length 时可对照 `content_length` |
| 落盘成功 | false | `{…,path,saved_to,bytes}` |
| 4xx/5xx | **true** | `web_download：目标返回 HTTP N…`（不落盘） |
| SSRF/参数/网络/超限/沙箱 | **true** | 明确错误文案 |
| 异步 | — | gateway 180s 转后台 |

## 6. 剩余问题

| 级别 | 问题 | 说明 / 建议 |
|---|---|---|
| P2 | `timeoutSec` 可设 3600 但宿主 180s 转后台 | 契约已声明；参数仍可能让模型误以为能同步等 30 分钟。可考虑把上限收到 150s，或明确建议大文件落盘。 |
| P3 | chunked 响应截断后无法知道总长度 | 无 Content-Length 时必须读完整个响应才能计数，与截断省流量/内存的目的冲突；已回显 `limit_bytes` 并明确 `truncated` 只表示“还有更多”。真要续取需 Range/落盘。 |
| P3 | 网络错误透出 net/http 英文原文 | 含目标地址；已带“检查 URL/改用 web_fetch”建议。 |
| P3 | `overwrite=false` 的 `uniquePath` 仍是 stat-then-create | 并发同名可能都选到同一目标（各自写完整文件，最后 rename 覆盖），不产生损坏文件但可能丢一路结果。 |
| P3 | `body` 对 GET/HEAD/DELETE 也发送 | 契约已声明；特殊场景需调用方自行约束。 |
| P3 | 严格参数校验仅覆盖 headers/maxRedirects/数字参数 | 未知键仍被忽略（schema `additionalProperties:false` 只约束 LLM）。 |

已修（2026-11-10）：P0 SSRF/内网访问、maxBytes 无上限、4xx/5xx 当成功、GBK 文本 base64、并发 `.part` 冲突、headers 静默丢弃、maxRedirects null 当 0、URL host/长度、saveAs 目录 `..`、落盘不记状态、内容硬上限从 64MiB 收到 8MiB（防 base64/上下文爆量）；**落盘默认单文件上限 100MB → 2048MB**（数百 MB 文件无需额外参数，且不受内容模式 8MiB 限制）。

## 7. 测试/验收

- 既有 download 测试：重定向 0/1、HTTP 状态、沙箱、Host 覆盖拒绝、落盘/覆盖。
- `TestEmbeddedToolContracts`：契约/schema（maxBytes 上限 = 8MiB）。
- 回归：`go test ./...`（web 插件）通过。