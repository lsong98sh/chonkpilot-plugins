// Package indexkind 是 resident 索引引擎的 consumer 标识（index_queue 扇出维度）。
package indexkind

const (
	VFTS      = "vfts"
	Codegraph = "codegraph"
)

// All 是当前驻留索引引擎列表（filesys 入队按此扇出）。
var All = []string{VFTS, Codegraph}
