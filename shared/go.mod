module github.com/lsong98sh/chonkpilot-plugins/shared

go 1.26.0

require (
	github.com/lsong98sh/chonkpilot-plugin-framework v0.1.1
	github.com/lsong98sh/chonkpilot-util v0.0.0
	golang.org/x/sys v0.41.0
)

// 开发期依赖策略（2026-10-29）：同机跨仓依赖一律本地 replace；正式发版时统一切回版本号并删除本节。
replace github.com/lsong98sh/chonkpilot-plugin-framework => ../../chonkpilot-framework/plugin/src

replace github.com/lsong98sh/chonkpilot-util => ../../chonkpilot-util
