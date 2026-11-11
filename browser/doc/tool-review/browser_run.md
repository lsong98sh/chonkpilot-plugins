---
plugin: browser
tool: browser_run
contract: programmatic
handler: src/main.go:95
reviewed_at: 2026-11-08
status: verified
---

# browser_run 审查

## 1. 一句话结论

有风险，不建议当前形态：最小可用路径（navigate/click/text）能跑通，但工具描述与引擎参数名不一致（`eval(expr)` vs `script`）、screenshot 写入完全不校验沙箱、`context_id` 不校验归属，且 `html/text/console_dump` 无任何输出上限。

## 2. 调用链

LLM → gateway `doCall`（剥离 `tool_call_display_name`；`mcp.timeout_sec=180` → `entry.TimeoutSec`；工具无 `_meta.async` → mode=`auto`、阈值 180s）→ 插件 MCP `tools/call`（SDK `startMCP`，`chonkpilot-framework/plugin/src/sdk/plugin.go:544-557`；注册点 `src/main.go:55-56`）→ `browserPlugin.run`（`src/main.go:95`）→ action 归一/必填校验 → `engineclient.Ensure`（`src/main.go:100`；`framework/.../sdk/engineclient/client.go:119-162`）→ 未就绪则后台 spawn 引擎 exe 并退避 ping（`client.go:166-239`）→ `Client.Call`（`client.go:258-290`；每次新建 UDS 连接、一行一 JSON）→ enginehost `dispatch`（`framework/.../sdk/enginehost/server.go:200-215`）→ `handleOperation`（`server.go:290-324`；同一 context 用 `entry.mu` 串行）→ `webviewEngine.Do`（`src/engine/internal/browserplugin/engine_webview.go:127-272`）→ 结果 map 回插件补 `context_id`（`src/main.go:145-150`）→ SDK `json.MarshalIndent` 成 MCP TextContent（`sdk/plugin.go:613-622`）→ gateway/LLM。耗时超 180s 时 gateway 先回 pending，终态经 `mcp-tasks-report`。

## 3. 契约面

本插件**没有 `.tool.md`**，工具在 `main.go` 里程序化注册；**schema 由代码构造**（`src/main.go:82-93`），无契约 YAML 可比对，下面以代码为准。

- schema：`{type:object, properties:{action:string, context_id:string, purpose:string, args:object}, required:["action"]}`；`action` 无 enum、`args` 无子 schema、无 `additionalProperties:false`（`src/main.go:82-93`）。
- gateway 会额外注入必填参数 `tool_call_display_name`（`chonkpilot-agent/gateway/src/registry.go:32-63`），插件侧看不到（基线 §2）。
- 插件级默认值：`purpose` 缺省 `"llm"`（`src/main.go:155-157`）；`headless/width/height/debug` 取配置，缺省 headless=true、1280×800（`src/config.go:93-95`、`src/main.go:158-164`）；`screenshot` 未给 path 且配置了 `screenshot-dir` 时补 `<dir>/browser-<ms>.png`（`src/main.go:139-144`）。
- 摘要（`src/main.go:78-80`）与实现对照：

| 描述 | 实现 | 结论 |
|---|---|---|
| `eval(expr)` | 只读 `args.script`（`engine_webview.go:150-154`） | **不一致（P0）** |
| `wait(selector\|ms)` | 只支持 selector，可选 `timeoutMs`；没有 ms-only 模式（`engine_webview.go:208-223`） | 不一致 |
| `text(selector?)` | selector 必填（`engine_webview.go:171-174`） | 不一致 |
| `screenshot(path?)` | path 必填；仅配置了默认目录时才可省略（`engine_webview.go:226-229`、`main.go:140-144`） | 条件成立 |
| action 列表（schema）含 `ping` | `ping/status` 都接受（`main.go:104-105`） | `status` 未声明 |
| “首次调用会自动开会话” | 成立（`main.go:128-134`） | 一致 |
| `console_dump` / `html` / `title` / `url` / `reload` / `back` / `forward` / `set_html` / `debug_open` | 均存在（`engine_webview.go:142-267`） | 一致 |
| 未提及 | `js`/`goto`/`open`/`shot`/`type` 是引擎别名（`engine_webview.go:130,150,194,225`） | 模型无从发现 |

