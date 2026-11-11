---
plugin: desktop
tool: desktop_run
contract: programmatic
handler: src/main.go:95
reviewed_at: 2026-11-08
status: verified
---

# desktop_run 审查

## 1. 一句话结论

不建议当前形态：`screenshot` 不带目标窗口会 **nil 解引用并打死整个引擎进程**；`click/input/key/move/size/...` 描述未要求目标窗口（且 `start` 不传 meta，绑定入口不可达），按描述调用必然失败；`move`/`size` 的 `SetWindowPos` 标志缺失会破坏用户窗口；screenshot 同样绕过写沙箱。

## 2. 调用链

LLM → gateway `doCall`（剥离 `tool_call_display_name`；manifest `timeout_sec=180` → `entry.TimeoutSec`；无 `_meta.async` → mode=`auto`、阈值 180s）→ 插件 MCP `tools/call`（SDK `startMCP`，`chonkpilot-framework/plugin/src/sdk/plugin.go:544-557`；注册点 `src/main.go:55-56`）→ `desktopPlugin.run`（`src/main.go:95`）→ `Ensure`（`:100`；`engineclient.Client.Ensure` `.../sdk/engineclient/client.go:119-162`）→ spawn `chonkpilot-desktop-plugin.exe` / 退避 ping（`client.go:166-239`）→ `Client.Call("operation")`（`client.go:258-290`）→ enginehost `handleOperation`（`.../sdk/enginehost/server.go:290-324`，同 context `entry.mu` 串行）→ `desktopplugin.engine.Do`（`src/engine/internal/desktopplugin/engine.go:45-204`）→ `desktop.*` Win32/GDI 原语 → 结果 map 补 `context_id` → SDK 编码为 MCP TextContent。

## 3. 契约面

无 `.tool.md`；工具在 `main.go` 程序化注册，**schema 由 `desktopSchema()` 代码构造**（`src/main.go:82-93`），以代码为准。

- schema：`{type:object, properties:{action, context_id, purpose, args}, required:["action"]}`；`args` 是自由对象，无子 schema、无 `additionalProperties:false`、`action` 无 enum。
- gateway 追加必填 `tool_call_display_name`（`chonkpilot-agent/gateway/src/registry.go:32-63`），插件不可见。
- 插件默认值：`purpose` 缺省 `"llm"`（`src/main.go:153-155`）；`start` 只传 `{purpose}`（`:156`），**不传 meta**；`screenshot` 未给 path 且配置了 `screenshot-dir` 时补 `<dir>/desktop-<ms>.png`（`:139-143`）。
- 摘要（`src/main.go:78-80`）与实现对照：

| 描述 | 实现 | 结论 |
|---|---|---|
| `list_windows()` | 成立（`engine.go:59-65`） | 一致 |
| `find_window(title\|class)` | 还接受 `hwnd`（`engine.go:237-257`）；标题失败后退化为子串匹配（`:265-271`） | 描述不完整 |
| `window_info()` / `focus()` / `click(x,y)` / `input(text)` / `key(keys)` / `move(x,y)` / `size(w,h)` / `minimize` / `maximize` / `restore` | **均要求目标窗口**（args 里的 `hwnd/title/class`，或 start 绑定），否则 `no target window`（`engine.go:210-218`） | **描述缺失必填目标（P0）** |
| `screenshot(path?)` | path 必填；不带目标时本意是整屏，实际 panic（`engine.go:132-155`、`capture_export.go:13-27`） | **描述与实现矛盾（P0）** |
| `start/stop 管理会话；首次调用自动开会话` | 成立（`main.go:106-134`） | 一致 |
| 未提及 | `click` 的 `button`（left/right/middle，`engine.go:91-94`）、`size` 别名 `resize`、`shot`/`type`（`engine.go:97,132,167`） | 模型无从发现 |
| `click(x,y)` 坐标语义 | 相对**客户区左上角**（`engine.go:92-94`），未说明；`x/y` 缺省 0 | 语义缺失 |

## 4. 实现面

