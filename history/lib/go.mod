module github.com/chonkpilot/chonkpilot-plugin-history

go 1.26.0

require (
	github.com/chonkpilot/chonkpilot-lib v0.0.0
	github.com/chonkpilot/chonkpilot-plugin v0.0.0
)

replace github.com/chonkpilot/chonkpilot-lib => ../../../chonkpilot/src/lib/core

replace github.com/chonkpilot/chonkpilot-plugin => ../../../chonkpilot/src/plugins/plugin
