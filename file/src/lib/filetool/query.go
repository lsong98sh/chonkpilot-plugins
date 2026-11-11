// query.go — file_find / file_detect / file_iconv。
package filetool

import (
	"bufio"
	"bytes"
	"container/heap"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
	"github.com/lsong98sh/chonkpilot-util/encodingx"
	"github.com/lsong98sh/chonkpilot-util/strx"
)

const (
	defaultFindLimit  = 2000
	hardFindLimit     = 2000
	defaultMaxHash    = int64(512 << 20)
	defaultHitsPerF   = 100
	defaultSampleSize = 64 << 10
	// maxSampleBytes 是 file_detect 单次取样的硬上限（4 MiB）。
	// 超过直接 failed（SAMPLE_TOO_LARGE），不读取、不分配，避免超大整数撑爆内存。
	maxSampleBytes = 4 << 20
	// defaultTotalHits 是 grep 单次 target 的命中行总预算（跨文件累计）。
	defaultTotalHits = 2000
	// hardTotalHits 是 maxTotalHits 的硬上限（超出按上限收敛，防误传超大值）。
	hardTotalHits = 20000
)

// findHit 是 grep 的一处命中。
type findHit struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// findEntry 是 file_find 的一个结果项。
type findEntry struct {
	Rel         string    `json:"rel"`
	IsDir       bool      `json:"is_dir"`
	Size        int64     `json:"size,omitempty"`
	Mtime       string    `json:"mtime,omitempty"`
	MD5         string    `json:"md5,omitempty"`
	HashSkipped string    `json:"hash_skipped,omitempty"`
	Hits        []findHit `json:"hits,omitempty"`
}

// runFind 实现 file_find（四合一）。
func runFind(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		root, perr := resolvePath(asString(t, "root"), fmt.Sprintf("targets[%d].root", i))
		if perr != "" {
			results = append(results, itemErr(map[string]any{"root": asString(t, "root")}, perr))
			continue
		}
		results = append(results, findOne(root, t))
	}
	return finish(tool, results)
}

