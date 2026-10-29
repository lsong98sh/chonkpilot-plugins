module github.com/lsong98sh/chonkpilot-plugins/desktop/engine

go 1.26.0

require (
	github.com/lsong98sh/chonkpilot-plugins/shared v0.0.0
	golang.org/x/sys v0.49.0
)

replace github.com/lsong98sh/chonkpilot-plugins/shared => ../../../shared

// 开发期依赖策略（2026-10-29）：同机跨仓依赖一律本地 replace；正式发版时统一切回版本号并删除本节。
replace github.com/lsong98sh/chonkpilot-plugin-framework => ../../../../chonkpilot-framework/plugin/src

replace github.com/lsong98sh/chonkpilot-util => ../../../../chonkpilot-util
