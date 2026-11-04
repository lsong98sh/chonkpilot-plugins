package main

import "testing"

func TestHTMLToText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<b>该喝水了</b><br>顺便站起来走两步", "该喝水了\n顺便站起来走两步"},
		{"<p>第一段</p><p>第二段</p>", "第一段\n第二段"},
		{"a&nbsp;&amp;&nbsp;b", "a & b"},
		{"<div>  x   y </div>", "x y"},
		{"", ""},
		{"<div", "<div"},
	}
	for _, tc := range cases {
		if got := HTMLToText(tc.in); got != tc.want {
			t.Errorf("HTMLToText(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}
