package fileops

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// maxReadBytes 是 file_read 单文件读入内存的硬上限（32MB，与 fetch 响应体上限同口径）：
// 超过即只读前 maxReadBytes 字节并置 truncated=true，防超大文件整读入内存（C-54）。
const maxReadBytes = 32 << 20

// readReq 是 file_read 的单文件请求。
type readReq struct {
	path        string
	start       int
	limit       int
	lineNumbers bool
	tail        int
	infoOnly    bool
	ranges      [][2]int
}

// readOne 读取单个文件，返回结构化 info（path/md5/encoding/lines/size/content 等）；错误时 info 带 error。
func readOne(req readReq, workDir string) (map[string]interface{}, error) {
	resolved, errMsg := ResolvePath(req.path, workDir)
	if errMsg != "" {
		return infoMap(req.path, errMsg), fmt.Errorf("%s", errMsg)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return infoMap(req.path, err.Error()), fmt.Errorf("stat file: %w", err)
	}
	// 读操作**不加跨进程锁**：读是共享语义，与并发写无锁冲突；且锁文件落在被读文件同目录，
	// 在只读目录下会因无法创建锁文件而误伤合法读（C-03）。写侧（fileops 修改类动作）
	// 仍在写前/写后持锁，互斥语义由写侧保证。
	info := map[string]interface{}{
		"path": req.path, "lines": 0, "size": fi.Size(),
		"encoding": "unknown", "modified": fi.ModTime().Format(time.RFC3339), "truncated": false,
	}
	// C-54：超过上限的文件不再整文件读入内存（即便给了 start/limit/ranges）——只读前 maxReadBytes
	// 字节作为内容/行数/编码的截断前缀并置 truncated=true；md5 仍流式计算全量（O(1) 内存）。
	var data []byte
	if fi.Size() > maxReadBytes {
		f, oerr := os.Open(resolved)
		if oerr != nil {
			info["error"] = oerr.Error()
			return info, fmt.Errorf("read file: %w", oerr)
		}
		var rerr error
		data, rerr = io.ReadAll(io.LimitReader(f, maxReadBytes))
		f.Close()
		if rerr != nil { // 非 EOF 的读取错误不再静默吞掉（R7-C-9）；并置项级失败，避免顶层误报 success（R8-C-4）
			info["error"] = rerr.Error()
			return info, fmt.Errorf("read file: %w", rerr)
		}
		info["truncated"] = true
	} else {
		var rerr error
		data, rerr = os.ReadFile(resolved)
		if rerr != nil {
			info["error"] = rerr.Error()
			return info, fmt.Errorf("read file: %w", rerr)
		}
	}
	info["md5"], _ = fileMD5(resolved)
	info["encoding"] = EncodingName(data)
	info["lines"] = countLines(data)
	// 截断文件（>32MB）：info["lines"] 只是**已读前缀**的行数，非全文行数。仅当**确需**全文行数
	// （info=true 或 line_numbers=true）时才流式扫描 total_lines（O(1) 内存），避免「仅因 truncated
	// 就无条件全量扫描」叠加 md5 造成的二次全量 I/O。
	truncated := info["truncated"].(bool)
	totalLines := -1
	if truncated && (req.infoOnly || req.lineNumbers) {
		if total, lerr := countFileLines(resolved); lerr == nil {
			totalLines = total
			info["total_lines"] = total
		}
	}

	if req.infoOnly {
		return info, nil
	}

	// >32MB 且带 ranges/start 时，行段语义建立在截断前缀上会与真实行号错位（静默错行），
	// 显式报错而非误导。**与 tail 同传时亦报错**：否则 ranges/start 会被下方 tail 分支静默吞掉
	// （既不报错也不使用行段）；limit-only 取前 N 行与前缀一致，放行。
	if truncated && (len(req.ranges) > 0 || req.start > 0) {
		err := fmt.Errorf("文件超过读取上限（%d 字节），不支持 ranges/start 行段读取，请改用 tail 或 info", maxReadBytes)
		info["error"] = err.Error()
		return info, err
	}

	// R7-C-60：>32MB 文件的 tail 语义不能建立在截断前缀上（前缀尾 ≠ 文件尾）。
	// 改为从文件末尾反向流式定位最后 N 行，与 file_read tail 契约（只读末尾 N 行）一致。
	// line_numbers：readFileTail 原样返回内容，需按全文总行数补行号（旧实现丢行号）。
	if truncated && req.tail > 0 {
		content, terr := readFileTail(resolved, req.tail)
		if terr != nil {
			info["error"] = terr.Error()
			return info, terr
		}
		// 仅当基线（全文总行数）已知时才补行号：countFileLines 失败（totalLines<0）时**放弃行号**，
		// 避免旧实现 base 回落 1 造成的「从 1 起」错误行号误导调用方。
		if req.lineNumbers && content != "" && totalLines >= 0 {
			tailN := strings.Count(content, "\n") + 1
			base := totalLines - tailN + 1
			if base < 1 {
				base = 1
			}
			content = addLineNumbers(strings.Split(content, "\n"), base)
		}
		info["content"] = content
		return info, nil
	}

	content, err := readContent(data, req)
	if err != nil {
		info["error"] = err.Error()
		return info, err
	}
	info["content"] = content
	if !truncated {
		info["truncated"] = req.start > 0 || req.limit > 0 || req.tail > 0 || len(req.ranges) > 0
	}
	return info, nil
}