// findOne 执行一次查找。
func findOne(root string, t map[string]any) map[string]any {
	out := map[string]any{"root": root}

	// root 必须是真实目录：普通文件 / symlink 明确失败（旧实现会静默返回 ok + 空结果）。
	st, statErr := os.Lstat(root)
	if statErr != nil {
		return itemErr(out, statErr.Error())
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return itemErr(out, "root 是 symlink，请传入真实目录路径")
	}
	if !st.IsDir() {
		return itemErr(out, "root 不是目录")
	}

	limit := asInt(t, "limit", defaultFindLimit)
	if limit <= 0 {
		limit = defaultFindLimit
	}
	if limit > hardFindLimit {
		limit = hardFindLimit
	}
	maxHits := asInt(t, "maxHitsPerFile", defaultHitsPerF)
	if maxHits <= 0 {
		maxHits = defaultHitsPerF
	}
	maxTotalHits := asInt(t, "maxTotalHits", defaultTotalHits)
	if maxTotalHits <= 0 {
		maxTotalHits = defaultTotalHits
	}
	if maxTotalHits > hardTotalHits {
		maxTotalHits = hardTotalHits
	}
	globPat := strings.TrimSpace(asString(t, "glob"))
	grepPat := asString(t, "grep")
	useRegex := asBool(t, "regex", false)
	// output 模式：dir 是「等价 ls」的便捷别名（输出同 json，缺省 depth=1）。
	mode := strings.ToLower(strings.TrimSpace(asString(t, "output")))
	if mode == "" {
		mode = "json"
	}
	// depth 方案 B：省略时 dir 默认 1，其余模式默认 0（0 = 不限）；显式 depth 始终优先。
	depth := 0
	if mode == "dir" {
		depth = 1
	}
	if v, ok := intArg(t, "depth"); ok {
		depth = v
	}
	if depth < 0 {
		return itemErr(out, "depth must be >= 0")
	}
	meta := strings.ToLower(strings.TrimSpace(asString(t, "meta")))
	if meta == "" {
		meta = "none"
	}
	maxHashBytes := int64(asInt(t, "maxHashBytes", 0))
	if maxHashBytes <= 0 {
		maxHashBytes = defaultMaxHash
	}

	var re *regexp.Regexp
	if grepPat != "" && useRegex {
		compiled, err := regexp.Compile(grepPat)
		if err != nil {
			return itemErr(out, "bad regex: "+err.Error())
		}
		re = compiled
	}
	filter := buildWalkFilter(asStringSlice(t, "skipDirs"), asStringSlice(t, "ignoreFiles"))
	period, perr := parseFindPeriod(asString(t, "from"), asString(t, "to"))
	if perr != "" {
		return itemErr(out, perr)
	}

	// 确定性截断：遍历过程中只保留字典序最小的 limit 项（最大堆，堆顶为当前保留集合中最大 rel）。
	// matched 仍统计全量命中；遍历顺序因此不再影响返回子集。
	kept := &findEntryHeap{}
	matched := 0
	scanned := 0
	totalHits := 0
	hitsTruncated := false
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单点不可读跳过，不中断整体查找
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		level := strings.Count(rel, "/") + 1
		if d.IsDir() {
			if filter.SkipDirs[strings.ToLower(d.Name())] || filter.SkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if depth > 0 && level > depth {
				return filepath.SkipDir
			}
			// 目录也作为结果项返回（is_dir=true）：dir 模式据此实现 ls 语义，tree 才有目录节点。
			// grep 模式只返回命中文件，不掺入目录。
			if grepPat == "" {
				if globPat == "" || fileops.MatchGlob(rel, globPat) {
					info, ierr := d.Info()
					if ierr == nil && period.match(info.ModTime()) {
						matched++
						entry := findEntry{Rel: rel, IsDir: true}
						if meta != "none" {
							entry.Mtime = info.ModTime().UTC().Format(time.RFC3339)
						}
						pushFindEntry(kept, entry, limit)
					}
				}
			}
			// 深度已到边界 → 不再下钻（depth=0 = 不限）。
			if depth > 0 && level >= depth {
				return filepath.SkipDir
			}
			return nil
		}
		if depth > 0 && level > depth {
			return nil
		}
		if filter.ignored(d.Name()) {
			return nil
		}
		if globPat != "" && !fileops.MatchGlob(rel, globPat) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if !period.match(info.ModTime()) {
			return nil
		}
		scanned++
		entry := findEntry{Rel: rel}
		if meta != "none" {
			entry.Size = info.Size()
			entry.Mtime = info.ModTime().UTC().Format(time.RFC3339)
		}
		if grepPat != "" {
			hits, herr := grepFile(p, grepPat, re, maxHits)
			if herr != nil || len(hits) == 0 {
				return nil
			}
			// 全局命中预算：达到上限即停止追加并收口遍历；
			// scanned / matched_total 记录“截至点”的值。
			remaining := maxTotalHits - totalHits
			if remaining <= 0 {
				hitsTruncated = true
				return filepath.SkipAll
			}
			if len(hits) > remaining {
				hits = hits[:remaining]
				hitsTruncated = true
			}
			totalHits += len(hits)
			entry.Hits = hits
		}
		matched++
		pushFindEntry(kept, entry, limit)
		if totalHits >= maxTotalHits {
			hitsTruncated = true
			return filepath.SkipAll
		}
		return nil
	})
	if walkErr != nil {
		return itemErr(out, walkErr.Error())
	}
	entries := make([]findEntry, 0, kept.Len())
	for kept.Len() > 0 {
		entries = append(entries, heap.Pop(kept).(findEntry))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	truncated := matched > len(entries) || hitsTruncated

	// meta=hash：只对已返回条目计算，受 maxHashBytes 限制。
	hashTruncated := false
	if meta == "hash" {
		var used int64
		for i := range entries {
			if entries[i].IsDir {
				continue // 目录不做内容哈希
			}
			if used+entries[i].Size > maxHashBytes {
				entries[i].HashSkipped = "exceeded_limit"
				hashTruncated = true
				continue
			}
			if sum, err := md5File(filepath.Join(root, filepath.FromSlash(entries[i].Rel))); err == nil {
				entries[i].MD5 = sum
				used += entries[i].Size
			} else {
				entries[i].HashSkipped = "unreadable"
			}
		}
	}

	out["truncated"] = truncated
	out["matched_total"] = matched
	if grepPat != "" {
		out["scanned"] = scanned
		out["hits_truncated"] = hitsTruncated
	}
	out["hash_truncated"] = hashTruncated
	out["limit"] = limit

	switch mode {
	case "list":
		lines := make([]string, 0, len(entries))
		for _, e := range entries {
			lines = append(lines, e.Rel)
		}
		out["lines"] = lines
	case "tree":
		out["text"] = renderTree(entries)
	default:
		// json / dir：dir 是 ls 便捷别名，输出形态同 json（items 含 is_dir）。
		out["items"] = entries
	}
	return itemOK(out)
}

