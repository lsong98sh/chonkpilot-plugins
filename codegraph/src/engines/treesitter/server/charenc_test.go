// 编码归一单测（对齐 docs/spec/20-modules/29-codegraph.md §索引）：
// GBK / UTF-16(BOM) / UTF-8(BOM) 源码在交给 tree-sitter 前统一转 UTF-8，
// 中文标识符与注释不再因非法 UTF-8 丢符号（解码实现 = chonkpilot-util/encodingx）。
package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

func TestIndexSource(t *testing.T) {
	cn := "package main\n\n// 中文注释\nfunc 中文函数() {}\n"
	gbk := func(s string) []byte {
		b, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	u16 := func(s string) []byte {
		b, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder(), []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	if got := string(indexSource([]byte(cn))); got != cn {
		t.Fatalf("UTF-8 应原样：%.40q", got)
	}
	if got := string(indexSource(append([]byte{0xEF, 0xBB, 0xBF}, []byte(cn)...))); got != cn {
		t.Fatalf("UTF-8 BOM 应剥离：%.40q", got)
	}
	if got := string(indexSource(gbk(cn))); got != cn {
		t.Fatalf("GBK 应转 UTF-8：%.40q", got)
	}
	if got := string(indexSource(u16(cn))); got != cn {
		t.Fatalf("UTF-16LE 应转 UTF-8：%.40q", got)
	}
	if encodingx.BOMName([]byte(cn)) != "" {
		t.Fatal("测试夹具不应带 BOM")
	}
}

// TestIndexNonUTF8Source：真索引链路——非 UTF-8 源码经 parseEntry 归一后应能解析出中文标识符。
func TestIndexNonUTF8Source(t *testing.T) {
	src := "package main\n\n// 中文注释：编码归一\nfunc 中文函数() {}\n"
	gbkBytes, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	u16Bytes, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder(), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	utf8BOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(src)...)

	cases := []struct {
		name string
		data []byte
	}{
		{"utf-8", []byte(src)},
		{"utf-8-bom", utf8BOM},
		{"gbk", gbkBytes},
		{"utf-16le", u16Bytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "a.go"), c.data, 0o644); err != nil {
				t.Fatal(err)
			}
			w, err := Open(dir)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer dropWorkspace(w.Dir)
			if err := w.Initialize(nil, nil, nil); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			found := false
			for _, s := range w.ix.AllSymbols() {
				if s.Name == "中文函数" {
					found = true
				}
			}
			if !found {
				t.Fatalf("应解析出中文标识符「中文函数」，实际符号：%v", w.ix.AllSymbols())
			}
			if fi, ok := w.ix.Files["a.go"]; ok && fi.HasErr {
				t.Fatalf("归一后不应有语法错误节点：%+v", fi)
			}
		})
	}
}
