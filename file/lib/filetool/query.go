// query.go — file_find / file_detect / file_iconv。
package filetool

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugins/file/lib/cli"
	"github.com/lsong98sh/chonkpilot-plugins/file/lib/fileops"
)

const (
	defaultFindLimit  = 2000
	hardFindLimit     = 2000
	defaultMaxHash    = int64(512 << 20)
	defaultHitsPerF   = 100
	defaultSampleSize = 64 << 10
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
	return finish(tool, req.purpose, results)
}

// findOne 执行一次查找。
func findOne(root string, t map[string]any) map[string]any {
	out := map[string]any{"root": root}

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
	globPat := strings.TrimSpace(asString(t, "glob"))
	grepPat := asString(t, "grep")
	useRegex := asBool(t, "regex", false)
	depth := asInt(t, "depth", 0)
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

	var entries []findEntry
	matched := 0
	scanned := 0
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
		if d.IsDir() {
			if filter.SkipDirs[strings.ToLower(d.Name())] || filter.SkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if depth > 0 && strings.Count(rel, "/")+1 > depth {
				return filepath.SkipDir
			}
			return nil
		}
		if depth > 0 && strings.Count(rel, "/")+1 > depth {
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
		entry := findEntry{Rel: rel, Size: info.Size()}
		if meta != "none" {
			entry.Mtime = info.ModTime().UTC().Format(time.RFC3339)
		}
		if grepPat != "" {
			hits, herr := grepFile(p, grepPat, re, maxHits)
			if herr != nil || len(hits) == 0 {
				return nil
			}
			entry.Hits = hits
		}
		matched++
		if len(entries) < limit {
			entries = append(entries, entry)
		}
		return nil
	})
	if walkErr != nil {
		return itemErr(out, walkErr.Error())
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	truncated := matched > len(entries)

	// meta=hash：只对已返回条目计算，受 maxHashBytes 限制。
	hashTruncated := false
	if meta == "hash" {
		var used int64
		for i := range entries {
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
	if grepPat == "" {
		out["matched_total"] = matched
	} else {
		out["scanned"] = scanned
		out["matched_total"] = matched
	}
	out["hash_truncated"] = hashTruncated
	out["limit"] = limit

	switch strings.ToLower(strings.TrimSpace(asString(t, "output"))) {
	case "list":
		lines := make([]string, 0, len(entries))
		for _, e := range entries {
			lines = append(lines, e.Rel)
		}
		out["lines"] = lines
	case "tree":
		out["text"] = renderTree(entries)
	default:
		out["items"] = entries
	}
	return itemOK(out)
}

// grepFile 在单个文件中查找命中行。
func grepFile(path, pattern string, re *regexp.Regexp, maxHits int) ([]findHit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if fileops.IsBinaryBytes(data) {
		return nil, fmt.Errorf("binary")
	}
	text := fileops.DecodeText(data)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	hits := make([]findHit, 0, 4)
	for i, ln := range lines {
		hit := false
		if re != nil {
			hit = re.MatchString(ln)
		} else {
			hit = strings.Contains(ln, pattern)
		}
		if !hit {
			continue
		}
		hits = append(hits, findHit{Line: i + 1, Text: truncRunes(ln, 500)})
		if len(hits) >= maxHits {
			break
		}
	}
	return hits, nil
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

// parseFindPeriod 解析 YYYY-MM-DD。
func parseFindPeriod(from, to string) (findPeriodX, string) {
	var p findPeriodX
	if s := strings.TrimSpace(from); s != "" {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return p, "bad from (want YYYY-MM-DD)"
		}
		p.from = t
	}
	if s := strings.TrimSpace(to); s != "" {
		t, err := time.Parse("2006-01-02", s)
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

// truncRunes 按 rune 截断。
func truncRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
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
	return finish(tool, req.purpose, results)
}

// detectOne 检测单个文件。
func detectOne(path string, t map[string]any) map[string]any {
	out := map[string]any{"path": path}
	n := asInt(t, "sampleBytes", defaultSampleSize)
	if n < 64 {
		n = 64
	}
	data, _, err := readHead(path, int64(n))
	if err != nil {
		return itemErr(out, err.Error())
	}
	d := detectEncoding(data)
	out["binary"] = d.Binary
	out["bom"] = d.BOM
	if d.Encoding != "" {
		out["encoding"] = d.Encoding
	}
	out["confidence"] = d.Confidence
	if len(d.Candidates) > 0 {
		out["candidates"] = d.Candidates
	}
	if d.Sample != "" {
		out["sample"] = d.Sample
	}
	return itemOK(out)
}

// runIconv 实现 file_iconv。
func runIconv(tool string, args map[string]any) *cli.Result {
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
		results = append(results, iconvOne(path, t))
	}
	return finish(tool, req.purpose, results)
}

// iconvOne 转换单个文件编码。
func iconvOne(path string, t map[string]any) map[string]any {
	out := map[string]any{"path": path}
	to := normalizeEncoding(asString(t, "to"))
	if to == "" || to == "auto" {
		return itemErr(out, "to is required")
	}
	oldData, err := os.ReadFile(path)
	if err != nil {
		return itemErr(out, err.Error())
	}
	before, _ := md5File(path)
	if want := strings.TrimSpace(asString(t, "expectMd5")); want != "" && !strings.EqualFold(want, before) {
		return itemSkip(map[string]any{"path": path, "md5": before}, "md5 mismatch")
	}

	from := normalizeEncoding(asString(t, "from"))
	var text string
	if from == "" || from == "auto" {
		det := detectEncoding(oldData)
		if det.Binary {
			return itemErr(out, "binary file (not a text encoding)")
		}
		if det.Confidence == "low" {
			cands := make([]string, 0, len(det.Candidates))
			for _, c := range det.Candidates {
				cands = append(cands, c.Encoding)
			}
			out["candidates"] = cands
			return itemErr(out, "encoding_undetermined (specify from explicitly)")
		}
		from = normalizeEncoding(det.Encoding)
	}
	if from == to {
		return itemSkip(map[string]any{"path": path, "from": from, "to": to}, "already_target")
	}
	decoded, derr := decodeByName(oldData, from)
	if derr != nil {
		return itemErr(out, "decode_error: "+derr.Error())
	}
	text = decoded

	bomMode := strings.ToLower(strings.TrimSpace(asString(t, "bom")))
	wantBOM := false
	switch bomMode {
	case "add":
		wantBOM = true
	case "strip":
		wantBOM = false
	default: // keep
		wantBOM = hasBOM(oldData)
	}
	data, actualBOM, eerr := encodeWithEncoding(text, to, wantBOM)
	if eerr != nil {
		return itemErr(out, "encode_error: "+eerr.Error())
	}
	if err := writeAtomic(path, data); err != nil {
		return itemErr(out, "write: "+err.Error())
	}
	out["from"] = from
	out["to"] = to
	if actualBOM {
		out["bom"] = to
	} else {
		out["bom"] = "none"
	}
	out["bytesBefore"] = len(oldData)
	out["bytesAfter"] = len(data)
	out["md5Before"] = before
	if after, err := md5File(path); err == nil {
		out["md5After"] = after
	}
	return itemOK(out)
}