// grepFile 在单个文件中查找命中行。
//
// 流式实现（不整文件入内存）：先读头部（最多 encodingProbeBytes）做二进制与编码判定，
// 再逐行扫描。UTF-8/legacy 编码按原始字节以 '\n' 切行（这些编码的换行字节不会被
// 多字节字符复用），legacy 逐行用 encodingx.Decode 解码，避免整文件解码驻留内存。
// UTF-16 例外：按 '\n' 切原始字节可能落在 UTF-16 码元的中间，不安全，故回退整文件读取后解码。
func grepFile(path, pattern string, re *regexp.Regexp, maxHits int) ([]findHit, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	head := make([]byte, encodingProbeBytes)
	n, rerr := io.ReadFull(f, head)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		_ = f.Close()
		return nil, rerr
	}
	head = head[:n]
	// 二进制检测只看头部（与 file_read 的编码探测口径一致）。
	if detectEncoding(head).Binary {
		_ = f.Close()
		return nil, fmt.Errorf("binary")
	}
	// 头部可能截断在多字节 UTF-8 字符中间，先去掉残缺尾字节再判编码，
	// 避免大型 UTF-8 文件被 auto-detect 误判成 legacy 编码。
	encHead := head
	if n == len(head) && n == encodingProbeBytes {
		encHead = trimIncompleteUTF8Tail(head)
	}
	encName := encodingx.AutoName(encHead)

	if encName == encodingx.UTF16LE || encName == encodingx.UTF16BE {
		_ = f.Close()
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text, derr := encodingx.Decode(encName, data)
		if derr != nil {
			return nil, derr
		}
		lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
		return scanFindLines(lines, pattern, re, maxHits), nil
	}
	defer f.Close()

	reader := io.MultiReader(bytes.NewReader(head), f)
	br := bufio.NewReaderSize(reader, readChunkBytes)
	hits := make([]findHit, 0, 4)
	lineNo := 0
	sawLine := false
	lastEndedNewline := false
	for {
		chunk, lerr := br.ReadString('\n')
		if lerr != nil && lerr != io.EOF {
			return nil, lerr
		}
		if chunk == "" && lerr == io.EOF {
			// strings.Split 语义：空内容 / 以换行结尾时，末尾还有一个空行。
			if !sawLine || lastEndedNewline {
				if matchFindLine("", pattern, re) {
					hits = append(hits, findHit{Line: lineNo + 1, Text: ""})
				}
			}
			break
		}
		sawLine = true
		lineNo++
		lastEndedNewline = strings.HasSuffix(chunk, "\n")
		ln := chunk
		if lastEndedNewline {
			ln = strings.TrimSuffix(ln, "\n")
			ln = strings.TrimSuffix(ln, "\r") // CRLF
		}
		// 首行剥掉 UTF-8 BOM（与 file_read/fileops.DecodeText 口径一致）。
		// 短文件可能 AutoName=""，因此不能只在 encName==utf-8 分支处理。
		if lineNo == 1 {
			ln = strings.TrimPrefix(ln, "\ufeff")
		}
		switch encName {
		case encodingx.UTF8:
			// UTF-8 无需逐行解码。
		case "":
			// 编码未知：保持原始字节。
		default:
			// legacy 编码逐行解码；单行解码失败时退回原始字节，保证不丢行。
			if decoded, derr := encodingx.Decode(encName, []byte(ln)); derr == nil {
				ln = decoded
			}
		}
		if matchFindLine(ln, pattern, re) {
			hits = append(hits, findHit{Line: lineNo, Text: truncRunes(ln, 500)})
			if len(hits) >= maxHits {
				return hits, nil
			}
		}
		if lerr == io.EOF {
			break
		}
	}
	return hits, nil
}

