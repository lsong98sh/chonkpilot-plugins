// readwrite.go — file_read / file_write。
package filetool

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/encoding/traditionalchinese"
	xunicode "golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const (
	// defaultMaxBytes 是单次 file_read 调用（所有 target 合计）返回内容上限的默认值。
	defaultMaxBytes = 1 << 20 // 1 MiB
	// maxContentBytesHard 是单次调用内容上限的硬上限（防误传超大值撑爆内存/上下文）。
	maxContentBytesHard = 32 << 20 // 32 MiB
	// encodingProbeBytes 是编码探测读取的头部字节数。
	encodingProbeBytes = 64 << 10
	// readChunkBytes 是流式读取缓冲大小。
	readChunkBytes = 64 << 10
)

// runRead 实现 file_read。
//
// 单遍流式读取：md5 / 全文行数 / 内容 / 编码探测在同一次 I/O 中完成。
// maxBytes 是**本次调用所有 target 合计**的返回内容上限：当前文件截断后，后续 target 直接 skipped。
func runRead(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	maxBytes := asInt(args, "maxBytes", defaultMaxBytes)
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > maxContentBytesHard {
		return paramErr(tool, fmt.Sprintf("maxBytes must be <= %d", maxContentBytesHard))
	}

	results := make([]map[string]any, 0, len(req.targets))
	remaining := maxBytes
	for i, t := range req.targets {
		// maxBytes 只限制 content：infoOnly 不返回正文、也不消耗预算，预算耗尽后仍应读取元信息。
		infoOnly := asBool(t, "infoOnly", false)
		if remaining <= 0 && !infoOnly {
			results = append(results, itemSkip(map[string]any{"path": asString(t, "path")}, "maxBytes exhausted"))
			continue
		}
		label := fmt.Sprintf("targets[%d].path", i)
		path, perr := resolvePath(asString(t, "path"), label)
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		res, used, exhausted := readOneStream(path, t, remaining)
		results = append(results, res)
		if exhausted {
			// 当前文件已因预算/单行过大截断：按“第一个超限文件之后全部跳过”的语义收口。
			remaining = 0
		} else if used > 0 {
			remaining -= used
			if remaining < 0 {
				remaining = 0
			}
		}
	}
	return finish(tool, results)
}

// readOneStream 读取单个文件。
//
// 返回：(结果对象, 本次实际返回的 content 字节数, 是否因预算/单行过大截断而应跳过后续 target)。
// 失败/跳过/仅元信息 → used=0；仅元信息/失败 → exhausted=false。
// readOneStream 读取单个文件（显式编码解码失败时自动识别并二次读取）。
func readOneStream(path string, t map[string]any, budget int) (map[string]any, int, bool) {
	return readOneStreamAttempt(path, t, budget, true)
}

