// chonkpilot-codegraph-dependency-graph-executor：codegraph_get_dependency_graph 单工具执行器。
package main

import (
	"os"

	"github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/treesitter/execx"
)

func main() { os.Exit(execx.Run("codegraph_get_dependency_graph")) }