// matchFindLine 判断一行是否命中（字面量或正则）。
func matchFindLine(line, pattern string, re *regexp.Regexp) bool {
	if re != nil {
		return re.MatchString(line)
	}
	return strings.Contains(line, pattern)
}

// scanFindLines 对已切好的行做匹配（UTF-16 整文件回退路径复用）。
func scanFindLines(lines []string, pattern string, re *regexp.Regexp, maxHits int) []findHit {
	hits := make([]findHit, 0, 4)
	for i, ln := range lines {
		if !matchFindLine(ln, pattern, re) {
			continue
		}
		hits = append(hits, findHit{Line: i + 1, Text: truncRunes(ln, 500)})
		if len(hits) >= maxHits {
			break
		}
	}
	return hits
}

// trimIncompleteUTF8Tail 去掉头部末尾残缺的 UTF-8 字节序列
// （只用于编码判定，不改变实际流式读取的字节）。
func trimIncompleteUTF8Tail(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			break
		}
		b = b[:len(b)-1]
	}
	return b
}

// findEntryHeap 是容量受 limit 限制的最大堆：堆顶 = 当前保留集合中 rel 最大者。
// 用「字典序最小的 N 项」替换「walk 顺序先到先得」，保证同一树重复查询结果稳定。
type findEntryHeap []findEntry

func (h findEntryHeap) Len() int           { return len(h) }
func (h findEntryHeap) Less(i, j int) bool { return h[i].Rel > h[j].Rel }
func (h findEntryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *findEntryHeap) Push(x any) { *h = append(*h, x.(findEntry)) }

func (h *findEntryHeap) Pop() any {
	old := *h
	n := len(old)
	it := old[n-1]
	*h = old[:n-1]
	return it
}

// pushFindEntry 维护「字典序最小的 limit 项」。
func pushFindEntry(h *findEntryHeap, e findEntry, limit int) {
	if limit <= 0 {
		return
	}
	if h.Len() < limit {
		heap.Push(h, e)
		return
	}
	if e.Rel < (*h)[0].Rel {
		heap.Pop(h)
		heap.Push(h, e)
	}
}

// walkFilterX 是 file_find 的遍历过滤。
type walkFilterX struct {
	SkipDirs    map[string]bool
	IgnoreFiles []string
}

// buildWalkFilter 合并内置跳过目录与调用方追加项。
func buildWalkFilter(skipDirs, ignoreFiles []string) *walkFilterX {
	f := &walkFilterX{SkipDirs: map[string]bool{}}
	for k := range fileops.SkipDirs {
		f.SkipDirs[k] = true
	}
	for _, s := range skipDirs {
		if s != "" {
			f.SkipDirs[s] = true
		}
	}
	for _, s := range ignoreFiles {
		if s != "" {
			f.IgnoreFiles = append(f.IgnoreFiles, s)
		}
	}
	return f
}

// ignored 判断文件名是否命中 ignore_files。
func (f *walkFilterX) ignored(name string) bool {
	for _, pat := range f.IgnoreFiles {
		if fileops.MatchGlob(name, pat) {
			return true
		}
	}
	return false
}

// findPeriodX 是 mtime 区间过滤。
type findPeriodX struct {
	from, to time.Time
}

// parseFindPeriod 解析 YYYY-MM-DD（按本地时区解释，与 ModTime 的本地日期对齐）。
func parseFindPeriod(from, to string) (findPeriodX, string) {
	var p findPeriodX
	if s := strings.TrimSpace(from); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return p, "bad from (want YYYY-MM-DD)"
		}
		p.from = t
	}
	if s := strings.TrimSpace(to); s != "" {
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return p, "bad to (want YYYY-MM-DD)"
		}
		p.to = t.Add(24*time.Hour - time.Nanosecond)
	}
	return p, ""
}

