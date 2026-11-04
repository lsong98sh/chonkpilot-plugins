// D-50（2026-10-31）：`instanceForWorkdir` 多用户选择口径。
package history

import (
	"testing"

	"github.com/lsong98sh/chonkpilot-plugin-framework/runtime/instance"
)

func TestPickInstanceForWorkdirPrefersSameUser(t *testing.T) {
	wd := `E:\ws\p1`
	list := []instance.Info{
		{ID: "a-other", WorkDir: wd, User: "alice"},
		{ID: "b-mine", WorkDir: wd, User: "bob"},
		{ID: "c-mine", WorkDir: wd, User: "bob"},
		{ID: "d-else", WorkDir: `E:\ws\p2`, User: "bob"},
	}
	if got := pickInstanceForWorkdir(list, wd, "bob"); got != "b-mine" {
		t.Fatalf("同用户应优先且取 id 最小：got %q", got)
	}
	// 无同用户 → 回落任一（id 最小），保持引入前行为。
	if got := pickInstanceForWorkdir(list, wd, "carol"); got != "a-other" {
		t.Fatalf("无同用户应回落任一实例：got %q", got)
	}
	// 本机用户未知（""）→ 不按用户过滤。
	if got := pickInstanceForWorkdir(list, wd, ""); got != "a-other" {
		t.Fatalf("未知用户应回落任一实例：got %q", got)
	}
	// 无匹配 workdir → 空串。
	if got := pickInstanceForWorkdir(list, `E:\ws\none`, "bob"); got != "" {
		t.Fatalf("无匹配应返回空串：got %q", got)
	}
}
