// readwrite.go — file_read / file_write。
package filetool

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
)

// maxFullReadBytes 全文读取上限（超过只允许 infoOnly / tail / start+limit）。
const maxFullReadBytes = 32 << 20

// runRead 实现 file_read。
func runRead(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		label := fmt.Sprintf("targets[%d].path", i)
		path, perr := resolvePath(asString(t, "path"), label)
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		results = append(results, readOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// readOne 读取单个文件。
func readOne(path string, t map[string]any) map[string]any {
	st, err := os.Stat(path)
	if err != nil {
		return itemErr(map[string]any{"path": path}, err.Error())
	}
	if st.IsDir() {
		return itemErr(map[string]any{"path": path}, "is a directory (use file_find to list)")
	}
	out := map[string]any{
		"path":   path,
		"size":   st.Size(),
		"mtime":  st.ModTime().UTC().Format(time.RFC3339),
		"is_dir": false,
	}
	if sum, err := md5File(path); err == nil {
		out["md5"] = sum
	}
	infoOnly := asBool(t, "infoOnly", false)

	data, truncated, err := readBytes(path, st.Size(), infoOnly, asString(t, "encoding"), t)
	if err != nil {
		return itemErr(out, err.Error())
	}
	if infoOnly {
		out["encoding"] = fileops.EncodingName(data)
		if n, err := countLines(data); err == nil {
			out["lines"] = n
		}
		return itemOK(out)
	}

	text := decodeWithFallback(data, asString(t, "encoding"))
	lines := splitLines(text)
	out["encoding"] = fileops.EncodingName(data)
	out["lines"] = len(lines)
	out["truncated"] = truncated

	selected, selErr := selectLines(lines, t)
	if selErr != nil {
		return itemErr(out, selErr.Error())
	}
	if asBool(t, "lineNumbers", false) {
		var b strings.Builder
		for _, ln := range selected.lines {
			fmt.Fprintf(&b, "%6d\t%s\n", ln.no, ln.text)
		}
		out["content"] = b.String()
	} else {
		parts := make([]string, 0, len(selected.lines))
		for _, ln := range selected.lines {
			parts = append(parts, ln.text)
		}
		out["content"] = strings.Join(parts, "\n")
	}
	if selected.ranged {
		out["range"] = selected.desc
	}
	return itemOK(out)
}

// readBytes 读取原始字节；大文件按读法决定是否可读。
func readBytes(path string, size int64, infoOnly bool, encoding string, t map[string]any) ([]byte, bool, error) {
	if infoOnly {
		// 只读头部用于编码判定（最多 64KB）
		return readHead(path, 64<<10)
	}
	if size > maxFullReadBytes {
		tail := asInt(t, "tail", 0)
		start := asInt(t, "start", 0)
		limit := asInt(t, "limit", 0)
		switch {
		case tail > 0:
			return readTail(path, 8<<20)
		case start > 0 && limit > 0:
			data, _, err := readHead(path, maxFullReadBytes)
			return data, true, err
		default:
			return nil, false, fmt.Errorf("file too large (%d bytes > %d); use infoOnly, tail, or start+limit", size, int64(maxFullReadBytes))
		}
	}
	data, err := os.ReadFile(path)
	return data, false, err
}

// readHead 读取文件头部最多 n 字节。
func readHead(path string, n int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := f.Read(buf)
	if err != nil && read == 0 {
		return nil, false, err
	}
	return buf[:read], false, nil
}

// readTail 读取文件末尾最多 n 字节（用于大文件 tail）。
func readTail(path string, n int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	off := st.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, false, err
	}
	return buf, off > 0, nil
}

// decodeWithFallback 按显式编码解码；auto / 未知 → 走 fileops.DecodeText 识别链。
func decodeWithFallback(data []byte, encoding string) string {
	if n := normalizeEncoding(encoding); n != "auto" {
		if s, err := decodeByName(data, n); err == nil {
			return s
		}
	}
	return fileops.DecodeText(data)
}

// lineEntry 是带行号的文本行。
type lineEntry struct {
	no   int
	text string
}

// selectedLines 是行选择结果。
type selectedLines struct {
	lines  []lineEntry
	ranged bool
	desc   string
}

// splitLines 按行切分（保留行内容，不保留换行符）。
func splitLines(text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// selectLines 按 start/limit/tail/ranges 选行。
func selectLines(lines []string, t map[string]any) (selectedLines, error) {
	start := asInt(t, "start", 0)
	limit := asInt(t, "limit", 0)
	tail := asInt(t, "tail", 0)
	ranges := strings.TrimSpace(asString(t, "ranges"))
	total := len(lines)

	if ranges != "" {
		if start > 0 || limit > 0 || tail > 0 {
			return selectedLines{}, fmt.Errorf("ranges is mutually exclusive with start/limit/tail")
		}
		out := selectedLines{ranged: true, desc: ranges}
		for _, seg := range strings.Split(ranges, ",") {
			seg = strings.TrimSpace(seg)
			if seg == "" {
				continue
			}
			var a, b int
			if _, err := fmt.Sscanf(seg, "%d:%d", &a, &b); err != nil {
				return selectedLines{}, fmt.Errorf("bad range %q (want a:b)", seg)
			}
			if a < 1 {
				a = 1
			}
			if b > total {
				b = total
			}
			for i := a; i <= b; i++ {
				out.lines = append(out.lines, lineEntry{no: i, text: lines[i-1]})
			}
		}
		return out, nil
	}
	if tail > 0 {
		if start > 0 || limit > 0 {
			return selectedLines{}, fmt.Errorf("tail is mutually exclusive with start/limit")
		}
		if tail > total {
			tail = total
		}
		out := selectedLines{ranged: true, desc: fmt.Sprintf("tail:%d", tail)}
		for i := total - tail + 1; i <= total; i++ {
			out.lines = append(out.lines, lineEntry{no: i, text: lines[i-1]})
		}
		return out, nil
	}
	if start > 0 || limit > 0 {
		if start < 1 {
			start = 1
		}
		if start > total {
			start = total + 1
		}
		end := total
		if limit > 0 && start+limit-1 < end {
			end = start + limit - 1
		}
		out := selectedLines{ranged: true, desc: fmt.Sprintf("%d:%d", start, end)}
		for i := start; i <= end; i++ {
			out.lines = append(out.lines, lineEntry{no: i, text: lines[i-1]})
		}
		return out, nil
	}
	out := selectedLines{lines: make([]lineEntry, 0, total)}
	for i, ln := range lines {
		out.lines = append(out.lines, lineEntry{no: i + 1, text: ln})
	}
	return out, nil
}

// countLines 统计字节内容按 \n 切分的行数。
func countLines(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	n := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n, nil
}

// runWrite 实现 file_write。
func runWrite(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	seen := map[string]bool{}
	for i, t := range req.targets {
		label := fmt.Sprintf("targets[%d].path", i)
		path, perr := resolvePath(asString(t, "path"), label)
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		if seen[path] {
			results = append(results, itemErr(map[string]any{"path": path}, "duplicate target in one call"))
			continue
		}
		seen[path] = true
		results = append(results, writeOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// writeOne 写入单个文件。
func writeOne(path string, t map[string]any) map[string]any {
	content, ok := t["content"].(string)
	if !ok {
		return itemErr(map[string]any{"path": path}, "content is required")
	}
	out := map[string]any{"path": path}

	oldData, oldExists := []byte(nil), false
	if b, err := os.ReadFile(path); err == nil {
		oldData, oldExists = b, true
	}
	overwrite := asBool(t, "overwrite", true)
	if oldExists && !overwrite {
		return itemErr(out, "target exists (overwrite=false)")
	}
	if asBool(t, "createDirs", true) {
		if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error())
		}
	}

	encName := normalizeEncoding(asString(t, "encoding"))
	if encName == "auto" {
		encName = "utf-8"
		if oldExists {
			encName = autoEncodeNameOf(oldData)
		}
	}
	bom := strings.ToLower(strings.TrimSpace(asString(t, "bom")))
	if bom == "" {
		bom = "keep"
	}
	wantBOM := false
	switch bom {
	case "add":
		wantBOM = true
	case "keep":
		wantBOM = oldExists && hasBOM(oldData)
	}
	data, actualBOM, err := encodeWithEncoding(content, encName, wantBOM)
	if err != nil {
		return itemErr(out, err.Error())
	}
	if err := writeAtomic(path, data); err != nil {
		return itemErr(out, "write: "+err.Error())
	}
	action := "created"
	if oldExists {
		action = "modified"
	}
	out["action"] = action
	out["size"] = len(data)
	out["encoding"] = encName
	if actualBOM {
		out["bom"] = encName
	} else {
		out["bom"] = "none"
	}
	if sum, err := md5File(path); err == nil {
		out["md5"] = sum
	}
	return itemOK(out)
}

// autoEncodeNameOf 检测已有文件的编码名（用于 encoding=auto 的原编码回写）。
func autoEncodeNameOf(data []byte) string {
	if _, ok := utf16OrderOf(data); ok {
		if data[0] == 0xFE && data[1] == 0xFF {
			return "utf-16be"
		}
		return "utf-16le"
	}
	if looksLikeUTF8(data) {
		return "utf-8"
	}
	det := detectEncoding(data)
	if det.Encoding != "" && det.Confidence != "low" {
		return normalizeEncoding(det.Encoding)
	}
	return "utf-8"
}

// hasBOM 判断数据是否带 BOM。
func hasBOM(data []byte) bool {
	if _, ok := utf16OrderOf(data); ok {
		return true
	}
	return len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF
}

// encodeWithEncoding 按编码编码并控制 BOM。
func encodeWithEncoding(s, encName string, wantBOM bool) ([]byte, bool, error) {
	n := normalizeEncoding(encName)
	if n == "utf-8" {
		b := []byte(s)
		if wantBOM {
			return append([]byte{0xEF, 0xBB, 0xBF}, b...), true, nil
		}
		return b, false, nil
	}
	if n == "utf-16le" || n == "utf-16be" {
		order := unicodeLE
		if n == "utf-16be" {
			order = unicodeBE
		}
		b, err := encodeUTF16(s, order, wantBOM)
		return b, wantBOM, err
	}
	b, err := encodeByName(s, n)
	return b, false, err
}

// dirOf 返回文件所在目录（path 已解析为绝对路径）。
func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		return p[:i]
	}
	return "."
}