// match 判断时间是否落在区间内（空区间 = 全通过）。
func (p findPeriodX) match(t time.Time) bool {
	if !p.from.IsZero() && t.Before(p.from) {
		return false
	}
	if !p.to.IsZero() && t.After(p.to) {
		return false
	}
	return true
}

// renderTree 把扁平条目渲染成树形文本。
func renderTree(entries []findEntry) string {
	var b strings.Builder
	for _, e := range entries {
		depth := strings.Count(e.Rel, "/")
		b.WriteString(strings.Repeat("  ", depth))
		b.WriteString(filepath.Base(e.Rel))
		if e.IsDir {
			b.WriteString("/")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// truncRunes 按 rune 截断（超限追加省略号）。
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strx.TruncRunes(s, n) + "…"
}

// runDetect 实现 file_detect。
func runDetect(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, 0, len(req.targets))
	for i, t := range req.targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results = append(results, itemErr(map[string]any{"path": asString(t, "path")}, perr))
			continue
		}
		results = append(results, detectOne(path, t))
	}
	return finish(tool, results)
}

// detectOne 检测单个文件。
func detectOne(path string, t map[string]any) map[string]any {
	out := map[string]any{"path": path}
	n := asInt(t, "sampleBytes", defaultSampleSize)
	if n < 64 {
		n = 64
	}
	// 上限先于读取：超限直接失败，不打开、不分配（防超大整数导致 OOM）。
	if n > maxSampleBytes {
		out["code"] = "SAMPLE_TOO_LARGE"
		return itemErr(out, "sampleBytes too large (max 4MiB)")
	}
	data, _, err := readHead(path, int64(n))
	if err != nil {
		return itemErr(out, err.Error())
	}
	// 空文件没有可判定内容：encoding=unknown、confidence=low，且不返回 sample/candidates。
	if len(data) == 0 {
		out["binary"] = false
		out["bom"] = "none"
		out["encoding"] = "unknown"
		out["confidence"] = "low"
		return itemOK(out)
	}
	d := detectEncoding(data)
	out["binary"] = d.Binary
	out["bom"] = d.BOM
	out["encoding"] = d.Encoding
	out["confidence"] = d.Confidence
	if out["confidence"] == "" {
		out["confidence"] = "low" // 无候选/样本不足时显式给 low，不返回空串
	}
	if out["encoding"] == "" {
		out["encoding"] = "unknown"
	}
	if len(d.Candidates) > 0 {
		out["candidates"] = d.Candidates
	}
	if d.Sample != "" {
		out["sample"] = d.Sample
	}
	return itemOK(out)
}

// runIconv 实现 file_iconv。
//
// 批处理（2026-11-09，与 write/replace/patch 共用 batchlock.go 的调度）：
//  1. 先解析全部路径；同批重复 path → **后项** failed `duplicate target`
//     （不合并、不做 last-wins；第一项正常执行）；
//  2. 每个有效唯一 path 一个 job，第一遍各尝试一次独占打开；
//     errFileInUse 记 pending 并继续后面的文件；
//  3. 其他文件处理完后，对 pending 做 3 轮 50/100/200ms 重试；
//     仍失败 → 该 path failed + code="FILE_IN_USE"；
//  4. results 按入参顺序预分配、按索引填充；summary.total = 入参数量。
func runIconv(tool string, args map[string]any) *cli.Result {
	req, msg := parseRequest(args)
	if msg != "" {
		return paramErr(tool, msg)
	}
	results := make([]map[string]any, len(req.targets))
	seen := make(map[string]bool, len(req.targets))
	jobs := make([]batchJob, 0, len(req.targets))
	for i, t := range req.targets {
		path, perr := resolvePath(asString(t, "path"), fmt.Sprintf("targets[%d].path", i))
		if perr != "" {
			results[i] = itemErr(map[string]any{"path": asString(t, "path")}, perr)
			continue
		}
		if seen[path] {
			results[i] = itemErr(map[string]any{"path": path}, "duplicate target")
			continue
		}
		seen[path] = true
		jobs = append(jobs, newIconvJob(path, t, i, results))
	}
	runBatchJobs(results, jobs)
	return finish(tool, results)
}