func readOneStreamAttempt(path string, t map[string]any, budget int, allowFallback bool) (map[string]any, int, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return itemErr(map[string]any{"path": path}, err.Error()), 0, false
	}
	if st.IsDir() {
		return itemErr(map[string]any{"path": path}, "is a directory (use file_find to list)"), 0, false
	}
	out := map[string]any{
		"path":  path,
		"size":  st.Size(),
		"mtime": st.ModTime().UTC().Format(time.RFC3339),
	}

	if _, ok := t["ranges"]; ok {
		out["code"] = "RANGE_UNSUPPORTED"
		return itemErr(out, "ranges is not supported"), 0, false
	}

	start := asInt(t, "start", 0)
	limit := asInt(t, "limit", 0)
	tail := asInt(t, "tail", 0)
	lineNumbers := asBool(t, "lineNumbers", false)
	infoOnly := asBool(t, "infoOnly", false)
	if start < 0 {
		out["code"] = "START_INVALID"
		return itemErr(out, "start must be >= 1"), 0, false
	}
	if limit < 0 {
		out["code"] = "LIMIT_INVALID"
		return itemErr(out, "limit must be >= 1"), 0, false
	}
	if tail < 0 {
		out["code"] = "TAIL_INVALID"
		return itemErr(out, "tail must be >= 1"), 0, false
	}
	if tail > 0 && (start > 0 || limit > 0) {
		out["code"] = "TAIL_CONFLICT"
		return itemErr(out, "tail is mutually exclusive with start/limit"), 0, false
	}
	if start == 0 {
		start = 1
	}

	f, err := os.Open(path)
	if err != nil {
		return itemErr(out, err.Error()), 0, false
	}
	defer f.Close()

	// 单遍：tee 负责给原始字节算 md5；头部先读出来做编码探测，再通过 MultiReader 回放给 decoder。
	hasher := md5.New()
	tee := io.TeeReader(f, hasher)
	probe := make([]byte, encodingProbeBytes)
	n, rerr := io.ReadFull(tee, probe)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		out["code"] = "READ_ERROR"
		return itemErr(out, "read: "+rerr.Error()), 0, false
	}
	head := probe[:n]

	requestedEnc := normalizeEncoding(asString(t, "encoding"))
	encName, dec, head, code, encErr := chooseEncoding(head, asString(t, "encoding"))
	if encErr != "" {
		out["code"] = code
		if code == "BINARY_UNSUPPORTED" {
			out["binary"] = true
		}
		return itemErr(out, encErr), 0, false
	}
	out["encoding"] = encName

	// 空文件（或仅 BOM）：total=0、md5=空文件/BOM 的 md5，内容为空。
	if len(head) == 0 {
		out["total"] = 0
		out["file_md5"] = hex.EncodeToString(hasher.Sum(nil))
		if !infoOnly {
			out["returned"] = 0
			out["content"] = ""
			out["truncated"] = false
		}
		return itemOK(out), 0, false
	}

	var reader io.Reader = io.MultiReader(bytes.NewReader(head), tee)
	if dec != nil {
		reader = transform.NewReader(reader, dec)
	}
	br := bufio.NewReaderSize(reader, readChunkBytes)

	var (
		lineNo        int
		totalLines    int
		content       []byte
		remaining     = budget
		contentStart  bool
		contentDone   bool
		returnedN     int
		truncated     bool
		lineTooLarge  bool
		nextStart     int
		budgetDropped bool
		ring          []tailLine
		ringBytes     int
	)

	for {
		nextNo := lineNo + 1
		selected := false
		if !infoOnly {
			switch {
			case tail > 0:
				selected = true
			case !contentDone && nextNo >= start && (limit <= 0 || nextNo < start+limit):
				selected = true
			}
		}
		keep := 0
		if !infoOnly && (tail > 0 || selected) {
			keep = budget
		}

		ln, lerr := readStreamLine(br, keep)
		if lerr != nil {
			// 显式编码解码失败（非法字节序列）→ 自动识别并二次读取，结果带 fallback 标识。
			if allowFallback && requestedEnc != "auto" {
				return readFallback(path, t, budget, requestedEnc)
			}
			out["code"] = "READ_ERROR"
			return itemErr(out, "read: "+lerr.Error()), 0, false
		}
		// 显式 UTF-8 无 decoder，需逐行验证；非法字节同样触发自动识别回退。
		if requestedEnc == "utf-8" && dec == nil && len(ln.text) > 0 && !utf8.Valid(ln.text) {
			if allowFallback {
				return readFallback(path, t, budget, requestedEnc)
			}
			out["code"] = "READ_ERROR"
			return itemErr(out, "read: invalid UTF-8 byte sequence"), 0, false
		}
		if ln.eof && !ln.hadNewline && len(ln.text) == 0 && !ln.tooLong {
			break
		}
		lineNo++
		totalLines = lineNo

		if !infoOnly {
			switch {
			case tail > 0:
				if len(ring) >= tail {
					dropped := ring[0]
					ring = ring[1:]
					ringBytes -= len(dropped.text) + 1
				}
				ring = append(ring, tailLine{no: lineNo, text: string(ln.text), tooLong: ln.tooLong})
				ringBytes += len(ln.text) + 1
				for ringBytes > budget && len(ring) > 1 {
					dropped := ring[0]
					ring = ring[1:]
					ringBytes -= len(dropped.text) + 1
					budgetDropped = true
				}
			case selected && !contentDone:
				if lineNumbers {
					prefix := []byte(fmt.Sprintf("%6d\t", lineNo))
					suffix := []byte("\n")
					required := len(prefix) + len(ln.text) + len(suffix)
					tooLargeLine := ln.tooLong || len(ln.text) > budget
					switch {
					case tooLargeLine && contentStart:
						// 单行超过整次文件预算：停在完整行边界，标记该行过大。
						truncated = true
						lineTooLarge = true
						nextStart = lineNo
						contentDone = true
					case tooLargeLine:
						// 第一行就过大：返回能放下的前缀（LINE_TOO_LARGE）。
						var ok bool
						content, remaining, ok = appendFully(content, prefix, remaining)
						if ok {
							content, remaining, _ = appendContent(content, ln.text, remaining)
							if remaining > 0 {
								content, remaining, _ = appendFully(content, suffix, remaining)
							}
						}
						truncated = true
						lineTooLarge = true
						returnedN++
						contentStart = true
						contentDone = true
					case required <= remaining:
						content = append(content, prefix...)
						content = append(content, ln.text...)
						content = append(content, suffix...)
						remaining -= required
						returnedN++
						contentStart = true
					case contentStart:
						// 预算不足以接纳整行：停在完整行边界，下一次从 lineNo 继续。
						truncated = true
						nextStart = lineNo
						contentDone = true
					default:
						// 第一行因前缀/换行开销放不下：返回可放下的前缀。
						var ok bool
						content, remaining, ok = appendFully(content, prefix, remaining)
						if ok {
							content, remaining, _ = appendContent(content, ln.text, remaining)
							if remaining > 0 {
								content, remaining, _ = appendFully(content, suffix, remaining)
							}
						}
						truncated = true
						returnedN++
						contentStart = true
						contentDone = true
					}
				} else {
					var prefix []byte
					if contentStart {
						prefix = []byte("\n")
					}
					required := len(prefix) + len(ln.text)
					tooLargeLine := ln.tooLong || len(ln.text) > budget
					switch {
					case tooLargeLine && contentStart:
						truncated = true
						lineTooLarge = true
						nextStart = lineNo
						contentDone = true
					case tooLargeLine:
						var ok bool
						content, remaining, ok = appendFully(content, prefix, remaining)
						if ok {
							content, remaining, _ = appendContent(content, ln.text, remaining)
						}
						truncated = true
						lineTooLarge = true
						returnedN++
						contentStart = true
						contentDone = true
					case required <= remaining:
						content = append(content, prefix...)
						content = append(content, ln.text...)
						remaining -= required
						returnedN++
						contentStart = true
					case contentStart:
						truncated = true
						nextStart = lineNo
						contentDone = true
					default:
						var ok bool
						content, remaining, ok = appendFully(content, prefix, remaining)
						if ok {
							content, remaining, _ = appendContent(content, ln.text, remaining)
						}
						truncated = true
						returnedN++
						contentStart = true
						contentDone = true
					}
				}
			}
		}
		if ln.eof {
			break
		}
	}

	if tail > 0 {
		contentStr, returned, tTrunc, tTooLarge := buildTailContent(ring, lineNumbers, budget)
		content = []byte(contentStr)
		returnedN = returned
		truncated = truncated || tTrunc || budgetDropped
		lineTooLarge = lineTooLarge || tTooLarge
	}

	out["total"] = totalLines
	out["file_md5"] = hex.EncodeToString(hasher.Sum(nil))
	if infoOnly {
		return itemOK(out), 0, false
	}

	out["returned"] = returnedN
	out["content"] = string(content)
	out["truncated"] = truncated
	if truncated {
		if lineTooLarge {
			out["truncated_reason"] = "line_too_large"
			out["code"] = "LINE_TOO_LARGE"
		} else {
			out["truncated_reason"] = "maxBytes"
		}
	}
	if nextStart > 0 {
		out["next_start"] = nextStart
	}
	if tail == 0 && start > totalLines && out["code"] == nil {
		out["code"] = "START_BEYOND_EOF"
		return itemErr(out, "start beyond EOF (file has "+fmt.Sprint(totalLines)+" lines)"), len(content), truncated
	}
	return itemOK(out), len(content), truncated
}

