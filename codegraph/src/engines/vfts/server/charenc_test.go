// 编码识别与 UTF-8 转码单测（对齐 docs/spec/20-modules/2B-vfts.md §4.1）：
//   - UTF-8 / UTF-8 BOM / UTF-16 LE-BE BOM → 正确解码（BOM 不入文本）；
//   - GBK/GB18030 中文文本 → 识别为 gb18030 并转 UTF-8；
//   - SJIS / Big5 / EUC-KR → 仅在 `encodingx.BestAuto` **高置信度**时采纳（D-73）；
//     纯汉字等歧义文本仍不猜测，保持原字节（避免造出假中文）。
package server

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

func TestDecodeIndexText(t *testing.T) {
	gbk := func(s string) []byte {
		b, _, err := transform.Bytes(simplifiedchinese.GBK.NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("gbk encode: %v", err)
		}
		return b
	}
	utf16le := func(s string) []byte {
		b, _, err := transform.Bytes(unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("utf16le encode: %v", err)
		}
		return b
	}
	utf16be := func(s string) []byte {
		b, _, err := transform.Bytes(unicode.UTF16(unicode.BigEndian, unicode.UseBOM).NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("utf16be encode: %v", err)
		}
		return b
	}
	sjis := func(s string) []byte {
		b, _, err := transform.Bytes(japanese.ShiftJIS.NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("sjis encode: %v", err)
		}
		return b
	}
	big5 := func(s string) []byte {
		b, _, err := transform.Bytes(traditionalchinese.Big5.NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("big5 encode: %v", err)
		}
		return b
	}
	euckr := func(s string) []byte {
		b, _, err := transform.Bytes(korean.EUCKR.NewEncoder(), []byte(s))
		if err != nil {
			t.Fatalf("euckr encode: %v", err)
		}
		return b
	}

	cnText := "这是一个用于测试编码识别的中文文本文件，包含常见的汉字与技术词汇，例如索引、分词、检索和配置文件。"
	cases := []struct {
		name string
		data []byte
		want string
		enc  string
	}{
		{"UTF-8 原样", []byte(cnText), cnText, "utf-8"},
		{"UTF-8 BOM 剥离", append([]byte{0xEF, 0xBB, 0xBF}, []byte(cnText)...), cnText, "utf-8"},
		{"UTF-16LE BOM", utf16le(cnText), cnText, "utf-16le"},
		{"UTF-16BE BOM", utf16be(cnText), cnText, "utf-16be"},
		{"GBK 中文", gbk(cnText), cnText, "gb18030"},
		{"GBK 代码注释", gbk("// 初始化数据库连接，读取配置文件并启动服务\n"), "// 初始化数据库连接，读取配置文件并启动服务\n", "gb18030"},
		{"SJIS 高置信识别", sjis("日本語のテストです、カタカナも含みます。"), "日本語のテストです、カタカナも含みます。", "shift_jis"},
		{"Big5 高置信识别", big5("繁體中文測試，資料檢索與檔案設定。"), "繁體中文測試，資料檢索與檔案設定。", "big5"},
		{"EUC-KR 高置信识别", euckr("한국어 테스트입니다. 검색과 설정을 포함합니다."), "한국어 테스트입니다. 검색과 설정을 포함합니다.", "euc-kr"},
		{"SJIS 纯汉字歧义不猜测", sjis("日本語文章検索設定変更"), string(sjis("日本語文章検索設定変更")), "utf-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, enc := decodeIndexText(c.data, "")
			if enc != c.enc {
				t.Fatalf("识别编码 = %q，期望 %q（文本 %.40q）", enc, c.enc, got)
			}
			if got != c.want {
				t.Fatalf("解码文本 = %.60q，期望 %.60q", got, c.want)
			}
			if strings.HasPrefix(got, "\uFEFF") {
				t.Fatalf("文本不应保留 BOM：%.40q", got)
			}
		})
	}

	// 项目级显式配置：歧义文本也按指定编码解码（确定性优先）。
	pureKanji := sjis("日本語文章検索設定変更")
	if got, enc := decodeIndexText(pureKanji, "shift_jis"); enc != "shift_jis" || got != "日本語文章検索設定変更" {
		t.Fatalf("forced shift_jis: got=(%.40q, %q)", got, enc)
	}
	if got, enc := decodeIndexText([]byte("plain utf8 文本"), "shift_jis"); enc != "utf-8" || got != "plain utf8 文本" {
		t.Fatalf("合法 UTF-8 不应被 forced 覆盖：got=(%.40q, %q)", got, enc)
	}
}

func TestNormalizeEncodingName(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"auto":    "",
		"AUTO":    "",
		"UTF8":    "utf-8",
		"sjis":    "shift_jis",
		"CP950":   "big5",
		"euc_kr":  "euc-kr",
		"latin-1": "",
	}
	for in, want := range cases {
		if got := normalizeEncodingName(in); got != want {
			t.Fatalf("normalizeEncodingName(%q) = %q，期望 %q", in, got, want)
		}
	}
}