- 每次调用（含 `stop`/`ping`/`status`）先 `Ensure`（`src/main.go:100`）。
- 会话：`map[instance_id]context_id` 仅内存（`src/main.go:30,160-186`）；`start` 只传 purpose，引擎 `New` 只有在 `meta` 非空时才绑定目标窗口（`engine.go:32-42`），因此 **LLM 无法通过 `start` 预绑定**，每个动作都必须自带目标。
- 目标解析：`hasTargetKey` 认 `hwnd/title/class`（`engine.go:221-234`）；`findWindowFrom` 先 hwnd（数字/字符串），再 class，再 title（精确 → 子串）；失败文案分别为 `window not found by title substring: X`、`specify hwnd, class, or title`。
- `list_windows` 用 `EnumWindowsList` 枚举**全部顶层窗口**（含不可见/系统窗口）（`desktop/window.go:84-93`），每项只给 `hwnd/title/client_screen_rect`，无 `class`/`visible`（`engine.go:277-291`）。
- `click`：`ClientScreenRect(h)` + 相对坐标 → `SetCursorPos` + `mouse_event`；`SetCursorPos` 失败时 `clickAt` 静默返回，调用仍报成功（`desktop/win32.go:42-57`、`engine.go:84-95`）。
- `input`：`SendKeyUnicode` 逐字符 down/up，无 IME 处理、无间隔（`engine.go:97-113`）；`key`：`PressKeySpec` 解析 `ctrl+s` 形式，未知键返回中文错误（`capture_export.go:30-46`、`keyspec.go:34-64`）。
- `move` / `size`：`SetWindowPos` 使用 `SWP_NOZORDER|SWP_NOACTIVATE`，**缺 `SWP_NOSIZE`（move）与 `SWP_NOMOVE`（size）** → move 同时把窗口尺寸置 0，size 同时把窗口移到 (0,0)（`desktop/win32.go:66-75`）。
- `screenshot`：有 `hwnd/title/class` 时截客户区；否则 `h=0` → `CaptureWindowPNG(0)` → `rect=nil` → `captureImage(nil,0)` 解引用 panic（`engine.go:137-144`、`capture_export.go:13-27`、`desktop/screen.go:105-110`）。引擎与 enginehost **都没有 `recover()`**（全仓 grep 无命中）。
- 权限/沙箱：插件从不读 `cx.Sandbox`（`sdk/plugin.go:70-73` 有定义，`src/main.go` 无引用）；screenshot 的 path 由 LLM 直传 `os.MkdirAll` + `os.WriteFile(0644)`（`engine.go:149-153`）。
- 会话池：MaxContexts=8、IdleTimeout=10min、janitor 1min（`enginehost/server.go:73-79,340-375`）；desktop `Close()` 是空操作，stop 只释放池项，不动目标窗口（`engine.go:206-207`）。
- 并发：同 context 串行（`server.go:310-314`）；不同 context 可并发操作**同一目标窗口**，鼠标/键盘是全局状态，无窗口级互斥。
- 资源：每次 `EnumWindowsList`/`EnumWindowsFind` 都 `syscall.NewCallback`（`desktop/window.go:70-77,87-92`），长期高频调用可能耗尽进程 callback 槽 [推断]。
- 幂等：click/input/key/move 均不幂等；无版本号/锁。

## 5. 返回面：LLM 实际看到的所有形态

成功 = 缩进 JSON + `isError=false`；失败 = 纯文本 + `isError=true`（`sdk/plugin.go:613-626`）。无 `status/code` 信封。