func infoMap(path, errMsg string) map[string]interface{} {
	return map[string]interface{}{"path": path, "error": errMsg}
}

// countLines 统计逻辑行数（与 readContent/readFileTail/countFileLines **同语义**：仅去掉
// **一个**尾换行，结尾的多个空行仍各计一行）。旧实现用 TrimRight 去掉**所有**尾换行，导致
// 以多个空行结尾的文件中 countLines 与 countFileLines/readContent 互相矛盾。
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	s := string(data)
	nl := strings.Count(s, "\n")
	if strings.HasSuffix(s, "\n") {
		return nl // 去一个尾空行："a\nb\n"→2；"\n"→1；"\n\n"→2
	}
	return nl + 1
}

// countFileLines 流式统计文件总行数（O(1) 内存，不整读大文件），行分割与 readContent/readFileTail
// 同语义（结尾**单个**换行不算新空行）："a\nb\n" → 2；"a\nb" → 2；"\n" → 1；空文件 → 0。
// UTF-16（BOM）文件**先流式解码再按字符计 \n**——UTF-16 换行为 2 字节（LE "0A 00"/BE "00 0A"），
// 按原始字节统计会与内容侧 DecodeText 的行语义不一致（total_lines 偏大、tail 行号基线错位）。
// 供 >32MB 截断文件补齐 total_lines 与 tail 行号基线。
func countFileLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var r io.Reader = f
	head := make([]byte, 2)
	n0, _ := io.ReadFull(f, head)
	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return 0, serr
	}
	if n0 == 2 && HasUTF16BOM(head) {
		// 与 readContent 的 DecodeText 同源（UTF16 LE + UseBOM 自动识别字节序）。
		r = transform.NewReader(f, unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewDecoder())
	}
	nl := 0
	last := byte('\n')
	any := false
	buf := make([]byte, 64<<10)
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			any = true
			nl += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return 0, rerr
		}
	}
	if !any {
		return 0, nil
	}
	if last == '\n' {
		return nl, nil
	}
	return nl + 1, nil
}

