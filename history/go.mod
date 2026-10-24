module github.com/chonkpilot/chonkpilot-history-plugin

go 1.26.0

require (
	github.com/chonkpilot/chonkpilot-lib v0.0.0
	github.com/chonkpilot/chonkpilot-plugin v0.0.0
	github.com/chonkpilot/chonkpilot-plugin-history v0.0.0
	github.com/lsong98sh/chonkpilot-plugin-framework v0.0.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/modelcontextprotocol/go-sdk v1.7.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/lsong98sh/chonkpilot-plugin-framework => ../../chonkpilot-plugin-framework

replace github.com/chonkpilot/chonkpilot-lib => ../../chonkpilot/src/lib/core

replace github.com/chonkpilot/chonkpilot-plugin => ../../chonkpilot/src/plugins/plugin

replace github.com/chonkpilot/chonkpilot-plugin-history => ./lib