// preferChineseCandidate 在候选编码并列/低置信时用「解码结果像不像中文」做最终仲裁：
// 当前候选不像中文、而 GBK/GB18030 候选像中文 → 采用中文候选；否则保持原判定。
func preferChineseCandidate(head []byte, cands []encodingx.Candidate, current string) string {
	if current == "" || current == "binary" {
		return current
	}
	if s, err := encodingx.Decode(current, head); err == nil && encodingx.LooksLikeChinese(s) {
		return current
	}
	for _, c := range cands {
		if c.Encoding != encodingx.GBK && c.Encoding != encodingx.GB18030 {
			continue
		}
		if s, err := encodingx.Decode(c.Encoding, head); err == nil && encodingx.LooksLikeChinese(s) {
			return c.Encoding
		}
	}
	return current
}

// chooseEncoding 决定读取时使用的源编码与 decoder；同时回放 head（可能剥掉 BOM）。
// code/errMsg 非空表示失败（二进制 / 不支持的编码）。
func chooseEncoding(head []byte, raw string) (enc string, dec transform.Transformer, outHead []byte, code, errMsg string) {
	want := normalizeEncoding(raw)
	if want != "auto" {
		// 显式编码不能绕过二进制拒绝。UTF-16 是合法文本、字节天然含 0x00，例外放行；
		// 其余显式编码先做与 auto 相同的二进制探测（0x00 启发式）。
		if want != "utf-16le" && want != "utf-16be" {
			if det := detectEncoding(head); det.Binary {
				return "", nil, head, "BINARY_UNSUPPORTED", "binary file (not supported)"
			}
		}
		switch want {
		case "utf-8":
			// 显式 UTF-8 保持原字节；读取过程中按行做 utf8.Valid 校验（失败触发自动识别回退）。
			return want, nil, stripUTF8BOM(head), "", ""
		case "utf-16le":
			return want, xunicode.UTF16(xunicode.LittleEndian, xunicode.IgnoreBOM).NewDecoder(), stripUTF16BOM(head), "", ""
		case "utf-16be":
			return want, xunicode.UTF16(xunicode.BigEndian, xunicode.IgnoreBOM).NewDecoder(), stripUTF16BOM(head), "", ""
		default:
			d, ok := legacyDecoder(want)
			if !ok {
				return "", nil, head, "ENCODING_UNSUPPORTED", "unsupported encoding: " + strings.TrimSpace(raw)
			}
			return want, d, head, "", ""
		}
	}

	det := detectEncoding(head)
	if det.Binary {
		return "", nil, head, "BINARY_UNSUPPORTED", "binary file (not supported)"
	}
	enc = normalizeEncoding(det.Encoding)
	// 候选打分在「半角假名 vs 常用汉字」接近时可能把 GBK 文本判成 shift_jis（如短中文样本）。
	// 仅当当前候选解码结果「不像中文」而中文候选明显像中文时才改用后者，避免破坏真实日文识别。
	if !utf8.Valid(head) {
		enc = preferChineseCandidate(head, det.Candidates, enc)
	}
	switch enc {
	case "", "binary":
		enc = "utf-8"
		return enc, nil, head, "", ""
	case "utf-8":
		return enc, nil, stripUTF8BOM(head), "", ""
	case "utf-16le":
		return enc, xunicode.UTF16(xunicode.LittleEndian, xunicode.UseBOM).NewDecoder(), head, "", ""
	case "utf-16be":
		return enc, xunicode.UTF16(xunicode.BigEndian, xunicode.UseBOM).NewDecoder(), head, "", ""
	default:
		d, ok := legacyDecoder(enc)
		if !ok {
			// 探测结果不可用 → 按 UTF-8 兜底（与旧 DecodeAuto 的最终兜底一致）。
			return "utf-8", nil, head, "", ""
		}
		return enc, d, head, "", ""
	}
}

