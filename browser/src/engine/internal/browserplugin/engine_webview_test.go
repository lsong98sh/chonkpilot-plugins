package browserplugin

import (
	"strings"
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk/enginehost"
)

// TestDebugOpenRequiresDebug 未开 debug 时不得谎报 DevTools 已打开。
func TestDebugOpenRequiresDebug(t *testing.T) {
	e := &webviewEngine{opts: enginehost.ContextOptions{Debug: false}}
	if _, err := e.Do("debug_open", nil); err == nil || !strings.Contains(err.Error(), "devtools disabled") {
		t.Fatalf("debug=false 应拒绝 debug_open: %v", err)
	}
}