## 4. 实现面

- 入口：`action` 小写+trim，空则整体失败（`src/main.go:96-99`）；不在插件侧做 action 白名单，未知 action 由引擎返回 `unsupported action`（`engine_webview.go:269-271`）。
- 每次调用（含 `stop`/`ping`/`status`）都先 `Ensure`（`src/main.go:100`），因此 `stop` 即使无活动 context 也会拉起引擎（`:109-115`）。
- 会话映射：`map[instance_id]context_id`，仅内存（`src/main.go:30,168-194`）；`stop` 先 `take`（删除映射）再调引擎，失败也会丢映射；`start` 直接覆盖映射，旧 context 留在引擎里。
- 引擎池：MaxContexts=8、IdleTimeout=10min、janitor 1min（`enginehost/server.go:73-79,340-375`）；janitor 回收 context 不会通知插件 → 插件映射变陈旧。
- 并发：每个 context 一把 `entry.mu` 串行（`server.go:310-314`）；不同 context 完全并发；`Ensure` 用共享 startup 去重 spawn（`client.go:119-162`）。
- 取消/超时：`Client.Call` 用 ctx deadline 或默认 120s（`client.go:266-270`）；超时只是关连接，**引擎侧操作继续跑**并继续持锁；插件无法取消引擎操作。
- 内部超时：eval 15s、title/url 5s、text/html 10s、navigate 15s、wait 默认 10s（`timeoutMs` 无上限）、screenshot 20s、reload 15s（`engine_webview.go:26,139,155,162,165,176,188,202,213,234,252`）。
- WebView2：每 context 一个窗口 + 一个 OS 线程 `Run`（`engine_webview.go:59-83`）；`profile` 未传，`profileDir` 恒为 `%LOCALAPPDATA%\chonkpilot\plugins\profiles\default`（`engine_webview.go:46-49,104-124`）→ 所有 context 共用 cookie/localStorage/cache。
- 截图：LLM 给的 path 直达引擎 `os.MkdirAll` + `os.WriteFile(0644)`（`engine_webview.go:238-243`），插件**从未读取 `cx.Sandbox`**（`sdk/plugin.go:70-73` 定义了策略，`src/main.go` 无引用）。
- 网络/读取面：`navigate` 放行 `http/https/file/about`（`engine_webview.go:407-416`）；`file://` + `html/text` 可把本机文件内容读进上下文；`eval` 可执行任意页面 JS。
- 资源清理：`stop` → `Terminate` + 最多 5s + `Destroy`（`engine_webview.go:275-287`）；引擎进程本身常驻，不随 context 退出。
- 幂等：navigate/click/fill 不幂等；无变更版本号/锁跨进程。

## 5. 返回面：LLM 实际看到的所有形态

成功 = MCP `TextContent`（缩进 JSON，`isError=false`）；失败 = `TextContent` 纯文本 + `isError=true`（`sdk/plugin.go:613-626`）。没有 `status/code` 信封。