// legacyDecoder 返回遗留编码的流式 decoder。
func legacyDecoder(name string) (transform.Transformer, bool) {
	switch name {
	case "gbk":
		return simplifiedchinese.GBK.NewDecoder(), true
	case "gb18030":
		return simplifiedchinese.GB18030.NewDecoder(), true
	case "shift_jis":
		return japanese.ShiftJIS.NewDecoder(), true
	case "big5":
		return traditionalchinese.Big5.NewDecoder(), true
	case "euc-kr":
		return korean.EUCKR.NewDecoder(), true
	}
	return nil, false
}

// tailLine 是 tail 模式保留的一行。
type tailLine struct {
	no      int
	text    string
	tooLong bool
}

// buildTailContent 从 ring 中取“最后尽可能多的行”，总字节数不超过 budget。
// 返回 (content, 返回行数, 是否截断, 是否单行过大)。
func buildTailContent(ring []tailLine, lineNumbers bool, budget int) (string, int, bool, bool) {
	if len(ring) == 0 {
		return "", 0, false, false
	}
	if budget <= 0 {
		return "", 0, true, false
	}
	parts := make([]string, len(ring))
	for i, e := range ring {
		if lineNumbers {
			parts[i] = fmt.Sprintf("%6d\t%s\n", e.no, e.text)
		} else {
			parts[i] = e.text
		}
	}
	// suffix[i] = parts[i:] 拼成 content 后的字节数。
	suffix := make([]int, len(parts)+1)
	for i := len(parts) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + len(parts[i])
		if !lineNumbers && i < len(parts)-1 {
			suffix[i]++ // 分隔符 "\n"
		}
	}
	start := 0
	for start < len(parts) && suffix[start] > budget {
		start++
	}
	tooLarge := false
	if start == len(parts) {
		// 连最后一行都放不下：保留最后一行的前缀（LINE_TOO_LARGE）。
		start = len(parts) - 1
		p := parts[start]
		if len(p) > budget {
			p = string(trimToValidUTF8([]byte(p)[:budget]))
			parts[start] = p
		}
		tooLarge = true
	}
	truncated := start > 0 || tooLarge
	for _, e := range ring {
		if e.tooLong {
			truncated = true
			tooLarge = true
		}
	}
	if lineNumbers {
		var b strings.Builder
		for _, p := range parts[start:] {
			b.WriteString(p)
		}
		return b.String(), len(parts) - start, truncated, tooLarge
	}
	return strings.Join(parts[start:], "\n"), len(parts) - start, truncated, tooLarge
}

