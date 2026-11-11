# browser 工具审查索引

审查范围：manifest 插件 `browser`，唯一 LLM 工具 `browser_run`（程序化注册，无 `.tool.md`）。分配清单与实际一致（1 个工具）。

| 工具 | 结论 | P0 | P1 | P2 | 文档 |
|---|---|---|---|---|---|
| browser_run | 有风险，不建议当前形态（eval 参数名不符、screenshot 绕过沙箱、context 跨实例可劫持） | 3 | 5 | 8 | [browser_run.md](browser_run.md) |
| **小计** | | **3** | **5** | **8** | |

## 核对结果

- `src/initdata/capability/plugins/browser/manifest.json:11,20-24`：`mcp.hot_tools=["browser_run"]`、`timeout_sec=180`、`namespace="-"`、`origin="builtin"`、`runtime.exe=chonkpilot-plugin-browser.exe`；与主控事实一致。
- 会话实现在独立引擎 `chonkpilot-browser-plugin.exe`（`src/engine/main.go:40-54`），插件经 `engineclient` 转发；`dist/plugins/browser/` 同时存在两个 exe，`dist` manifest 与源码 manifest 内容一致（仅 CRLF 差异），无产物漂移。
- 工具面由 `browserSchema()` 代码构造（`src/main.go:82-93`），**schema 以代码为准**；无 `.tool.md`、无 `contract.Register`。
- `hot_tools` 只有 `browser_run`；SDK 会把它自身注册的**全部**工具名写入 `hot_tools`（`chonkpilot-framework/plugin/src/sdk/plugin.go:558`），本插件只有一个工具，暂无影响；若后续加工具，`HotTool()` 的标记会被忽略。
- 配置：`config.scope=prjusr`，实际读写 data 层 `plugins.browser.*`（`src/config.go:33-40,164-204`），默认 headless=true / 1280×800。

## 跨工具发现

1. **引擎/插件两层无沙箱契约**：`screenshot` 是唯一落盘动作，插件从不读 `_meta.chonkpilot.sandbox`；与 file 插件的写工具集合不同，不能照搬 file 的结论（`src/main.go:139-149`、`src/engine/internal/browserplugin/engine_webview.go:238-243`）。
2. **实例→context 映射只在插件内存**：引擎 janitor（10min）或引擎崩溃重启后映射不失效，也没有重建逻辑（`chonkpilot-framework/plugin/src/sdk/enginehost/server.go:340-375`）。
3. **超时语义**：工具无 `_meta.async`，gateway 缺省 `auto` + manifest 180s 阈值；到点转后台而非重跑（`chonkpilot-agent/gateway/src/mcpgateway.go:933-958,1095-1102`）。引擎客户端另有 120s 单次调用上限（`.../sdk/engineclient/client.go:70-75`），可能先于网关阈值返回 `i/o timeout`。

## 范围差异

- 分配清单只要求 `browser_run`，已全覆盖；未发现虚拟/隐藏工具。
- 未执行 build/test/插件进程；未启动浏览器引擎。所有结论基于源码（`src/`、`chonkpilot-framework`、`chonkpilot-agent/gateway`、主仓 manifest）。