| 形态 | isError | LLM 看到的完整内容 |
|---|---|---|
| start 成功 | false | `{context_id,purpose,engine:"win32"}`（`enginehost/server.go:252-259`、`src/engine/main.go:37-42`） |
| stop 成功 | false | `{closed:true,context_id}`（`server.go:286`） |
| stop 无会话 | false | `{ok:true,stopped:false,reason:"无活动 context"}`（`main.go:113-114`） |
| ping/status | false | `{ok,contexts,uptime_s,idle_s,socket}`（`server.go:327-338`） |
| list_windows | false | `{windows:[{hwnd,title,client_screen_rect:{left,top,right,bottom,width,height}}],count,context_id}`；包含不可见/无标题窗口（`engine.go:59-65,277-291`） |
| find_window / window_info | false | 单个 `{hwnd,title,client_screen_rect}` + `context_id`（`engine.go:52-57,67-72`） |
| focus | false | `{hwnd,focused:true,context_id}`（`engine.go:74-82`） |
| click | false | `{hwnd,screen_x,screen_y,button,context_id}`；`button` 省略时为空串，但实际按左键（`engine.go:84-95`、`win32.go:47-54`） |
| input | false | `{hwnd,chars,context_id}`（`engine.go:113`） |
| key | false | `{hwnd,keys,context_id}`（`engine.go:130`） |
| screenshot | false | `{path,bytes,format:"png",context_id}`（`engine.go:155`） |
| move / size | false | `{hwnd,moved:true}` / `{hwnd,resized:true}` + `context_id`；窗口形态可能已被破坏（见 P1） |
| minimize / maximize / restore | false | `{hwnd,state:"minimized\|maximized\|restored"}`（`engine.go:177-199`） |
| 部分成功/空结果 | false | 无变化语义；`list_windows` 至少 1 项。`start` 在 `instance_id` 为空时也“成功”，但映射不落库（`main.go:160-163`） |
| 无目标窗口 | true | `operation_failed: no target window: pass hwnd/title/class, or bind one at start (meta)`（`engine.go:217`）——`start (meta)` 经工具不可达 |
| 目标不存在 | true | `operation_failed: window not found by title substring: X` / `specify hwnd, class, or title`（`engine.go:271,273`） |
| 参数缺失 | true | `operation_failed: text is required` / `keys is required (e.g. "ctrl+s")` / `path is required`（`engine.go:104,122,135`） |
| 按键非法 | true | `operation_failed: 未知按键 X` / `未知修饰键 X` / `组合键缺主键：X`（`keyspec.go:43,53,58,63`） |
| 前台失败 | true | `operation_failed: SetForegroundWindow failed (non-interactive session?)` / `focus target window failed`（`engine.go:80,107,125`） |
| 截屏失败 | true | `operation_failed: capture: ...`（Method1/2/3 全失败文案）/ `invalid capture rect: WxH`（`screen.go:108-110,176-190`） |
| 写盘失败 | true | `operation_failed: create dir: ...` / `write screenshot: ...`（`engine.go:149-153`） |
| 未知 action | true | `operation_failed: unsupported action: X`（`engine.go:201-203`） |
| context 过期/未知 | true | `unknown_context: unknown or expired context: ctx-N`（`server.go:302-308`） |
| context 上限 | true | `too_many_contexts: max contexts reached (8); stop an idle context first`（`server.go:228-232`） |
| 引擎未就绪 | true | `引擎缺失: <path>` / `启动引擎失败: ...` / `引擎未就绪（exe=..., socket=...）`（`engineclient/client.go:160,218-239`） |
| **screenshot 无目标（崩溃）** | 无工具结果 | `CaptureWindowPNG(0)` → panic → **引擎进程退出**；MCP 连接断开，gateway 侧表现为 transport error（文案由网关生成，非本工具）；插件 `ctxID` 仍指向旧 context |
| 引擎崩溃/断连 | true | 原始 Go 错误：`EOF` / `read unix ...`（`client.go:277-279`） |
| 用户取消 | true | `context canceled`（`sdk/plugin.go:552-554`）；Win32 动作无法撤回 |
| 客户端超时 | true | `read unix ...: i/o timeout`（默认 120s，`client.go:266-270`） |
| 异步已受理 | false（gateway 层） | >180s：`{status:"pending", task_id,...}`，终态经 `mcp-tasks-report`（`mcpgateway.go:1095-1102`） |
| 沙箱拒绝 | — | **不存在该形态**：无 `cx.Sandbox` 判断 |

体积：`screenshot` 只回 `{path,bytes,format}`，**PNG 二进制不进 LLM 上下文**（`engine.go:155`）；`list_windows` 是唯一可能偏大的返回（每窗口 6 个 rect 字段，枚举全部顶层窗口），无 `limit`/分页。

## 6. 一致性与问题

