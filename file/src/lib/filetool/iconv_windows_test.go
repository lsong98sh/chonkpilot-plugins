//go:build windows

package filetool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
)

// TestIconvFileInUse：目标被其他应用打开（os.Open 持有）时，批调度重试 3 轮后
// failed + code=FILE_IN_USE，内容不变；关闭句柄后可成功转换。
func TestIconvFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "inuse.txt")
	content := "中文内容测试"
	mustWrite(t, path, []byte(content))
	holder, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	targets := []any{map[string]any{"path": path, "from": "utf-8", "to": "utf-16le"}}
	out, _ := runTool(t, "file_iconv", map[string]any{"targets": targets})
	r := firstResult(t, out)
	if r["status"] != statusFailed || r["code"] != "FILE_IN_USE" {
		t.Fatalf("want failed FILE_IN_USE while held open: %v", r)
	}
	if got := mustRead(t, path); string(got) != content {
		t.Fatalf("file mutated while held open: %q", got)
	}

	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	out, _ = runTool(t, "file_iconv", map[string]any{"targets": targets})
	if r := firstResult(t, out); r["status"] != statusOK {
		t.Fatalf("iconv after close want ok: %v", r)
	}
	decoded, derr := encodingx.Decode("utf-16le", mustRead(t, path))
	if derr != nil {
		t.Fatal(derr)
	}
	if decoded != content {
		t.Fatalf("decoded=%q want %q", decoded, content)
	}
}
