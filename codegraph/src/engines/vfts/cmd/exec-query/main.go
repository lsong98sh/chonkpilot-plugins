// chonkpilot-vfts-query-executor：vfts_query 单工具执行器。
package main

import (
	"os"

	"github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/vfts/execx"
)

func main() { os.Exit(execx.Run()) }
