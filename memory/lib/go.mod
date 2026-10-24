module github.com/chonkpilot/chonkpilot-plugin-memory

go 1.26.0

require (
	github.com/chonkpilot/chonkpilot-data v0.0.0
	github.com/chonkpilot/chonkpilot-lib v0.0.0
	github.com/chonkpilot/chonkpilot-plugin v0.0.0
)

require (
	github.com/chonkpilot/chonkpilot-ignore v0.0.0 // indirect
	github.com/lsong98sh/chonkpilot-chat v0.1.0 // indirect
	github.com/lsong98sh/chonkpilot-session v0.1.0 // indirect
	go.etcd.io/bbolt v1.4.2 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/chonkpilot/chonkpilot-data => ../../../chonkpilot/src/lib/data

replace github.com/chonkpilot/chonkpilot-ignore => ../../../chonkpilot/src/lib/ignore

replace github.com/chonkpilot/chonkpilot-lib => ../../../chonkpilot/src/lib/core

replace github.com/chonkpilot/chonkpilot-plugin => ../../../chonkpilot/src/plugins/plugin