// streamLine 是一行解码后的文本（不含换行符）。
type streamLine struct {
	text       []byte
	hadNewline bool
	eof        bool
	tooLong    bool
}

// readStreamLine 从解码流中读一行；keep 是保留的最大字节数（0 = 只消费不保留）。
// 单行超过 keep 时只保留前缀并置 tooLong=true，剩余部分继续消费（保证 md5/行数准确）。
func readStreamLine(br *bufio.Reader, keep int) (streamLine, error) {
	var (
		buf     []byte
		tooLong bool
	)
	for {
		chunk, err := br.ReadSlice('\n')
		if len(chunk) > 0 {
			data := chunk
			ended := false
			if err == nil {
				data = chunk[:len(chunk)-1]
				ended = true
			}
			if keep > 0 {
				room := keep - len(buf)
				if room > 0 {
					if len(data) > room {
						buf = append(buf, data[:room]...)
						tooLong = true
					} else {
						buf = append(buf, data...)
					}
				} else if len(data) > 0 {
					tooLong = true
				}
			} else if len(data) > 0 {
				tooLong = true
			}
			if ended {
				if len(buf) > 0 && buf[len(buf)-1] == '\r' {
					buf = buf[:len(buf)-1]
				}
				if tooLong {
					buf = trimToValidUTF8(buf)
				}
				return streamLine{text: buf, hadNewline: true, tooLong: tooLong}, nil
			}
		}
		switch err {
		case nil:
			continue
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(buf) > 0 || tooLong {
				if tooLong {
					buf = trimToValidUTF8(buf)
				}
				return streamLine{text: buf, eof: true, tooLong: tooLong}, nil
			}
			return streamLine{eof: true}, nil
		default:
			return streamLine{}, err
		}
	}
}

