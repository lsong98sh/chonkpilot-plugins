module github.com/lsong98sh/chonkpilot-plugins/browser/engine

go 1.26.0

require (
	github.com/jchv/go-webview2 v0.0.0
	github.com/lsong98sh/chonkpilot-plugins/shared v0.0.0
)

require (
	github.com/jchv/go-winloader v0.0.0-20250406163304-c1995be93bd1 // indirect
	golang.org/x/sys v0.41.0 // indirect
)

replace github.com/lsong98sh/chonkpilot-plugins/shared => ../../shared

// WebView2 绑定 = 主仓 fork 的副本（third_party/go-webview2）。该 fork 提供
// EvalWithResult / Screenshot / DevTools / ScriptEval 等上游没有的能力；副本与主仓
// `src/lib/go-webview2` 保持同步，待该 fork 独立发版后改为按版本引用。
replace github.com/jchv/go-webview2 => ../../third_party/go-webview2