// readContent 读取并切片文件内容（start/limit、tail、ranges、全文）。
func readContent(data []byte, req readReq) (string, error) {
	if IsBinaryBytes(data) {
		return DataURI(data), nil
	}
	content := DecodeText(data)
	lines := strings.Split(content, "\n")
	// 去掉末尾空行（文件以 \n 结尾）
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// 空文件：Split 得到 [""]，去尾空行后 len==0。此时 ranges 分支的 clamp(_,1,0) 与
	// start/limit 分支的 startIdx 都会算成 0/1，lines[startIdx-1:endIdx] 即 lines[-1:…] → 越界 panic。
	// 直接返回空内容（tail 分支在 len==0 时本也安全，此处一并短路）。
	if len(lines) == 0 {
		return "", nil
	}

	var selected []string
	switch {
	case len(req.ranges) > 0:
		var segs []string
		for ri, r := range req.ranges {
			startIdx := clamp(r[0], 1, len(lines))
			endIdx := clamp(r[1], startIdx, len(lines))
			seg := lines[startIdx-1 : endIdx]
			label := fmt.Sprintf("-- segment %d: lines %d-%d --", ri+1, startIdx, endIdx)
			if req.lineNumbers {
				segs = append(segs, label+"\n"+addLineNumbers(seg, startIdx))
			} else {
				segs = append(segs, label+"\n"+strings.Join(seg, "\n"))
			}
		}
		return strings.Join(segs, "\n\n"), nil
	case req.tail > 0:
		tailN := req.tail
		if tailN > len(lines) {
			tailN = len(lines)
		}
		selected = lines[len(lines)-tailN:]
	case req.start > 0 || req.limit > 0:
		startIdx := req.start
		if startIdx < 1 {
			startIdx = 1
		}
		if startIdx > len(lines) {
			startIdx = len(lines)
		}
		endIdx := len(lines)
		if req.limit > 0 {
			endIdx = startIdx - 1 + req.limit
			if endIdx > len(lines) {
				endIdx = len(lines)
			}
		}
		selected = lines[startIdx-1 : endIdx]
	default:
		selected = lines
	}

	if req.lineNumbers {
		// 基线须与上方**内容选择**同源（同优先级、同裁剪）：否则 tail/start 越界或二者同传时
		// 会用未裁剪的请求值算出行号（负值 / 错行）。选择顺序 = tail 优先，其次 start/limit。
		base := 1
		switch {
		case req.tail > 0:
			tailN := req.tail
			if tailN > len(lines) {
				tailN = len(lines)
			}
			base = len(lines) - tailN + 1
		case req.start > 0:
			base = clamp(req.start, 1, len(lines))
		}
		return addLineNumbers(selected, base), nil
	}
	return strings.Join(selected, "\n"), nil
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func addLineNumbers(lines []string, start int) string {
	width := len(fmt.Sprintf("%d", start+len(lines)-1))
	if width < 2 {
		width = 2
	}
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d  %s", width, start+i, line)
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// readFileTail 从文件末尾反向分块读取最后 n 行（不整读大文件，R7-C-60）。
// 行分割与 readContent 同语义（去掉末尾空行）；n 超过总行数时返回全部行。
func readFileTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const chunk = 64 << 10
	// maxTailScan 是反向扫描窗口上限（R8-C-3）：对无换行文件（minified 单行）若逐块读满会整读整文件，
	// 故超过该窗口即停止并返回已得内容，保持「末尾 N 行」语义。
	const maxTailScan = 8 << 20
	var buf []byte
	pos := fi.Size()
	for pos > 0 && int64(len(buf)) < maxTailScan {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		tmp := make([]byte, readSize)
		rn, rerr := f.ReadAt(tmp, pos)
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		// 并发截断时 ReadAt 可能少于请求字节，只用有效前缀（否则含 NUL）。
		buf = append(tmp[:rn], buf...)
		if bytes.Count(buf, []byte{'\n'}) > n {
			break
		}
	}
	content := DecodeText(buf)
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	// 反向扫描在**窗口耗尽**（pos>0，未到文件头）且窗口内行数不足以判定「首个完整行」时，buf 首行
	// 可能是被截断的半行（chunk / maxTailScan 边界落在行中）——此时丢弃该半行，保证返回内容首行完整，
	// 使调用方按行号基线标注时不错位（否则 tailN 按换行计数会整体错位）。判据：读 buf 前一个字节，
	// 为 '\n' 则首行恰为行首（完整），否则为半行。若 buf 已覆盖到文件头（pos==0）或窗口内行数已 >n
	// （首行本就不在选中范围内），则无半行问题。
	if pos > 0 && len(lines) > 0 && len(lines) <= n {
		atLineStart := false
		prev := make([]byte, 1)
		if rn, rerr := f.ReadAt(prev, pos-1); rerr == nil && rn == 1 && prev[0] == '\n' {
			atLineStart = true
		}
		if !atLineStart {
			lines = lines[1:]
		}
	}
	if n > len(lines) {
		n = len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n"), nil
}
