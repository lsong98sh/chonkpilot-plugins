// chonkpilot-codegraph-module-summary-executor：codegraph_get_module_summary 单工具执行器。
package main

import (
	"os"

	"github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph/execx"
)

func main() { os.Exit(execx.Run("codegraph_get_module_summary")) }
