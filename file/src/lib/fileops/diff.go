package fileops

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
)

// DefaultDiffContext 是 unified diff 的默认上下文行数。
const DefaultDiffContext = 3

// diffOp 是行级 diff 的一行操作：kind ∈ {' ', '-', '+'}；line 保留原始行尾换行。
type diffOp struct {
	kind byte
	line string
}

// RenderFileDiff 对比两段文本并返回**标准 unified diff**（供 file_diff / file_replace 复用）。
//
// 输出形如：
//
//	--- left
//	+++ right
//	@@ -1,3 +1,3 @@
//	 context
//	-old
//	+new
//
// context 控制每个 hunk 前后保留的上下文行数（< 0 按 0 处理）；相邻修改间隔不超过
// 2*context 时合并到同一 hunk。输出可被 ApplyFileDiff 直接消费（往返一致）。
func RenderFileDiff(a, b, leftLabel, rightLabel string, context int) (string, error) {
	if context < 0 {
		context = 0
	}
	if a == b {
		return "", nil
	}
	return renderUnifiedDiff(a, b, leftLabel, rightLabel, context), nil
}

// renderUnifiedDiff 把行级 diff 组织成带 @@ 头的 hunk。
func renderUnifiedDiff(a, b, leftLabel, rightLabel string, context int) string {
	ops := lineOps(a, b)
	// 为每个操作计算它在旧/新文件中的起始行号（1-based），不消费旧行时记录“下一条旧行号”。
	type pos struct {
		oldNo, newNo int
	}
	positions := make([]pos, len(ops))
	oldNo, newNo := 1, 1
	for i, op := range ops {
		positions[i] = pos{oldNo: oldNo, newNo: newNo}
		switch op.kind {
		case ' ':
			oldNo++
			newNo++
		case '-':
			oldNo++
		case '+':
			newNo++
		}
	}

	// 收集需要输出的区间：以每个变更行为锚点，前后各扩 context 行；间隔 ≤ 2*context 则合并。
	var ranges [][2]int
	lastChange := -1
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		if lastChange >= 0 && i-lastChange-1 <= 2*context {
			ranges[len(ranges)-1][1] = i
		} else {
			ranges = append(ranges, [2]int{i, i})
		}
		lastChange = i
	}
	if len(ranges) == 0 {
		return ""
	}

	var bd strings.Builder
	fmt.Fprintf(&bd, "--- %s\n+++ %s\n", leftLabel, rightLabel)
	for _, r := range ranges {
		start := r[0] - context
		if start < 0 {
			start = 0
		}
		end := r[1] + context + 1
		if end > len(ops) {
			end = len(ops)
		}
		oldCount, newCount := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				oldCount++
			}
			if op.kind != '-' {
				newCount++
			}
		}
		oldStart := positions[start].oldNo - 1
		if oldCount > 0 {
			oldStart++
		}
		newStart := positions[start].newNo - 1
		if newCount > 0 {
			newStart++
		}
		fmt.Fprintf(&bd, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
		for _, op := range ops[start:end] {
			line := op.line
			if !strings.HasSuffix(line, "\n") {
				line += "\n"
			}
			bd.WriteByte(op.kind)
			bd.WriteString(line)
		}
	}
	return bd.String()
}

// lineOps 用 diffmatchpatch 的“行转字符”模式得到行级 diff，再展开成逐行操作。
func lineOps(a, b string) []diffOp {
	dmp := diffmatchpatch.New()
	chars1, chars2, lineArray := dmp.DiffLinesToChars(a, b)
	diffs := dmp.DiffMain(chars1, chars2, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArray)
	var ops []diffOp
	for _, d := range diffs {
		kind := byte(' ')
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			kind = '-'
		case diffmatchpatch.DiffInsert:
			kind = '+'
		}
		for _, ln := range splitKeepEOL(d.Text) {
			ops = append(ops, diffOp{kind: kind, line: ln})
		}
	}
	return ops
}

// splitKeepEOL 按行切分并保留每行的 `\n`（最后一行可能没有换行）。
func splitKeepEOL(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 8)
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}
