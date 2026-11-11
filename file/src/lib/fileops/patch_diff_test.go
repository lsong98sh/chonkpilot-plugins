package fileops

import (
	"strings"
	"testing"
)

func TestApplyFileDiffValidatesEveryContextLine(t *testing.T) {
	src := "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"
	patch := "@@ -2,3 +2,3 @@\n line2\n-line3\n+LINE3\n line4\n" +
		"@@ -8,3 +8,3 @@\n line8\n line9-broken\n-line10\n+LINE10\n"
	if _, err := ApplyFileDiff(src, patch); err == nil {
		t.Fatal("第二个 hunk 的后续 context 行不匹配，应整项失败")
	}
}

func TestApplyFileDiffAllowsPlusPlusAndMinusMinusContent(t *testing.T) {
	src := "a\nb\nc\n"
	patch := "@@ -1,3 +1,4 @@\n a\n+++i;\n b\n c\n"
	got, err := ApplyFileDiff(src, patch)
	if err != nil {
		t.Fatalf("添加以 ++ 开头的内容行应成功: %v", err)
	}
	if want := "a\n++i;\nb\nc\n"; got != want {
		t.Fatalf("新增行被吞/改错: got %q want %q", got, want)
	}

	src = "a\n--flag\nc\n"
	patch = "@@ -1,3 +1,2 @@\n a\n---flag\n c\n"
	got, err = ApplyFileDiff(src, patch)
	if err != nil {
		t.Fatalf("删除以 -- 开头的内容行应成功: %v", err)
	}
	if want := "a\nc\n"; got != want {
		t.Fatalf("删除行未被正确处理: got %q want %q", got, want)
	}
}

func TestApplyFileDiffCRLF(t *testing.T) {
	src := "a\r\nb\r\nc\r\n"
	patch := "@@ -1,3 +1,3 @@\n a\n-b\n+B\n c\n"
	got, err := ApplyFileDiff(src, patch)
	if err != nil {
		t.Fatalf("CRLF 源文件应能应用 LF 补丁: %v", err)
	}
	if want := "a\r\nB\r\nc\r\n"; got != want {
		t.Fatalf("CRLF 风格未保持: got %q want %q", got, want)
	}
}

func TestApplyFileDiffRejectsLineCountMismatch(t *testing.T) {
	cases := []string{
		"@@ -1,3 +1,2 @@\n a\n-b\n",           // 声明旧 3 行，实际只有 2 行
		"@@ -1,2 +1,3 @@\n a\n+b\n c\n",       // 声明新 3 行，实际 2 行
		"@@ -1,1 +1,1 @@\n a\n b\n",           // 多出 context 行
		"@@ -1,1 +1,1 @@\n not-a-diff-line\n", // hunk 内非法行
	}
	for i, patch := range cases {
		if _, err := ApplyFileDiff("a\nb\nc\n", patch); err == nil {
			t.Fatalf("case %d: 行数/语法不符应失败", i)
		}
	}
}

func TestApplyFileDiffRejectsOverlapAndMissingHunk(t *testing.T) {
	if _, err := ApplyFileDiff("a\nb\nc\n", "no header\n"); err == nil {
		t.Fatal("无 @@ hunk 应失败")
	}
	patch := "@@ -3,1 +3,1 @@\n-c\n+C\n@@ -2,1 +2,1 @@\n-b\n+B\n"
	if _, err := ApplyFileDiff("a\nb\nc\n", patch); err == nil {
		t.Fatal("hunk 起始行回退（重叠）应失败")
	}
}

func TestRenderFileDiffRoundTrip(t *testing.T) {
	cases := []struct{ left, right string }{
		{"a\nb\nc\n", "a\nB\nc\n"},
		{"one\ntwo\nthree\nfour\nfive\n", "one\nTWO\nthree\nfour\nFIVE\n"},
		{"", "first\nsecond\n"},
		{"old\n", ""},
	}
	for i, tc := range cases {
		diff, err := RenderFileDiff(tc.left, tc.right, "left.txt", "right.txt", DefaultDiffContext)
		if err != nil {
			t.Fatalf("case %d: RenderFileDiff: %v", i, err)
		}
		if tc.left == tc.right {
			continue
		}
		if !strings.HasPrefix(diff, "--- left.txt\n+++ right.txt\n@@ ") {
			t.Fatalf("case %d: 非标准 unified diff 头:\n%s", i, diff)
		}
		got, err := ApplyFileDiff(tc.left, diff)
		if err != nil {
			t.Fatalf("case %d: ApplyFileDiff(%q): %v\n%s", i, diff, err, diff)
		}
		if got != tc.right {
			t.Fatalf("case %d: 往返不一致: got %q want %q\n%s", i, got, tc.right, diff)
		}
	}
}

func TestRenderFileDiffContext(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = "line" + string(rune('A'+i%26))
	}
	left := strings.Join(lines, "\n") + "\n"
	lines[2] = "CHANGED-A"
	lines[30] = "CHANGED-B"
	right := strings.Join(lines, "\n") + "\n"

	for _, context := range []int{0, 3, 40} {
		diff, err := RenderFileDiff(left, right, "a", "b", context)
		if err != nil {
			t.Fatalf("context=%d: %v", context, err)
		}
		hunks := strings.Count(diff, "@@ -")
		want := 2
		if context == 40 {
			want = 1
		}
		if hunks != want {
			t.Fatalf("context=%d: hunks=%d want %d\n%s", context, hunks, want, diff)
		}
		got, err := ApplyFileDiff(left, diff)
		if err != nil {
			t.Fatalf("context=%d: apply: %v", context, err)
		}
		if got != right {
			t.Fatalf("context=%d: 往返不一致", context)
		}
		if context == 0 && strings.Contains(diff, "\n line") {
			t.Fatalf("context=0 不应输出上下文行:\n%s", diff)
		}
	}
}