// appendFully 追加完整片段；放不下则原样返回且 ok=false（不写半截）。
func appendFully(dst, src []byte, remaining int) ([]byte, int, bool) {
	if len(src) > remaining {
		return dst, remaining, false
	}
	return append(dst, src...), remaining - len(src), true
}

// appendContent 追加内容片段；放不下时按 UTF-8 字符边界截断（允许半行）。
func appendContent(dst, src []byte, remaining int) ([]byte, int, bool) {
	if remaining <= 0 {
		if len(src) == 0 {
			return dst, remaining, true
		}
		return dst, remaining, false
	}
	if len(src) <= remaining {
		return append(dst, src...), remaining - len(src), true
	}
	cut := trimToValidUTF8(src[:remaining])
	return append(dst, cut...), 0, false
}

// trimToValidUTF8 从末尾回退到合法 UTF-8 边界（最多回退 3 字节）。
func trimToValidUTF8(b []byte) []byte {
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return b
}

// stripUTF8BOM 去掉 UTF-8 BOM（若有）。
func stripUTF8BOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// stripUTF16BOM 去掉 UTF-16 LE/BE BOM（若有）。
func stripUTF16BOM(b []byte) []byte {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		return b[2:]
	}
	return b
}

// readHead 读取文件头部最多 n 字节。空文件返回空字节且不报错（供 file_detect 等复用）。
//
// 用 io.ReadFull 而不是单次 Read：单次 Read 可能短读，导致 sample/检测窗口不完整。
// EOF / ErrUnexpectedEOF 表示文件短于 n，按实际字节数返回，不算错误。
func readHead(path string, n int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	buf := make([]byte, n)
	read, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, false, err
	}
	return buf[:read], false, nil
}

// runWrite 实现 file_write。
//
// 批处理顺序（2026-11-09）：
//  1. 先解析全部路径，按解析后的路径分组；同路径 **last-wins**（只执行最后一个
//     target），前面的重复项直接 skipped（reason/error = superseded_by_later_target），
//     不打开、不写入；
//  2. 按入参顺序为每个 winner 建 job，第一遍每个文件**只尝试一次**独占打开，
//     errFileInUse（Windows 共享/锁冲突、Unix EWOULDBLOCK）记 pending 并继续后面的文件；
//  3. 其他文件处理完后，对 pending 做 3 轮 50/100/200ms 重试；仍失败 → winner 单独
//     failed（code=FILE_IN_USE），前面的 skipped 保持 skipped（不回退执行）；
//  4. results 按入参顺序预分配、按索引填充，最后 finish。
func runWrite(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	defaultEnc := normalizeEncoding(asString(args, "defaultEncoding"))
	if defaultEnc == "auto" {
		defaultEnc = ""
	}
	if defaultEnc != "" && !supportedEncoding(defaultEnc) {
		return paramErr(tool, "unsupported defaultEncoding: "+defaultEnc+"（配置页默认字符集或显式 encoding 无效）")
	}
	results := make([]map[string]any, len(req.targets))
	pathOf := make([]string, len(req.targets))
	groups := make(map[string][]int, len(req.targets))
	for i, t := range req.targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results[i] = itemErr(map[string]any{"path": asString(t, "path")}, perr)
			continue
		}
		pathOf[i] = path
		groups[path] = append(groups[path], i)
	}

	// 同路径只执行最后一个 target；前面的重复项 skipped。
	winnerAt := make(map[int]bool, len(groups))
	for _, idxs := range groups {
		winner := idxs[len(idxs)-1]
		winnerAt[winner] = true
		for _, i := range idxs[:len(idxs)-1] {
			results[i] = itemSkip(map[string]any{"path": pathOf[i]}, "superseded_by_later_target")
		}
	}

	jobs := make([]batchJob, 0, len(winnerAt))
	for i := range req.targets {
		if !winnerAt[i] {
			continue
		}
		jobs = append(jobs, newWriteJob(pathOf[i], req.targets[i], i, results, defaultEnc))
	}
	runBatchJobs(results, jobs)
	return finish(tool, results)
}