// newIconvJob 构造 file_iconv 的单个文件调度单元。
// try 只在独占打开遇到 errFileInUse 时返回 true（不填结果，等待重试）。
func newIconvJob(path string, t map[string]any, index int, results []map[string]any) batchJob {
	return batchJob{
		indices: []int{index},
		try: func() bool {
			res, inUse := iconvOneOnce(path, t)
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

// iconvOneOnce 对**已按批调度尝试一次**的文件做编码转换；不内部 sleep/重试。
//
// 返回 (结果, inUse)：inUse=true 表示独占打开遇到 errFileInUse（未填结果，调用方必须忽略 res）。
//
// 顺序：参数校验 → 独占打开 → 头部探测（不移动偏移）→ 二进制拒绝 →
// already_target 快判（不建临时文件）→ backup 临时文件（同时算 bytesBefore/md5Before）→
// expectMd5（锁内、修改前）→ 显式 from 的 round-trip（临时文件 + 分段比较）→
// 流式 decode(from) → encode(to) 写入 out 临时文件（同时算 bytesAfter/md5After）→
// 平台提交（Windows 就地写 + backup 回滚；非 Windows rename）。
//
// 全流程不整文件入内存：源经 io.SectionReader + transform.Reader 分段读取，
// 临时文件均位于目标同目录，成功后删除（out 被 rename 时路径已消失）。
//
// 幂等：显式 from 时先做 actual detection，置信度非 low 且 actual==to → skipped already_target；
// 否则 decode(from) → encode(from) 必须与原字节完全一致（from_mismatch 时绝不写）。
func iconvOneOnce(path string, t map[string]any) (map[string]any, bool) {
	out := map[string]any{"path": path}

	to := normalizeEncoding(asString(t, "to"))
	if to == "auto" { // 空串与 auto 均归一为 auto
		return itemErr(out, "to is required"), false
	}
	if !supportedEncodingName(to) {
		return itemErr(out, "unsupported_encoding: "+to), false
	}
	rawFrom := normalizeEncoding(asString(t, "from"))
	if rawFrom != "auto" && !supportedEncodingName(rawFrom) {
		return itemErr(out, "unsupported_encoding: "+rawFrom), false
	}

	// 单次独占打开；共享/锁冲突交给批调度器记 pending 重试。
	f, err := openExclusiveExistingOnce(path)
	if err != nil {
		if errors.Is(err, errFileInUse) {
			return nil, true
		}
		return itemErr(out, err.Error()), false
	}
	defer f.Close()

	// 只读头部（最多 encodingProbeBytes）做二进制与编码判定；ReadAt 不移动文件偏移。
	headBuf := make([]byte, encodingProbeBytes)
	n, rerr := f.ReadAt(headBuf, 0)
	if rerr != nil && rerr != io.EOF {
		return itemErr(out, "read: "+rerr.Error()), false
	}
	head := headBuf[:n]
	det := detectEncoding(head)
	if det.Binary {
		return itemErr(out, "binary file (not a text encoding)"), false
	}
	actual := normalizeEncoding(det.Encoding)
	reliable := actual != "" && actual != "binary" && det.Confidence != "low"
	sourceBOM := encodingx.BOMName(head)

	// expectMd5：与旧实现一致，先做 md5 校验，再做 from==to / already_target 快判。
	// 只需要 md5 时直接流式计算（不创建 backup）；结果保留供后续 backup 复用。
	expectMd5 := strings.TrimSpace(asString(t, "expectMd5"))
	var md5Before string
	md5Ready := false
	if expectMd5 != "" {
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return itemErr(out, "read: "+serr.Error()), false
		}
		hasher := md5.New()
		if _, cerr := io.CopyBuffer(hasher, f, make([]byte, streamChunkBytes)); cerr != nil {
			return iconvStreamError(out, cerr), false
		}
		md5Before = hex.EncodeToString(hasher.Sum(nil))
		md5Ready = true
		if !strings.EqualFold(expectMd5, md5Before) {
			return itemSkip(map[string]any{"path": path, "md5": md5Before}, "md5 mismatch"), false
		}
	}

	// already_target 快判：命中时完全不创建临时文件（expectMd5 已在上面校验）。
	from := rawFrom
	if from == "auto" {
		if !reliable {
			if names := candidateNames(det.Candidates); len(names) > 0 {
				out["candidates"] = names
			}
			return itemErr(out, "encoding_undetermined (specify from explicitly)"), false
		}
		from = actual
		if from == to {
			return itemSkip(map[string]any{"path": path, "from": from, "to": to}, "already_target"), false
		}
	} else if reliable && actual == to {
		// 显式 from：先做 actual detection —— 已是目标编码（重试幂等）。
		return itemSkip(map[string]any{"path": path, "from": from, "to": to}, "already_target"), false
	}

	dir := filepath.Dir(path)

	// backup：原文件完整字节备份；md5Before 已算过则只复制，不重复 hash。
	backup, berr := createIconvTemp(dir)
	if berr != nil {
		return itemErr(out, "write: create backup temp: "+berr.Error()), false
	}
	defer backup.discard()

	if _, serr := f.Seek(0, io.SeekStart); serr != nil {
		return itemErr(out, "read: "+serr.Error()), false
	}
	var bytesBefore int64
	if md5Ready {
		bytesBefore, berr = io.CopyBuffer(&stageWriter{w: backup.f}, f, make([]byte, streamChunkBytes))
		if berr != nil {
			return iconvStreamError(out, berr), false
		}
	} else {
		hasher := md5.New()
		bytesBefore, berr = io.CopyBuffer(io.MultiWriter(&stageWriter{w: backup.f}, hasher), f, make([]byte, streamChunkBytes))
		if berr != nil {
			return iconvStreamError(out, berr), false
		}
		md5Before = hex.EncodeToString(hasher.Sum(nil))
	}

	skip := skipBOMFor(from, sourceBOM)

	// 显式 from：round-trip 必须与原字节完全一致；否则 from 声明不成立。
	// 重新编码失败（如解码结果含 U+FFFD）同样说明 from 不成立。
	if rawFrom != "auto" {
		roundTrip, rterr := createIconvTemp(dir)
		if rterr != nil {
			return itemErr(out, "write: create round-trip temp: "+rterr.Error()), false
		}
		defer roundTrip.discard()

		dec, ok := newStreamDecoder(from)
		if !ok {
			return itemErr(out, "unsupported_encoding: "+from), false
		}
		// round-trip 的 BOM 策略与旧实现一致：源有 BOM 即按 from 重新输出 BOM；
		// 源 BOM 与 from 不一致时因此必然与原字节不同 → from_mismatch。
		enc, ok := newStreamEncoder(from, sourceBOM != "")
		if !ok {
			return itemErr(out, "unsupported_encoding: "+from), false
		}
		writeBOM := sourceBOM != "" && from == encodingx.UTF8
		if _, serr := streamConvert(roundTrip.f, f, bytesBefore, skip, dec, enc, writeBOM); serr != nil {
			if isStreamStage(serr, "encode") {
				return iconvFromMismatch(out, actual, det), false
			}
			return iconvStreamError(out, serr), false
		}
		equal, eqerr := filesEqual(backup.f, roundTrip.f)
		if eqerr != nil {
			return itemErr(out, "read: "+eqerr.Error()), false
		}
		if !equal {
			return iconvFromMismatch(out, actual, det), false
		}
		if from == to {
			// 声明 from==to 且 round-trip 一致：文件确已是目标编码，无需重写。
			return itemSkip(map[string]any{"path": path, "from": from, "to": to}, "already_target"), false
		}
	}

	bomMode := strings.ToLower(strings.TrimSpace(asString(t, "bom")))
	wantBOM := false
	switch bomMode {
	case "add":
		wantBOM = true
	case "strip":
		wantBOM = false
	default: // keep
		wantBOM = sourceBOM != ""
	}

	// out：原文件 → decode(from) → encode(to) 的流式结果，同时计算 bytesAfter / md5After。
	outTemp, oerr := createIconvTemp(dir)
	if oerr != nil {
		return itemErr(out, "write: create output temp: "+oerr.Error()), false
	}
	defer outTemp.discard()

	dec, ok := newStreamDecoder(from)
	if !ok {
		return itemErr(out, "unsupported_encoding: "+from), false
	}
	enc, ok := newStreamEncoder(to, wantBOM)
	if !ok {
		return itemErr(out, "unsupported_encoding: "+to), false
	}
	writeBOM := wantBOM && to == encodingx.UTF8
	hasherAfter := md5.New()
	bytesAfter, serr := streamConvert(io.MultiWriter(outTemp.f, hasherAfter), f, bytesBefore, skip, dec, enc, writeBOM)
	if serr != nil {
		return iconvStreamError(out, serr), false
	}
	md5After := hex.EncodeToString(hasherAfter.Sum(nil))

	// 提交：Windows 就地覆盖 + backup 回滚；非 Windows 临时文件 rename（原子）。
	mode := os.FileMode(0o644)
	if st, statErr := f.Stat(); statErr == nil {
		mode = st.Mode().Perm()
	}
	outcome, commitErr := commitStreamOutput(f, outTemp.f, backup.f, outTemp.path, bytesAfter, bytesBefore, mode)
	if commitErr != nil {
		msg := "write: " + commitErr.Error()
		switch {
		case outcome.partial:
			out["partial"] = true
			return itemErr(out, msg), false
		case outcome.rolledBack:
			return itemErr(out, msg+" (rolled back)"), false
		default:
			return itemErr(out, msg), false
		}
	}

	out["from"] = from
	out["to"] = to
	if wantBOM && targetSupportsBOM(to) {
		out["bom"] = to
	} else {
		out["bom"] = "none"
		// legacy 目标无法携带 BOM：请求了 BOM 但实际未写时显式标注。
		if wantBOM {
			out["bom_dropped"] = true
		}
	}
	out["bytesBefore"] = bytesBefore
	out["bytesAfter"] = bytesAfter
	out["md5Before"] = md5Before
	out["md5After"] = md5After
	return itemOK(out), false
}

// iconvFromMismatch 构造 from_mismatch 失败结果（附 detected / candidates）。
func iconvFromMismatch(out map[string]any, actual string, det detectResult) map[string]any {
	if actual != "" && actual != "binary" {
		out["detected"] = actual
	}
	if names := candidateNames(det.Candidates); len(names) > 0 {
		out["candidates"] = names
	}
	return itemErr(out, "from_mismatch")
}

// iconvStreamError 把流式转换链错误映射回既有错误文案。
func iconvStreamError(out map[string]any, err error) map[string]any {
	switch {
	case isStreamStage(err, "encode"):
		return itemErr(out, "encode_error: "+err.Error())
	case isStreamStage(err, "decode"):
		return itemErr(out, "decode_error: "+err.Error())
	case isStreamStage(err, "write"):
		return itemErr(out, "write: "+err.Error())
	default:
		return itemErr(out, "read: "+err.Error())
	}
}

// supportedEncodingName 判断归一化后的编码名是否被本工具支持。
func supportedEncodingName(name string) bool {
	switch normalizeEncoding(name) {
	case encodingx.UTF8, encodingx.UTF16LE, encodingx.UTF16BE,
		encodingx.GBK, encodingx.GB18030, encodingx.ShiftJIS, encodingx.Big5, encodingx.EUCKR:
		return true
	}
	return false
}

// targetSupportsBOM 判断目标编码是否能携带 BOM（仅 UTF-8 / UTF-16）。
func targetSupportsBOM(name string) bool {
	switch normalizeEncoding(name) {
	case encodingx.UTF8, encodingx.UTF16LE, encodingx.UTF16BE:
		return true
	}
	return false
}

// candidateNames 把候选编码提取为编码名数组（file_iconv 的 candidates 形态）。
func candidateNames(cands []encodingx.Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Encoding)
	}
	return out
}