| 形态 | isError | LLM 看到的完整内容 |
|---|---|---|
| 原子操作成功 | false | `{...动作字段..., context_id}`：navigate`{url,ready}`、click`{clicked}`、fill`{filled}`、wait`{found,selector}`、text`{text}`、html`{html}`、title`{title}`、url`{url}`、eval`{value}`、screenshot`{path,bytes,format:"png"}`、console_dump`{entries,count}`、set_html`{ready}`、reload/back/forward`{ready}`、debug_open`{opened}`；均追加 `context_id`（`engine_webview.go:130-267`、`main.go:149`） |
| start 成功 | false | `{context_id,purpose,engine:"webview2"}`（`enginehost/server.go:252-259`、`src/engine/main.go:42`） |
| stop 成功 | false | `{closed:true,context_id}`（`server.go:286`） |
| stop 无活动会话 | false | `{ok:true,stopped:false,reason:"无活动 context"}`（`main.go:113-115`） |
| ping/status | false | ping：`{ok,contexts,uptime_s,idle_s,socket}`（`server.go:327-338`） |
| 部分成功 | false | navigate/reload/back/forward/set_html 返回 `ready:false`，**没有 error**；页面未加载完成也会被当成功（`engine_webview.go:139-140,148,252-260`） |
| 空结果 | false | `console_dump{entries:"",count:0}`；`eval{value:""}`；`text` 空 innerText 反而报错（见下） |
| action 缺失 | true | `action 必填`（`main.go:98`） |
| 参数缺失 | true | `operation_failed: url is required` / `selector is required` / `script is required` / `html is required` / `path is required`（`engine_webview.go:133,146,153,172,185,197,210,228`） |
| 元素不存在 | true | `operation_failed: element not found: <sel>` / `click failed: element not found (<sel>)` / `fill failed: element not found (<sel>)`（`engine_webview.go:178,190,204`） |
| wait 超时 | true | `operation_failed: wait timeout: <sel> not found within <dur>`（`engine_webview.go:223`） |
| 目标值语义空 | true | selector 命中但 `innerText === ""` 时 `element not found: <sel>`（`engine_webview.go:175-179`）——误报 |
| URL 方案拒绝 | true | `operation_failed: refused url scheme (only http/https/file/about)`（`engine_webview.go:135-137`） |
| 未知 action | true | `operation_failed: unsupported action: <action>`（`engine_webview.go:269-271`） |
| DevTools 接口不可用 | true | `operation_failed: devtools not available (start with debug=true)`（`engine_webview.go:262-267`）；该文案只在 WebView 不实现 `DevTools` 时出现，与配置 `debug` 无直接绑定 |
| 引擎未启动/缺失 | true | `引擎缺失: <插件根>/chonkpilot-browser-plugin.exe` / `启动引擎失败: ...` / `引擎未就绪（exe=..., socket=...）`（`client.go:160,218-239`） |
| 引擎崩溃/断连 | true | 原始 Go 错误文本：`EOF`、`read unix ...: ...`、`write unix ...: ...`（`client.go:277-279`），无错误码、无恢复提示 |
| context 过期/未知 | true | `unknown_context: unknown or expired context: ctx-N`（`server.go:302-308`） |
| context 上限 | true | `too_many_contexts: max contexts reached (8); stop an idle context first`（`server.go:228-232`） |
| 用户取消 | true | `context canceled`（MCP 层，`sdk/plugin.go:552-554`）；引擎操作仍在跑 |
| 客户端超时 | true | `read unix ...: i/o timeout`（`client.go:266-270`，默认 120s） |
| 异步已受理 | false（gateway 层） | 超过 180s：`{status:"pending", task_id,...}`，终态经 `mcp-tasks-report`（`mcpgateway.go:1095-1102`） |
| 沙箱拒绝 | — | **不存在该形态**：代码没有任何 `cx.Sandbox` 判断 |
| 内部错误 | true | `internal_error: server closing`（`server.go:224-227`） |
| 响应超 8MB | true | 引擎 `response frame too large` 写失败 → 连接关闭，客户端得到 `EOF`（`enginehost/proto.go:26,101-112`） |

