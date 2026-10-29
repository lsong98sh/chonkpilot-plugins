package fileops

import (
	"fmt"
	"strings"
)

// ApplyFileDiff 是 applyUnifiedDiff 的导出包装（供 file_patch 原子工具复用）。
func ApplyFileDiff(src, diffContent string) (string, error) {
	return applyUnifiedDiff(src, diffContent)
}

// applyUnifiedDiff 应用 unified diff（支持多个 @@ hunk，按 -start 定位 + context/-/+ 逐行消费）。
// 校验：每个 hunk 的首个上下文/删除行须与源文件对应行逐字匹配，不匹配即报错拒绝（防止按行号
// 盲目消费破坏内容）。保留源文本的文件尾换行（无 hunk 覆盖的尾部换行不被 join 吞掉）。
func applyUnifiedDiff(src, diffContent string) (string, error) {
	diffLines := strings.Split(diffContent, "\n")
	srcLines := strings.Split(src, "\n")
	hadTrailing := len(srcLines) > 0 && srcLines[len(srcLines)-1] == ""
	if hadTrailing {
		srcLines = srcLines[:len(srcLines)-1]
	}
	var out []string
	srcIdx := 0
	matchedAny := false
	hunkFirst := false // 当前 hunk 尚未消费任何上下文/删除行（用于仅校验 hunk 首行）
	checkContext := func(lineNo int, want string) error {
		if srcIdx >= len(srcLines) {
			return fmt.Errorf("hunk 上下文不匹配：源文件第 %d 行缺失（期望 %q）", srcIdx+1, want)
		}
		if srcLines[srcIdx] != want {
			return fmt.Errorf("hunk 上下文不匹配：源文件第 %d 行实际 %q，期望 %q", srcIdx+1, srcLines[srcIdx], want)
		}
		return nil
	}
	for _, dl := range diffLines {
		dl = strings.TrimSuffix(dl, "\r")
		switch {
		case strings.HasPrefix(dl, "@@"):
			var oldStart int
			if _, err := fmt.Sscanf(dl, "@@ -%d", &oldStart); err == nil && oldStart > 0 {
				// 跳转到下一 hunk 前，先把两 hunk 之间未覆盖的源行原样收进 out
				// （标准 git diff 3 行 context、两处修改相距 >6 行必产生 gap；不回填会静默丢失中间源行）。
				if oldStart-1 > srcIdx && oldStart-1 <= len(srcLines) {
					out = append(out, srcLines[srcIdx:oldStart-1]...)
				}
				srcIdx = oldStart - 1
			}
			hunkFirst = true
			matchedAny = true
		case strings.HasPrefix(dl, "---") || strings.HasPrefix(dl, "+++"):
			// 文件头，忽略
		case strings.HasPrefix(dl, "+"):
			out = append(out, dl[1:])
		case strings.HasPrefix(dl, "-"):
			if hunkFirst {
				if err := checkContext(srcIdx, dl[1:]); err != nil {
					return "", err
				}
				hunkFirst = false
			}
			srcIdx++ // 跳过被删行
		default:
			// context 行（' ' 前缀或空行）
			content := dl
			if strings.HasPrefix(dl, " ") {
				content = dl[1:]
			}
			if hunkFirst {
				if err := checkContext(srcIdx, content); err != nil {
					return "", err
				}
				hunkFirst = false
			}
			if srcIdx < len(srcLines) {
				out = append(out, srcLines[srcIdx])
				srcIdx++
			}
		}
	}
	if srcIdx < len(srcLines) {
		out = append(out, srcLines[srcIdx:]...)
	}
	if !matchedAny {
		return "", fmt.Errorf("no hunks in diff")
	}
	result := strings.Join(out, "\n")
	if hadTrailing && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result, nil
}