// newWriteJob 构造 file_write 的单个 winner 调度单元。
// try 只在独占打开遇到 errFileInUse 时返回 true（不填结果，等待重试）。
func newWriteJob(path string, t map[string]any, index int, results []map[string]any, defaultEnc string) batchJob {
	return batchJob{
		indices: []int{index},
		try: func() bool {
			res, inUse := writeOneOnce(path, t, defaultEnc)
			if inUse {
				return true
			}
			results[index] = res
			return false
		},
		fillInUse: func() {
			results[index] = itemErr(map[string]any{"path": path, "code": "FILE_IN_USE"},
				"file is in use by another process: "+path)
		},
	}
}

// writeOneOnce 对**已按批调度尝试一次**的文件完成写入；不内部 sleep/重试。
//
// 返回 (结果, inUse)：
//   - inUse=true：独占打开遇到 errFileInUse，未填结果（由调度器记 pending 重试）；
//     调用方必须忽略此时的结果。
//   - inUse=false：已完成（ok / failed），结果按该 winner 的索引填回。
//
// 顺序：建父目录 → 独占打开（openExclusiveWriteOnce；overwrite 判定在内）→
// 从同一句柄读旧文件头判编码/BOM → 编码/BOM → 就地写。
// 只影响本项：前面的同路径 skipped 不回退执行；写失败且已写入部分字节 → partial:true。
func writeOneOnce(path string, t map[string]any, defaultEnc string) (map[string]any, bool) {
	content, ok := t["content"].(string)
	if !ok {
		return itemErr(map[string]any{"path": path}, "content is required"), false
	}
	out := map[string]any{"path": path}

	createDirs := asBool(t, "createDirs", true)
	parent := dirOf(path)
	if createDirs {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return itemErr(out, "create dir: "+err.Error()), false
		}
	} else if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		return itemErr(out, "write: parent dir not found (createDirs=false)"), false
	}

	overwrite := asBool(t, "overwrite", true)
	f, oldExists, err := openExclusiveWriteOnce(path, overwrite)
	if err != nil {
		if errors.Is(err, errFileInUse) {
			return nil, true
		}
		return itemErr(out, err.Error()), false
	}
	defer f.Close()

	if st, serr := f.Stat(); serr == nil && st.IsDir() {
		return itemErr(out, "is a directory"), false
	}

	// 只为编码/BOM 从独占句柄读取旧文件头（不整文件读入内存）；读后回到起点。
	var oldHead []byte
	if oldExists {
		head := make([]byte, encodingProbeBytes)
		n, rerr := io.ReadFull(f, head)
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return itemErr(out, "read for encoding: "+rerr.Error()), false
		}
		oldHead = head[:n]
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return itemErr(out, "seek: "+serr.Error()), false
		}
	}

	encName := normalizeEncoding(asString(t, "encoding"))
	if encName != "auto" {
		if encName == "" || !supportedEncoding(encName) {
			return itemErr(out, "unsupported encoding: "+strings.TrimSpace(asString(t, "encoding"))), false
		}
	} else if oldExists {
		name, emsg := autoEncodeNameStrict(oldHead)
		if emsg != "" {
			// 短样本/低置信度：若配置了默认字符集且数据确实像 GBK 中文，按 ANSI 兜底。
			if defaultEnc != "" && looksLikeANSI(oldHead) {
				name = encodingx.GBK
			} else {
				return itemErr(out, emsg+"; pass encoding explicitly"), false
			}
		}
		encName = normalizeEncoding(name)
		// 旧文件保持原编码；仅 Windows 中文 ANSI（GBK/GB18030）可转为配置的默认字符集。
		if defaultEnc != "" && isANSIEncoding(encName) {
			encName = defaultEnc
		}
	} else if defaultEnc != "" {
		// 新文件：未指定 encoding 时使用配置页的默认字符集。
		encName = defaultEnc
	} else {
		encName = encodingx.UTF8
	}

	bom := strings.ToLower(strings.TrimSpace(asString(t, "bom")))
	if bom == "" {
		bom = "strip" // 默认无 BOM
	}
	switch bom {
	case "keep", "add", "strip":
	default:
		return itemErr(out, `bom must be "keep"|"add"|"strip"`), false
	}
	// BOM 仅对 UTF-8 输出生效；默认无 BOM。keep 仅在旧文件本身是 UTF-8 且有 BOM 时保留。
	wantBOM := false
	if encName == encodingx.UTF8 {
		switch bom {
		case "add":
			wantBOM = true
		case "keep":
			wantBOM = oldExists && oldWasUTF8(oldHead) && hasBOM(oldHead)
		}
	}
	data, actualBOM, err := encodeWithEncoding(content, encName, wantBOM)
	if err != nil {
		return itemErr(out, err.Error()), false
	}
	written, werr := writeInPlace(f, data)
	if werr != nil {
		msg := "write: " + werr.Error()
		// file_write 没有旧内容快照：已写入部分字节时目标可能不完整。
		if written > 0 {
			out["partial"] = true
			msg += " (target may be incomplete)"
		}
		return itemErr(out, msg), false
	}
	action := "created"
	if oldExists {
		action = "modified"
	}
	out["action"] = action
	out["size"] = len(data)
	out["encoding"] = encName
	if actualBOM {
		out["bom"] = encodingx.UTF8
	} else {
		out["bom"] = "none"
	}
	// 用内存内容算 md5：独占句柄会挡住写后再次打开文件（md5File）。
	out["md5"] = md5Hex(data)
	return itemOK(out), false
}

