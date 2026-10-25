module github.com/chonkpilot/chonkpilot-codebase-plugin

go 1.26.0

require (
	github.com/lsong98sh/chonkpilot-plugin-framework v0.1.1
	github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph v0.0.0
	github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.7.0
)

require (
	github.com/ebitengine/purego v0.10.1 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/lsong98sh/chonkpilot-plugins/shared v0.0.0 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/tree-sitter/go-tree-sitter v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-go v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-java v0.23.5 // indirect
	github.com/tree-sitter/tree-sitter-javascript v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-python v0.25.0 // indirect
	github.com/tree-sitter/tree-sitter-rust v0.24.2 // indirect
	github.com/tree-sitter/tree-sitter-typescript v0.23.2 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/zvec-ai/zvec-go v0.7.0 // indirect
	go.etcd.io/bbolt v1.4.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/lsong98sh/chonkpilot-plugins/shared => ../shared

replace github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph => ./engines/codegraph

replace github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts => ./engines/vfts
