package fileops

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// hunkHeaderRe 匹配 unified diff 的 hunk 头：`@@ -a,b +c,d @@`（b/d 可省略，缺省 1）。
// 头后可跟函数上下文（如 `@@ -1,3 +1,4 @@ func main()`），故只锚定前缀。
var hunkHeaderRe = regexp.MustCompile(`^@@\s+-(\d+)(?:,(\d+))?\s+\+(\d+)(?:,(\d+))?\s+@@`)

// ApplyFileDiff 是 applyUnifiedDiff 的导出包装（供 file_patch 原子工具复用）。
func ApplyFileDiff(src, diffContent string) (string, error) {
	return applyUnifiedDiff(src, diffContent)
}

// applyUnifiedDiff 应用 unified diff（支持多个 @@ hunk，按 -start 定位 + context/-/+ 逐行消费）。
//
// 校验口径：
//   - 每个 hunk 内的**全部** context / 删除行都必须与源文件对应行逐字匹配，任何不匹配整项失败；
//   - `@@ -a,b +c,d @@` 声明的行数必须与实际 hunk 内容一致（b/d 省略时按 1）；
//   - hunk 只接受在 `---`/`+++` 文件头之后；hunk 内以 `++`/`--` 开头的增删内容按 `+`/`-` 处理，
//     不会再被当作文件头丢弃；
//   - CRLF 源文件：比对时忽略源行尾 `\r`，输出保持源文件的 CRLF 风格。
//
// 保留源文本的文件尾换行（无 hunk 覆盖的尾部换行不被 join 吞掉）。
func applyUnifiedDiff(src, diffContent string) (string, error) {
	srcLines, hadTrailing := splitSourceLines(src)
	crlf := strings.Contains(src, "\r\n")
	diffLines := strings.Split(diffContent, "\n")

	var out []string
	srcIdx := 0
	hunks := 0
	hunkNo := 0
	inHunk := false
	var oldRemain, newRemain int

	checkContext := func(content string) error {
		if srcIdx >= len(srcLines) {
			return fmt.Errorf("hunk %d 上下文不匹配：源文件第 %d 行缺失（期望 %q）", hunkNo, srcIdx+1, content)
		}
		got := strings.TrimSuffix(srcLines[srcIdx], "\r")
		if got != content {
			return fmt.Errorf("hunk %d 上下文不匹配：源文件第 %d 行实际 %q，期望 %q", hunkNo, srcIdx+1, got, content)
		}
		return nil
	}
	// appendGap 把两个 hunk 之间未被覆盖的源行原样收进 out（标准 diff 的 3 行上下文之间必有 gap）。
	appendGap := func(target int) error {
		if target < srcIdx {
			return fmt.Errorf("hunk %d 起始行重叠：目标位置 %d 早于当前源位置 %d", hunkNo, target+1, srcIdx+1)
		}
		if target > len(srcLines) {
			return fmt.Errorf("hunk %d 起始行越界：源文件共 %d 行，补丁定位到第 %d 行", hunkNo, len(srcLines), target+1)
		}
		if target > srcIdx {
			out = append(out, srcLines[srcIdx:target]...)
			srcIdx = target
		}
		return nil
	}
	addLine := func(content string) string {
		if crlf {
			return content + "\r"
		}
		return content
	}

	for _, raw := range diffLines {
		dl := strings.TrimSuffix(raw, "\r")

		if inHunk {
			if oldRemain == 0 && newRemain == 0 {
				inHunk = false // 本 hunk 已按声明行数消费完，继续按 hunk 外语法解析当前行
			} else {
				if strings.HasPrefix(dl, `\`) {
					continue // "\ No newline at end of file"：不计行数
				}
				prefix := byte(' ')
				content := ""
				if dl != "" {
					switch dl[0] {
					case ' ':
						prefix, content = ' ', dl[1:]
					case '-':
						prefix, content = '-', dl[1:]
					case '+':
						prefix, content = '+', dl[1:]
					default:
						return "", fmt.Errorf("hunk %d 行数不符/含非法行（应以空格、+ 或 - 开头）: %q", hunkNo, dl)
					}
				}
				switch prefix {
				case ' ':
					if oldRemain <= 0 || newRemain <= 0 {
						return "", fmt.Errorf("hunk %d 行数不符：context 行超出 hunk 头声明的行数", hunkNo)
					}
					if err := checkContext(content); err != nil {
						return "", err
					}
					out = append(out, srcLines[srcIdx])
					srcIdx++
					oldRemain--
					newRemain--
				case '-':
					if oldRemain <= 0 {
						return "", fmt.Errorf("hunk %d 行数不符：删除行超出声明的旧行数", hunkNo)
					}
					if err := checkContext(content); err != nil {
						return "", err
					}
					srcIdx++
					oldRemain--
				case '+':
					if newRemain <= 0 {
						return "", fmt.Errorf("hunk %d 行数不符：新增行超出声明的新行数", hunkNo)
					}
					out = append(out, addLine(content))
					newRemain--
				}
				continue
			}
		}

		switch {
		case strings.HasPrefix(dl, "@@"):
			m := hunkHeaderRe.FindStringSubmatch(dl)
			if m == nil {
				return "", fmt.Errorf("非法 hunk 头: %q", dl)
			}
			oldStart, err := strconv.Atoi(m[1])
			if err != nil {
				return "", fmt.Errorf("非法 hunk 头: %q", dl)
			}
			oldCount := 1
			if m[2] != "" {
				if oldCount, err = strconv.Atoi(m[2]); err != nil {
					return "", fmt.Errorf("非法 hunk 头: %q", dl)
				}
			}
			if _, err := strconv.Atoi(m[3]); err != nil {
				return "", fmt.Errorf("非法 hunk 头: %q", dl)
			}
			newCount := 1
			if m[4] != "" {
				if newCount, err = strconv.Atoi(m[4]); err != nil {
					return "", fmt.Errorf("非法 hunk 头: %q", dl)
				}
			}
			if oldCount < 0 || newCount < 0 {
				return "", fmt.Errorf("非法 hunk 头: %q", dl)
			}
			hunkNo++
			hunks++
			// 定位：oldCount>0 时 oldStart 是首行（1-based）；oldCount==0 时 oldStart 是插入点
			// 之前的行号（0 表示文件开头）。
			target := oldStart - 1
			if oldCount == 0 {
				target = oldStart
			}
			if err := appendGap(target); err != nil {
				return "", err
			}
			inHunk = true
			oldRemain, newRemain = oldCount, newCount
			if oldRemain == 0 && newRemain == 0 {
				inHunk = false
			}
		case strings.HasPrefix(dl, "--- ") || strings.HasPrefix(dl, "+++ "):
			// 文件头（仅在 hunk 外识别；hunk 内同前缀内容行已按 +/- 处理）。
		case dl == "":
			// 容忍 hunk 之间的空行。
		default:
			return "", fmt.Errorf("hunk 外出现无法识别的行: %q", dl)
		}
	}

	if inHunk && (oldRemain > 0 || newRemain > 0) {
		return "", fmt.Errorf("hunk %d 行数不足：仍缺 %d 个旧行 / %d 个新行", hunkNo, oldRemain, newRemain)
	}
	if hunks == 0 {
		return "", fmt.Errorf("no hunks in diff")
	}
	if srcIdx < len(srcLines) {
		out = append(out, srcLines[srcIdx:]...)
	}
	result := strings.Join(out, "\n")
	if hadTrailing && result != "" && !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result, nil
}

// splitSourceLines 按 `\n` 切分源文本并去掉尾随空段，同时返回是否存在文件尾换行。
// 行内 `\r`（CRLF）保留，供写回时维持原换行风格。
func splitSourceLines(src string) ([]string, bool) {
	lines := strings.Split(src, "\n")
	hadTrailing := len(lines) > 0 && lines[len(lines)-1] == ""
	if hadTrailing {
		lines = lines[:len(lines)-1]
	}
	return lines, hadTrailing
}