// autoEncodeNameStrict 用于 file_write 的 encoding=auto：只在有把握时返回编码名；
// 二进制/低置信度返回错误文案（不静默回落 UTF-8）。
func autoEncodeNameStrict(data []byte) (string, string) {
	switch encodingx.BOMName(data) {
	case encodingx.UTF16LE:
		return encodingx.UTF16LE, ""
	case encodingx.UTF16BE:
		return encodingx.UTF16BE, ""
	}
	if looksLikeUTF8(data) {
		return encodingx.UTF8, ""
	}
	det := detectEncoding(data)
	if det.Binary {
		return "", "binary file (not supported)"
	}
	if det.Encoding != "" && det.Confidence != "low" {
		return normalizeEncoding(det.Encoding), ""
	}
	return "", "encoding ambiguous (auto detection confidence low)"
}

// oldWasUTF8 判断旧文件本身是否是 UTF-8（BOM 或严格 UTF-8 文本）。
func oldWasUTF8(data []byte) bool {
	if name := encodingx.BOMName(data); name != "" {
		return name == encodingx.UTF8
	}
	return looksLikeUTF8(data)
}

// autoEncodeNameOf 检测已有文件的编码名（用于 encoding=auto 的原编码回写）。
func autoEncodeNameOf(data []byte) string {
	switch encodingx.BOMName(data) {
	case encodingx.UTF16LE:
		return encodingx.UTF16LE
	case encodingx.UTF16BE:
		return encodingx.UTF16BE
	}
	if looksLikeUTF8(data) {
		return encodingx.UTF8
	}
	det := detectEncoding(data)
	if det.Encoding != "" && det.Confidence != "low" {
		return normalizeEncoding(det.Encoding)
	}
	return encodingx.UTF8
}

// hasBOM 判断数据是否带 BOM。
func hasBOM(data []byte) bool {
	return encodingx.BOMName(data) != ""
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
		b, err := encodeUTF16(s, n, wantBOM)
		return b, wantBOM, err
	}
	b, err := encodeByName(s, n)
	return b, false, err
}

// readFallback 用 auto 编码二次读取，并标注原显式请求。
func readFallback(path string, t map[string]any, budget int, requested string) (map[string]any, int, bool) {
	t2 := cloneTarget(t)
	t2["encoding"] = "auto"
	res, used, exhausted := readOneStreamAttempt(path, t2, budget, false)
	if res != nil {
		res["encoding_requested"] = requested
		res["encoding_fallback"] = true
	}
	return res, used, exhausted
}

// cloneTarget 复制 target map（fallback 二次读取时避免修改调用方参数）。
func cloneTarget(t map[string]any) map[string]any {
	m := make(map[string]any, len(t))
	for k, v := range t {
		m[k] = v
	}
	return m
}

// dirOf 返回文件所在目录（path 已解析为绝对路径）。
func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i > 0 {
		return p[:i]
	}
	return "."
}
