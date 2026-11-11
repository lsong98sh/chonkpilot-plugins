# web 工具审查索引

审查基线：`E:/BizWorks/chonkpilot-plugins/工具审查.md` 第 101–284 行。2026-11-10 复核：三工具均已迁移到 `src/tools/web/*.tool.md`（schema/description/`_meta` 来自 md），旧审查中的 P0/P1 已修复，现仅余 P2/P3。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| web_search | 可用（正文预算/计数回显/8MiB 明确报错/超时对齐 180s） | 0 | 0 | 1 | [web_search.md](./web_search.md) |
| web_fetch | 可用（maxBytes/truncated、8MiB 明确报错、空正文提示） | 0 | 0 | 0 | [web_fetch.md](./web_fetch.md) |
| web_download | 可用（SSRF/4xx/编码/唯一 .part 已修；落盘流式、默认单文件 2048MB，不受内容模式 8MiB 限制） | 0 | 0 | 1 | [web_download.md](./web_download.md) |
| **合计** | | **0** | **0** | **2** | |

## 契约来源与注册面

- 三工具均为**契约驱动注册**：`main.go:88` 读取 embed 的 `tools/web/*.tool.md`，`registerTools` 按 tool 名分派 `web_search`/`web_fetch`/`web_download`；schema/description/`_meta`（async/timeout/on_timeout/hot/category）全部来自 md。
- 工具名即 LLM 可见名（manifest `mcp.namespace = "-"`）；manifest `hot_tools` 与 md `hot: true` 一致。
- md `_meta.timeout=180` 与 manifest `mcp.timeout_sec=180` 对齐；gateway 在 180s 转后台。
- SDK 侧仍不做 schema 运行时校验；关键数值/headers 参数在 handler 内做严格校验。

## 与旧审查的差异（已修）

| 旧问题 | 现状 |
|---|---|
| web_download 无 SSRF 防护 | `ssrf.go` 默认拒绝 loopback/私网/链路本地，重定向逐跳校验，`allow-private` 开关 |
| 4xx/5xx 当成功 | 一律 `isError=true`，不落盘 |
| `maxBytes` 无上限 / base64 爆量 | 内容模式硬上限 8MiB；更大文件走落盘模式 |
| GBK/UTF-16 被 base64 | `textdecode.go` 按 charset 解码 |
| 正文无预算/8MiB 静默截断 | web_search 正文预算 + `content_truncated`；web_fetch `maxBytes/truncated`；8MiB 明确报错 |
| 重试最坏 363s > 180s | 单次尝试上限 55s（3 次 + 退避 ≤168s） |
| 并发 `.part` 冲突 | `os.CreateTemp` 唯一临时文件 |
| 程序化注册/无契约 | 已迁移到 `.tool.md` |

## 跨工具遗留（P3/P2）

- `web_download.timeoutSec` 可设到 3600s，但宿主 180s 会转后台（契约已声明）。
- 网络错误仍保留 Go/上游英文原文（附恢复建议）。
- manifest `config.scope = "prjusr"` 与实现只用 usr 层不一致（配置面遗留，P2/跨插件）。

## 关键实现事实（2026-11-10 复核）

| 项 | 现状 |
|---|---|
| API Key | web_search / web_fetch 依赖 DeepSeek Key；web_download 不需要（本地直连） |
| 未配置 Key | 两工具同文案、`IsError:true`，不发起请求 |
| web_search 默认条数 | schema `default:10`（1..50）；响应 `count`（实际）/`count_requested`（请求值） |
| `need_content` 体积 | `max_content_bytes` 总预算（默认 256KiB、上限 8MiB）逐条截断，`content_truncated` 标记 |
| `localization` | 仍原样转发给上游，格式/语义未定义（P2） |
| web_fetch 正文上限 | `maxBytes` 默认 4MiB、硬上限 8MiB，`truncated` 标记 |
| web_download SSRF | 默认拒绝 loopback/私网/链路本地，重定向逐跳校验；`allow-private` 开关 |
| 沙箱白名单 | 解析真实路径（EvalSymlinks）+ `safepath.IsWithin` 边界，不是字符串前缀 |
| 重定向上限 | 默认 5（0..20，0=不跟随）；非整数参数报错 |
| `saveAs/dir/filename` | `saveAs` 优先；目录（已存在）拼 URL 文件名；不存在按文件名处理 |
| `maxBytes/maxMB/timeoutSec` | maxBytes ≤8MiB；maxMB 1..2048；timeoutSec 5..3600（>180 会被宿主转后台） |
| 内容模式 base64 | 上限 8MiB → base64 ≈10.7MiB 文本；更大文件走落盘模式 |
| 落盘清理 | 唯一 `.part.*` 临时文件；copy/close/超限/rename 失败均删除 |

## 跨工具一致性（遗留）

- **manifest `config.scope = "prjusr"` 与实现只用 usr 层**（`plugin/v1/config.user.*`）：项目级配置页不生效，插件也不读 prj 覆盖。属配置面遗留（P2/跨插件）。
- **`web_fetch` 复用检索超时键** `plugins.web.timeout-sec`（label 明说“检索/抓取”）；`fetch-timeout-sec`/`fetch-max-mb` 只服务 web_download，命名易误读。
- **`status` 页面**：search/fetch/download 都会 `record`；但 `last_count` 对下载固定 1，且不能区分工具。
- 三工具错误文案风格不统一：参数错误中文可执行；网络/解析错误仍保留 Go/上游英文原文（附恢复建议）。
- 产品/产物：`web/src/chonkpilot-web-plugin.exe` 文件名与 manifest `runtime.exe = "chonkpilot-web.exe"` 不同（构建链路改名，仅记录）。