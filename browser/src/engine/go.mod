module github.com/lsong98sh/chonkpilot-plugins/browser/engine

go 1.26.0

require (
	github.com/jchv/go-webview2 v0.0.0
	github.com/lsong98sh/chonkpilot-plugin-framework v0.1.1
	github.com/lsong98sh/chonkpilot-util v0.0.0
)

require (
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

// WebView2 绑定 = 独立 fork 仓 `chonkpilot-webview2`（唯一源；2026-10-30 删除插件仓
// third_party 内嵌副本）。该 fork 提供 EvalWithResult / Screenshot / DevTools / ScriptEval
// 等上游没有的能力；待该 fork 独立发版后改为按版本引用。
replace github.com/jchv/go-webview2 => ../../../../chonkpilot-webview2

// 开发期依赖策略（2026-10-29）：同机跨仓依赖一律本地 replace；正式发版时统一切回版本号并删除本节。
replace github.com/lsong98sh/chonkpilot-plugin-framework => ../../../../chonkpilot-framework/plugin/src

replace github.com/lsong98sh/chonkpilot-util => ../../../../chonkpilot-util