体积风险：`html` 是全页 `outerHTML`、`text` 是整段 `innerText`、`console_dump` 最多 2000 条且以 **JSON 字符串**（二次转义）返回（`engine_webview.go:167-180,246-248`）；没有分页/截断/next 指针。`screenshot` 只回 `{path,bytes,format}`，**PNG 二进制不进 LLM 上下文**（`engine_webview.go:244`）；`eval` 的返回是引擎字符串，内容由页面决定，可能很大。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | 描述/示例写 `eval(expr)`，实现只读 `args.script` | `src/main.go:79,89`；`engine_webview.go:150-154` | 模型按描述填 `{expr}` 必然得到 `script is required`；`js` 别名同样不可发现 |
| P0 | `screenshot` 把 LLM 给的 path 直接落盘，完全不校验 `_meta.chonkpilot.sandbox` | `src/main.go:139-149`；`engine_webview.go:225-244`；`sdk/plugin.go:70-73` | 任意绝对路径创建目录并写 0644 PNG，绕过写沙箱；配合 `set_html` 可写内容可控的图片 |
| P0 | `context_id` 无归属校验，任何实例可操作任意 context | `src/main.go:124-127`；`server.go:302-314`；context id 为顺序 `ctx-N`（`server.go:233-234`） | 实例 A 传 `ctx-1` 即可读写实例 B 的页面/cookie；gateway 的 instance 工具面隔离被绕过 |
| P1 | janitor 回收或引擎崩溃重启后，插件映射不失效，后续调用永久 `unknown_context` | `server.go:340-375`；`client.go:218-239`；`src/main.go:124-134` | 无自动恢复；模型必须自行 `start`（又会多留一个旧 context） |
| P1 | `html/text/console_dump/eval` 无任何输出上限；8MB 帧上限表现为 `EOF` | `engine_webview.go:167-180,246-248`；`enginehost/proto.go:26,101-112` | 单次调用可灌满上下文；超大页面变成不可解析的断连 |
| P1 | `args` 无子 schema，描述与实际参数名不符（`wait` 的 ms 模式、`text` 的“可选”selector、`screenshot(path?)`） | `src/main.go:78-92`；`engine_webview.go:170-179,208-213,225-229` | 模型必然反复试错；未配置默认目录时 `screenshot` 省略 path 必失败 |
| P1 | `wait.timeoutMs` 无上限且引擎不可取消；超时只关客户端连接 | `engine_webview.go:213-223`；`server.go:310-314`；`client.go:266-270` | 传大值后该 context 被锁死，后续调用全部超时 |
| P1 | 所有 context 共用同一 WebView2 profile（`profile` 从未传入） | `src/main.go:158-164`；`engine_webview.go:46-49,104-124` | 跨实例/跨会话共享 cookie、localStorage、缓存；多实例隔离名存实亡。同 profile 并发建 WebView2 的稳定性 [待核实] |
| P2 | `status` 被接受但未出现在 schema/描述；`console_dump.count` 用 `strings.Count(out, "\"level\"")` 统计；`debug_open` 在 `debug=false`（DevToolsDisabled）时仍返回 `opened:true` [推断] | `src/main.go:86,104`；`engine_webview.go:63-64,246-248,262-267` | 模型无法发现 status；日志文本含 `"level"` 时计数错误；DevTools 实际不可用却报成功 |
| P2 | `stop`/`ping`/`status` 也先 `Ensure`，无会话也会拉起引擎 | `src/main.go:100-115` | 空转进程常驻（10min idle 后仅回收 context，不杀进程） |
| P2 | `start` 重复调用不回收旧 context；上限 8 | `src/main.go:153-174`；`server.go:228-232` | 反复 start 泄漏 context，最终 `too_many_contexts` |
| P2 | 长 socket 路径回退不一致：引擎改监听 `%TEMP%/chonkpilot-engine.sock`，客户端仍拨长路径 | `server.go:97-100`；`client.go:85-92` | 长 `LOCALAPPDATA` 环境下引擎永远“未就绪” |
| P2 | `CHONK_BROWSER_SOCK` 只在引擎侧生效，客户端不读 | `server.go:378-387`；`client.go:85-92` | 设置该环境变量后两侧路径不一致 |
| P2 | selector 命中但 innerText 为空 → `element not found` | `engine_webview.go:175-179` | 空元素被误判为不存在，模型可能放弃 |
| P2 | 错误文案为英文/中文混杂的原始文本，无错误码与下一步 | `client.go:280-284`；`engine_webview.go` 各处 | 模型难以区分“重试/改参数/换工具” |
| P2 | `unquoteJSON` 手写去引号：以引号开头结尾的 JSON 标量会被“解包” | `engine_webview.go:320-352` | `eval` 返回 `"\"x\""` 类值时内容被改写 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| 统一参数名为 `script`（描述+示例），或引擎同时接受 `expr/script` | 改善 | 消除必然失败 | 低（1 文件） | 不 breaking（加别名） | 建议现在做 |
| 用 `action` 的 `oneOf`/分支子 schema 声明每个 action 的必填 args，并给 `enum` | 改善 | 少试错、少 token | 中（1 文件） | schema 变更，非 breaking | 建议现在做 |
| `screenshot` 走与 file 一致的沙箱校验；未给 path 且无默认目录时回落到插件数据目录 | 优化 | 消除沙箱绕过与必填歧义 | 中（`main.go`+`engine`） | 行为变更（越界将被拒） | 建议现在做 |
| `html/text/console_dump` 增加 `maxChars`（默认 20k）与 `truncated/totalChars` 字段 | 改善 | 防爆上下文、可判断是否截断 | 低-中 | 新增可选字段，非 breaking | 建议现在做 |
| `context_id` 校验归属（插件侧记录 instance→ctx，跨实例拒绝） | 优化 | 恢复 instance 隔离 | 低 | 会拒绝现有跨实例用法（无人应为合法） | 建议现在做 |
| 识别 `unknown_context` 自动清映射并重开一次（`start` 后重试原 action） | 优化 | 崩溃/回收后可自愈 | 低-中 | 重试可能重复点击等副作用，需白名单 action | 排期做 |
| 给 `wait.timeoutMs` 设上限（如 60s）并映射到 ctx 取消 | 优化 | 不再锁死 context | 低 | 大值被拒 | 建议现在做 |
| 按读/写/导航拆成 `browser_nav`/`browser_read`/`browser_shot` | 增强 | 工具描述更聚焦、可按需授限 | 高（契约+网关+提示词） | breaking | 仅记录 |

