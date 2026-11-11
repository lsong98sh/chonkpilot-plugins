module github.com/chonkpilot/chonkpilot-file-plugin

go 1.26.0

require (
	github.com/lsong98sh/chonkpilot-plugin-framework v0.1.1
	github.com/lsong98sh/chonkpilot-plugins/file/lib v0.0.0
	github.com/lsong98sh/chonkpilot-util v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/joshtechnologygroup/chardet v0.1.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/lsong98sh/chonkpilot-plugins/file/lib => ./lib

replace github.com/lsong98sh/chonkpilot-util => ../../../chonkpilot-util

// 开发期依赖策略（2026-10-29）：同机跨仓依赖一律本地 replace；正式发版时统一切回版本号并删除本节。
replace github.com/lsong98sh/chonkpilot-plugin-framework => ../../../chonkpilot-framework/plugin/src
