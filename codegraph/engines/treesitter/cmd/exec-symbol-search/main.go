// chonkpilot-codegraph-symbol-search-executor：codegraph_symbol_search 单工具执行器。
package main

import (
	"os"

	"github.com/lsong98sh/chonkpilot-plugins/codegraph/engines/treesitter/execx"
)

func main() { os.Exit(execx.Run("codegraph_symbol_search")) }