| 级别 | 问题 | 证据 | 影响 |
|---|---|---|---|
| P0 | screenshot 不带目标窗口时 `rect=nil` 被解引用，引擎无 `recover()` → 进程崩溃，所有 context 丢失 | `engine.go:132-155`；`capture_export.go:13-19`；`screen.go:105-110`；全仓无 `recover()` | 按描述 `screenshot(path?)` 调用即可打死引擎；后续调用 `EOF` + 陈旧 `ctxID`，直到下次 Ensure 重启仍不可用 |
| P0 | 10 个动作（window_info/focus/click/input/key/move/size/minimize/maximize/restore，以及带目标的 screenshot）要求目标窗口，描述/schema 只写 `click(x,y)` 等；`start` 不传 meta，`bind at start (meta)` 不可达 | `main.go:78-92`；`main.go:152-166`；`engine.go:210-218` | 模型按描述首次调用必然 `no target window`；即使读到错误，也无法用 `start` 预绑定 |
| P0 | `screenshot` 的 path 由 LLM 直传落盘，不校验 `_meta.chonkpilot.sandbox` | `main.go:139-149`；`engine.go:149-153`；`sdk/plugin.go:70-73` | 任意路径建目录、覆盖写 0644 PNG；写沙箱被绕过（且可截取任意窗口内容） |
| P0 | `context_id` 无归属校验，顺序 id 可被任何实例猜中并使用 | `main.go:124-127`；`server.go:302-314`；id=`ctx-N`（`server.go:233-234`） | 实例 A 可操作实例 B 绑定的窗口（点击/输入/截图），instance 隔离失效 |
| P1 | `move`/`size` 的 `SetWindowPos` 缺 `SWP_NOSIZE`/`SWP_NOMOVE`：move 把窗口尺寸置 0，size 把窗口移到 (0,0) | `desktop/win32.go:66-75` | 用户窗口被破坏（尺寸 0 几乎不可恢复），调用仍返回 `moved/resized:true` |
| P1 | janitor 回收或引擎崩溃重启后映射不失效，后续调用永久 `unknown_context` | `server.go:340-375`；`client.go:218-239`；`main.go:124-134` | 无自动恢复；模型只能显式 `start`（且旧 context 泄漏） |
| P1 | `list_windows` 返回全部顶层窗口，无 `class`/`visible`，模型难以定位目标 | `engine.go:59-65,277-291`；`desktop/window.go:84-93` | 选中隐藏/同名窗口的风险高；截图/点击打到错误窗口；输出 token 偏大 |
| P1 | `screenshot` 的“目标窗口 vs 整屏”语义未声明；有目标时截客户区，无目标时崩溃 | `engine.go:137-144`；`capture_export.go:10-27` | 模型无法预期截的是窗口还是屏幕；无目标路径直接崩溃（见 P0） |
| P1 | 同 context 串行、不同 context 之间对同一窗口无互斥；输入注入是全局状态 | `server.go:310-314`；`engine.go:84-130` | 两个实例并发 click/input 会交错，产生不可解释的 UI 结果 |
| P2 | `click` 在 `SetCursorPos` 失败时静默返回成功 | `desktop/win32.go:42-45`；`engine.go:94-95` | 模型认为已点击，后续状态不符却无错误 |
| P2 | `click` 的 `x/y` 缺省 0（客户区左上角），缺省即点击 | `engine.go:89-90` | 模型漏填坐标会静默点左上角，可能触发危险按钮 |
| P2 | `button` 参数、`hwnd` 查找、`shot`/`type`/`resize` 别名均未在 schema/描述中声明 | `engine.go:91,97,132,167`；`main.go:86-89` | 能力不可发现，模型只能用默认左键 |
| P2 | `window_info`/`list_windows` 不含 `class`/`visible`，但 `find_window` 支持 class | `engine.go:277-291` | 想按 class 找窗口的模型无法从列表获得 class |
| P2 | 每次枚举窗口（`list_windows`/`find_window`）都 `syscall.NewCallback`，无释放 | `desktop/window.go:70-77,87-92` | 高频 `list_windows` 长期运行可能耗尽 callback 槽 [推断] |
| P2 | `start` 重复调用泄漏 context；上限 8 的错误为英文且无插件侧清理 | `main.go:152-166`；`server.go:228-232` | 反复 start 后 `too_many_contexts`，模型不知如何处理 |
| P2 | 非交互会话下 `SetForegroundWindow` 失败的文案只给推测，无降级路径 | `engine.go:80,107,125` | 模型无法判断应改用 `click` 还是放弃 |
| P2 | `stop`/`ping` 先 Ensure，空会话也会拉起引擎 | `main.go:100-115` | 空转进程常驻 |