## 8. 建议的测试/验收（不执行）

1. 按工具描述原样调用 `eval` 且只给 `{expr:"1+1"}` → 期望失败，用于证明 P0；改为 `script` 后成功。
2. `screenshot` 在沙箱开启、path 指向白名单外 → 期望整体拒绝且不落盘（当前会落盘）。
3. 实例 A `start` 后，实例 B 传 `context_id=ctx-1` 执行 `text` → 期望拒绝（当前成功）。
4. 等 10min（或调小 IdleTimeout）让 janitor 回收后再调用旧 context → 记录错误与恢复路径；验证“自动重建”改造。
5. `html` 抓取 >8MB 页面 → 记录返回值/断连形态；验证 `maxChars` 改造。
6. `wait` 传 `timeoutMs=600000`，在 120s 后发第二个同 context 调用 → 观察第二个调用是否被锁死。
7. 引擎 exe 改名/删除 → 校验 `引擎缺失:` 文案；运行中 kill 引擎 → 校验下一次调用的错误与重启行为。
8. 两个实例同时 `start`，检查 profile 目录共用导致的 cookie 互通（以及第二个 context 是否创建成功）。

## 9. 基线修正（仅当共通基线不成立时）

- 基线 §6“缺省超时的工具可能被 gateway 注入 async:always”在本插件**不成立**：无 `.tool.md`/`_meta.async`，gateway 缺省 mode=`auto`，超时点取 manifest `mcp.timeout_sec=180`，到点转后台而非 `always`（`mcpgateway.go:933-958,1095-1102`）。
- 基线 §4 的路径规则**不适用**：`screenshot.path` 无绝对/相对校验，相对路径按引擎进程 cwd 解析；`file://` 读取不经任何白名单。
- 基线 §3 的沙箱只对写工具生效：本插件唯一的写工具是 `screenshot`，但实现完全不读 `sandbox`（`src/main.go` 无 `cx.Sandbox` 引用）。
- 基线 §5“不要假设运行时强校验”成立且更弱：`args` 是自由对象，插件层不做任何子参数校验，全部错误来自引擎。