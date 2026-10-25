# go-webview2 内嵌 fork 台账（D-46）

> 本目录是 **jchv/go-webview2 的内嵌 fork**（非 `vendor/`、非 git submodule），随源码入库。
> 本文记录**来源 / 版本 / 本地补丁清单 / 升级路径**，补齐此前无台账的缺口（供应链视角⑯）。

## 1. 来源

| 项 | 值 |
|----|----|
| 上游仓库 | `https://github.com/jchv/go-webview2` |
| module path | `github.com/jchv/go-webview2`（**保持与上游同名**，靠各消费方 `go.mod` 的 `replace` 指向本内嵌目录） |
| 引入方式 | 内嵌快照（文件直接入库），无 submodule / 无 vendor |
| 消费方 `replace` | `src/lib/gui/go.mod` · `src/gui/go.mod` · `src/desktop/go.mod`（`=> ../lib/go-webview2`） · `src/test/chonkpilot-gui/unittest/go.mod`（`=> ../../../lib/go-webview2`） |

## 2. 版本（基线）

- **基准 commit：`56598839c808a2340edee99204db479f410e9bf4`**（上游 `master` HEAD，2026-02-05，提交信息
  “Fix Windows 32-bit support for GetWindowLong/SetWindowLong functions (#81)”）——**内嵌快照 = 上游最新提交**。
  来源：[jchv/go-webview2@5659883](https://github.com/jchv/go-webview2/commit/56598839c808a2340edee99204db479f410e9bf4)。
- 上游**无 tag / release**（仓库 `/tags` 为空）；pkg.go.dev 仅有伪版本 `v0.0.0-...-5659883`（2026-02-05 发布）。
- `go.mod`（与上游 HEAD **逐字节一致**）：`go 1.16`；`github.com/jchv/go-winloader
  v0.0.0-20250406163304-c1995be93bd1`（2025-04-06，随上游提交 `0bcfea01` “Update winloader” 引入）、
  `golang.org/x/sys v0.0.0-20210218145245-beda7e5e158e`（2021-02-18）。**注意**：上游自 2021 起未再升级 `go` 指令与
  `x/sys`，故此二者**不能**用于年份推定（此前按 “go 1.16 + x/sys 2021-02” 猜测为 2021 年是**误判**）。
- **判定依据（比对过的候选与差异点）**：
  1. `internal/w32/w32_386.go` / `w32_64bit.go`：上游**仅**在 `5659883` 新增（该提交 diff 中此二文件为 `added`），
     fork 两份内容逐字一致 → 基线 ≥ `5659883`。
  2. `internal/w32/w32.go` var 块含 `User32GetWindowLongW` / `User32SetWindowLongW`（`5659883` 新增，位置/顺序与上游一致）。
  3. `webview.go` `SetSize` 使用 `w32.GetWindowLong` / `w32.SetWindowLong`（`5659883` 的重构写法）。
  4. fork 文件清单 = 上游 `master` tree（63 项，含 `webviewloader/sdk/{x64,x86,arm64}/WebView2Loader.dll`）；唯一本地新增为 `FORK.md`。
  5. 上游提交历史自 `5659883` 起无更新（GitHub commits API 最新一页首条即 `5659883`）→ 基线 = HEAD，**可确证**。
- 内嵌 `webviewloader/sdk/{x64,x86,arm64}/WebView2Loader.dll` 三份（`//go:embed`，随源码入库，见 `.gitignore` 负模式例外）。

## 3. 本地补丁清单（相对上游的改动）

> 基线已固定为 §2 的 `5659883`；下列为**本仓已识别**的本地改动；均带中文注释、可据注释与 `D-xx` 标记定位。

| # | 类别 | 位置 | 说明 |
|---|------|------|------|
| P1 | 生命周期 | `webview.go`（`windowContext` / `delWindowContext` / `wndproc` WM_DESTROY） | 窗口销毁（WM_DESTROY）清理 `windowContext` 条目，避免多窗口反复开关时 `*webview` 永久泄漏（**D-28**）；WM_NCCREATE 期间登记 `*webview`（使首个 WM_NCCALCSIZE 即按 frameless 处理）。 |
| P2 | 窗口样式 | `webview.go`（`WebViewOptions.Frameless` / `wndproc` WM_NCCALCSIZE / `CreateWithOptions`） | 无边框窗口支持：WM_NCCALCSIZE 返回 0 隐藏原生标题栏/边框；保留 `WS_OVERLAPPEDWINDOW` 使原生 resize/最大化/贴靠保持；最大化时把客户区钳到监视器工作区。 |
| P3 | 窗口样式 | `webview.go`（`WebViewOptions.Hidden` / `Show` / `WebViewLifecycle`） | 建窗后可先隐藏，导航完成后 `Show()`，避免首屏白闪；`WebViewLifecycle.SetNavigationCompletedCallback` 供宿主挂导航完成回调。 |
| P4 | 窗口样式 | `webview.go`（`WindowResizer` / `StartResize`） | 前端命中方位后调用 `StartResize(HT*)` 进入系统原生 **resize** 模态循环；同一入口传 `HTCAPTION(2)` 即进入系统**移动**循环（frameless 主窗口顶栏拖拽，G-63）。 |
| P5 | DevTools | `webview.go`（`WebViewOptions.DevToolsDisabled` / `DevTools` / `OpenDevToolsWindow`） | 屏蔽用户 F12/右键菜单开 DevTools，同时保留宿主机**程序化**打开能力（`DevTools.OpenDevToolsWindow`）。 |
| P6 | 加速键 | `webview.go`（`Accelerator` / `SetAcceleratorKeyCallback`） | 拦截加速键（如 F12），返回 true 表示吞掉、false 交 WebView2 处理。 |
| P7 | 脚本 | `webview.go`（`ScriptEval` / `EvalWithResult`） | 同步执行 JS 并取回 JSON 结果（阻塞至完成回调或超时）；**不可在 UI 线程调用**（会死锁）。 |
| P8 | 截图 | `webview.go`（`Screenshot`） | 经 `CapturePreview` 取页面 PNG 字节（阻塞至完成或超时）；**不可在 UI 线程调用**。 |
| P9 | 健壮性 | `webview.go`（`NewWithOptions`） | Settings 获取/设置失败由 `log.Fatal`（整进程退出）改为**记日志 + 返回 nil**（走既有单窗口失败收敛路径，**D-36**）。 |
| P10 | 配置 | `webview.go`（`WebViewOptions.DataPath` / `WindowOptions`） | 支持注入 WebView2 用户数据目录（多窗口 profile 隔离）与窗口标题/尺寸/图标/居中。 |
| P11 | 布局 | `webview.go`（`wndproc` WM_MOVE / WM_MOVING） | 窗口移动时调用 `NotifyParentWindowPositionChanged`，使 WebView2 控件跟随。 |
| P12 | 权限 | `webview.go`（`NewWithOptions`） | 授予 `ClipboardRead` 权限（富文本粘贴场景）。 |
| P13 | 供应链 | `webviewloader/` | `//go:embed` 内嵌三架构 `WebView2Loader.dll`，运行期磁盘加载失败时回退内存加载（`module.go`）。 |
| P14 | 设置接口 | `pkg/edge/ICoreWebViewSettings.go`（`GetSettings9`） | `ICoreWebView2Settings9`（`IsNonClientRegionSupportEnabled`）必须经 **`QueryInterface`**（IID `{0528a73b-e92d-49f4-927a-e547dddaa37d}`）取接口指针后再调用；经基接口 vtable 直调扩展槽位会「返回成功但不生效」（G-63）。 |

> 说明：`pkg/edge/*`、`internal/w32/*` 为上游 WebView2 COM 绑定层；本地改动集中在 `webview.go` 的
> 窗口/生命周期/能力接口，`pkg/edge` 侧仅在需要支撑上述能力处有配套扩展（如 `CapturePreview`、
> `ExecuteScriptWithResult`、`Show`、`AcceleratorKeyCallback`）。

> **本地移除（相对上游）**：本 fork **已移除** `Bind` / `MessageReceived` / `MessageCallback` 及
> `window.external` **注入链路**（上游的「宿主 ↔ JS 双向绑定 / 消息回调」入口）。本仓宿主 → 前端的
> **事件透传仅经 `Eval` → `window.mq.emitRemote`**（单向投递，见 [11-MQ与消息](../../docs/spec/10-architecture/11-MQ与消息.md) ·
> [20-gui](../../docs/spec/20-modules/20-gui.md) §12），**不再经 `window.external` 注入**。`webview.go`
> 保留 `Init(js)`（= `AddScriptToExecuteOnDocumentCreated`，注入前端桥 shim）与 `Eval` / `EvalWithResult`；
> `pkg/edge` 的 COM 级 `MessageReceived` 绑定保留（属上游 COM 层，本仓未使用）。

## 4. 升级路径

1. 取上游目标版本：以 §2 记录的基准 commit `5659883` 为锚，`git -C <upstream-clone> log` 增量前进；上游无 tag/release，最新即 `master` HEAD。
2. **diff 归因**：`diff -ru <upstream> src/lib/go-webview2` 得到全部差异 → 对照第 3 节清单，区分
   「上游已修（可丢弃本地补丁）」与「本地仍需（须重新套用）」。
3. 重新套用本地补丁（P1~P13），保留中文注释与 `D-xx` 标记；同步更新 `webviewloader/sdk/*` DLL（如需）。
4. 构建/回归：`go build ./...`（本模块，Windows/CGO）；消费侧（`src/lib/gui`、`src/desktop`、`src/gui`、
   `src/test/chonkpilot-gui/unittest`）跑各自 `go build ./...` 与 GUI 单测；`build-desktop.ps1` 产物验证。
5. 更新本台账：修订第 2 节基准 commit 与第 3 节补丁清单。

## 5. 维护约定

- **不在本目录引入新依赖**（保持与上游一致，降低升级冲突面）。
- 新增本地补丁须**同时**在此台账第 3 节登记（编号 + 位置 + 说明），并在代码处以中文注释 + `D-xx` 标注。
- 命名空间/API 尽量与上游兼容；能力扩展优先以**独立接口**（如 `ScriptEval`/`Screenshot`）叠加，
  而非改动 `WebView` 基础接口，便于升级时保留。