## 7. LLM 视角优化 / 改善 / 增强

| 建议 | 类型 | LLM 收益 | 成本 | 兼容性风险 | 可行性 |
|---|---|---|---|---|---|
| schema 里显式声明 `target:{hwnd\|title\|class}`，并对需要目标的 action 标 required；`start` 增加 `args.title/class/hwnd` 并透传 meta | 改善+增强 | 消除“必然失败”，支持一次绑定多动作 | 中（插件+引擎已支持 meta） | 新增可选字段，非 breaking | 建议现在做 |
| `screenshot` 修复 `hwnd=0` 路径：用 `GetFullScreenRect()`（已存在未用）或明确要求 target | 优化 | 消除引擎崩溃 | 低（1-2 文件） | 行为从“崩溃”变为“整屏截图”，非 breaking | 建议现在做 |
| `move`/`size` 补 `SWP_NOSIZE`/`SWP_NOMOVE` | 优化 | 不再破坏用户窗口 | 低（1 文件） | 修复性，非 breaking | 建议现在做 |
| screenshot 走沙箱白名单；未给 path 时回落插件数据目录并在返回中标注 | 优化 | 消除写沙箱绕过 | 中（插件+引擎） | 越界将被拒绝 | 建议现在做 |
| `context_id` 校验归属（instance→ctx） | 优化 | 恢复实例隔离 | 低 | 拒绝跨实例用法 | 建议现在做 |
| `list_windows` 增加 `class`/`visible` 过滤（默认只返回可见且有标题），并提供 `limit` | 改善 | 选中准确、省 token | 低-中 | 返回字段新增，非 breaking | 建议现在做 |
| 给 `click`/`input` 等加窗口级互斥（同一 hwnd 串行） | 优化 | 并发结果可解释 | 中 | 无 | 排期做 |
| 错误统一为 `{code,message,hint}`，`no target window` 附 `list_windows` 建议 | 改善 | 可恢复 | 中 | 返回结构变更 | 排期做 |
| 把 `desktop_run` 拆成 `desktop_window` / `desktop_input` / `desktop_shot` | 增强 | 权限与描述更聚焦 | 高 | breaking | 仅记录 |

## 8. 建议的测试/验收（不执行）

1. `{action:"screenshot", args:{path:"<tmp>\\a.png"}}`（无 hwnd/title/class）→ 期望返回整屏图；当前应复现引擎 panic/进程退出。
2. 按描述 `{action:"click", args:{x:10,y:10}}` → 期望失败文案包含“需要目标”；验证 start 绑定改造。
3. 对记事本执行 `move(x,y)` 后检查窗口尺寸是否变成 0；执行 `size(w,h)` 后检查窗口是否跳到 (0,0)。
4. 沙箱开启、screenshot path 指向白名单外 → 期望拒绝且不落盘（当前会落盘）。
5. 实例 B 传实例 A 的 `context_id` 执行 `focus/input` → 期望拒绝（当前成功）。
6. 引擎 kill 后调用旧 context → 记录 `unknown_context`，验证自动重建。
7. `list_windows` 在有隐藏窗口/同名标题的桌面上执行 → 检查模型能否仅凭返回字段选对目标。
8. 两个实例并发对同一窗口 `click`/`input` → 观察交错与最终 UI 状态。

## 9. 基线修正（仅当共通基线不成立时）

- 基线 §6 在本插件**不适用**：无 `.tool.md`/`_meta.async`，gateway 缺省 `auto` + manifest 180s 阈值（`mcpgateway.go:933-958,1095-1102`）。
- 基线 §4 路径规则不适用：`screenshot.path` 无绝对/相对校验，相对路径按引擎 cwd 解析。
- 基线 §3 沙箱：本插件写工具 = `screenshot`，但实现完全不读 `sandbox`（`src/main.go` 无 `cx.Sandbox`）。
- 基线 §5“不要假设运行时强校验”成立：`args` 自由对象，全部校验在引擎；`start(meta)` 这条引擎能力没有暴露到工具面。