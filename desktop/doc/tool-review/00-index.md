# desktop 工具审查索引

审查范围：manifest 插件 `desktop`，唯一 LLM 工具 `desktop_run`（程序化注册，无 `.tool.md`）。分配清单与实际一致（1 个工具）。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| desktop_run | 不建议当前形态（screenshot 无目标打死引擎、目标窗口必填未声明、move/size 破坏窗口、绕过沙箱） | 4 | 5 | 8 | [desktop_run.md](desktop_run.md) |
| **小计** | | **4** | **5** | **8** | |

## 核对结果

- `src/initdata/capability/plugins/desktop/manifest.json:11,20-24`：`mcp.hot_tools=["desktop_run"]`、`timeout_sec=180`、`namespace="-"`、`origin="builtin"`、`runtime.exe=chonkpilot-plugin-desktop.exe`；与主控事实一致。
- 会话实现在独立引擎 `chonkpilot-desktop-plugin.exe`（`src/engine/main.go:37-54`），插件经 `engineclient` 复用 browser 同款桥；`dist/plugins/desktop/` 两个 exe 均在，`dist` manifest 与源码内容一致（仅 CRLF 差异），无产物漂移。
- 工具面由 `desktopSchema()` 代码构造（`src/main.go:82-93`），**schema 以代码为准**。
- 配置只暴露 `plugins.desktop.screenshot-dir`（`src/config.go:33-48`）；桌面引擎无 profile/console 概念。
- `hot_tools` 只有 `desktop_run`；SDK 会把所有自注册工具写进 `hot_tools`（`.../sdk/plugin.go:558`），单工具场景无影响。

## 跨工具发现

1. **两个 P0 来自引擎内部而非桥接层**：`screenshot` 无目标窗口的 nil 解引用（`src/engine/internal/desktop/capture_export.go:13-19` + `screen.go:105-110`，全仓无 `recover()`）会打死引擎；`move`/`size` 的 `SetWindowPos` 标志缺失（`desktopplugin/win32.go:66-75`）会破坏用户窗口。
2. **enginehost 的 context 语义与 browser 相同**：MaxContexts=8、IdleTimeout=10min、插件侧映射不失效（`.../sdk/enginehost/server.go:73-79,340-375`）；desktop 的 `Close()` 是空操作，stop 只释放池项（`engine.go:206-207`）。
3. **沙箱**：`screenshot` 是唯一落盘动作，但插件不读 `_meta.chonkpilot.sandbox`（`src/main.go:139-153`），与 file 插件写工具集合不同，不能照搬 file 结论。
4. **`start` 的 meta 绑定能力存在但不可达**：引擎 `New` 支持 `meta.title/class/hwnd`（`engine.go:32-42`），插件 `start` 只发 `{purpose}`（`src/main.go:156`），schema 也没有对应字段。

## 范围差异

- 分配清单只要求 `desktop_run`，已全覆盖；未发现虚拟/隐藏工具。
- 未执行 build/test/插件进程；未启动桌面引擎、未注入输入。所有结论基于源码与 gateway 代码。