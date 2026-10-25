package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeConverter 起一个 mock 转换服务（httptest），统计请求数，返回可控转换结果。
// body 参数按文件扩展名/名字决定响应；默认成功返回 text。
type fakeConverter struct {
	srv      *httptest.Server
	calls    int32
	wantTok  string
	text     string
	locKind  string
	locs     []docLoc
	parser   string
	failCode string
}

func newFakeConverter(t *testing.T, wantTok, text string) *fakeConverter {
	t.Helper()
	fc := &fakeConverter{wantTok: wantTok, text: text, parser: "markitdown-mcp/0.1.0 (markitdown/x)"}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fc.calls, 1)
		if r.URL.Path != "/vfts/convert" {
			http.NotFound(w, r)
			return
		}
		if fc.wantTok != "" && r.Header.Get("X-Chonk-Token") != fc.wantTok {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": "unauthorized"})
			return
		}
		if fc.failCode != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": fc.failCode, "message": "boom"})
			return
		}
		resp := map[string]any{
			"ok": true, "text": fc.text, "loc_kind": fc.locKind, "truncated": false,
			"parser_version": fc.parser, "elapsed_ms": 1,
		}
		if len(fc.locs) > 0 {
			resp["locs"] = fc.locs
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeConverter) count() int { return int(atomic.LoadInt32(&fc.calls)) }

func writeDoc(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDocExtsIndependentOfExts：文档类扩展名独立成组——仅在 docs 开启时参与收集，
// 且不会被写进 vfts.exts 的普通文本通道收集（docs 关 → 不收集）。
func TestDocExtsIndependentOfExts(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "a.go", "package a\n")
	writeDoc(t, dir, "a.pdf", "%PDF-1.4 fake\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()

	// docs 关：仅 .go（exts 显式含 .pdf 也不收 —— 文档支持只由 docs 决定）
	on := false
	if err := w.Configure(nil, []string{".go", ".pdf"}, nil, nil, &DocsConfig{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	entries, skipped, err := w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.go" {
		t.Fatalf("docs 关：应收 a.go，实际 %v", got)
	}
	if skipped != 0 {
		t.Fatalf("docs 关：不应计跳过，实际 %d", skipped)
	}

	// docs 开（endpoint 已配）→ .go + .pdf 都收
	yes := true
	if err := w.Configure(nil, []string{".go"}, nil, nil, &DocsConfig{Enabled: &yes, Endpoint: strp("http://127.0.0.1:9")}); err != nil {
		t.Fatal(err)
	}
	entries, _, err = w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.go,a.pdf" {
		t.Fatalf("docs 开：应收 a.go,a.pdf，实际 %v", got)
	}
}

// TestDocsServiceAbsentSkipsBatch：docs 开但未配置 endpoint（服务不可用）→ 整批跳过文档类
// （不收集 → 不写文件名 chunk），并计入 skipped。
func TestDocsServiceAbsentSkipsBatch(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "a.go", "package a\n")
	writeDoc(t, dir, "a.docx", "fake docx\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true
	if err := w.Configure(nil, []string{".go"}, nil, nil, &DocsConfig{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	entries, skipped, err := w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.go" {
		t.Fatalf("服务不可用：文档类应整批跳过，实际 %v", got)
	}
	if skipped != 1 {
		t.Fatalf("docsSkipped 应为 1，实际 %d", skipped)
	}
	if s := w.Status(); s.DocsService != DocServiceAbsent {
		t.Fatalf("status.docsService 应为 absent，实际 %q", s.DocsService)
	}
}

// TestDocsConfigureEmptyEndpointClearsStale：引擎 Configure 的 docs 字段为「键存在即覆盖（含空值）」——
// 下发空 endpoint/token **必须清掉旧值**（不得回落残留），docsAvailable 随之为 false；键未下发（nil）才不改。
// 这是「转换服务停掉后引擎仍按旧 endpoint 转换」缺陷的直接回归。
func TestDocsConfigureEmptyEndpointClearsStale(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true

	// 服务可用：下发真实 endpoint（带尾斜杠 → 归一去尾）+ token
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{
		Enabled: &yes, Endpoint: strp("http://127.0.0.1:7317/"), Token: strp("tok"),
	}); err != nil {
		t.Fatal(err)
	}
	if !w.docsAvailable() {
		t.Fatal("已配置 endpoint 时 docsAvailable 应为 true")
	}
	if got := w.Meta().Docs.Endpoint; got != "http://127.0.0.1:7317" {
		t.Fatalf("endpoint 应去尾斜杠：%q", got)
	}

	// 服务不可用：下发**空串** endpoint + token → 覆盖旧值（清空）
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Endpoint: strp(""), Token: strp("")}); err != nil {
		t.Fatal(err)
	}
	if w.docsAvailable() {
		t.Fatal("endpoint 清空后 docsAvailable 必须为 false（不得回落旧值）")
	}
	if got := w.Meta().Docs.Endpoint; got != "" {
		t.Fatalf("空 endpoint 应覆盖旧值，实际残留 %q", got)
	}
	if got := w.docsTokenValue(); got != "" {
		t.Fatalf("空 token 应清空内存 token，实际残留 %q", got)
	}
	if s := w.Status().DocsService; s != DocServiceAbsent {
		t.Fatalf("docsService 应为 absent，实际 %q", s)
	}

	// 键未下发（nil）→ 不改（保持空）
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Enabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if got := w.Meta().Docs.Endpoint; got != "" {
		t.Fatalf("nil endpoint 不应改（应保持空），实际 %q", got)
	}

	// 阈值 / 缓存目录同为「键存在即覆盖（含零值/空值）」
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{
		MaxBytes: i64p(1 << 20), TextMaxBytes: i64p(1 << 10), CacheDir: strp("/tmp/doc"),
	}); err != nil {
		t.Fatal(err)
	}
	if m := w.Meta().Docs; m.MaxBytes != 1<<20 || m.TextMaxBytes != 1<<10 || m.CacheDir != "/tmp/doc" {
		t.Fatalf("阈值/缓存目录未按值覆盖：%+v", m)
	}
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{
		MaxBytes: i64p(0), TextMaxBytes: i64p(0), CacheDir: strp(""),
	}); err != nil {
		t.Fatal(err)
	}
	m := w.Meta().Docs
	if m.MaxBytes != 0 || m.TextMaxBytes != 0 || m.CacheDir != "" {
		t.Fatalf("零值/空值应覆盖：%+v", m)
	}
	if docMaxBytes(m) != defaultDocMaxBytes || docTextMax(m) != defaultDocTextMaxBytes {
		t.Fatalf("零值覆盖后应回落默认上限：%+v", m)
	}
}

// TestDocsThreeStateToggle：可用 → 不可用 → 可用 三态切换——
// 不可用时文档类整批跳过（不进 Indexed → 不进清单保留集）；恢复后文档类重新收集/索引。
func TestDocsThreeStateToggle(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "a.go", "package a\n")
	writeDoc(t, dir, "a.docx", "fake docx\n")
	fc := newFakeConverter(t, "tk", "doc body\n")

	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true

	// ① 可用：文档类入收集集
	if err := w.Configure(nil, []string{".go"}, nil, nil, &DocsConfig{
		Enabled: &yes, Endpoint: strp(fc.srv.URL), Token: strp("tk"),
	}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.docx,a.go" {
		t.Fatalf("服务可用：应收 a.docx,a.go，实际 %v", got)
	}

	// ② 不可用：清空 endpoint（服务停掉）→ 整批跳过 + 不进 Indexed
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Endpoint: strp(""), Token: strp("")}); err != nil {
		t.Fatal(err)
	}
	if w.docsAvailable() {
		t.Fatal("服务不可用时 docsAvailable 应为 false")
	}
	entries, skipped, err := w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.go" {
		t.Fatalf("服务不可用：文档类应整批跳过，实际 %v", got)
	}
	if skipped != 1 {
		t.Fatalf("docsSkipped 应为 1，实际 %d", skipped)
	}
	res, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range res.Indexed {
		if f.Path == "a.docx" {
			t.Fatalf("服务不可用：文档类不得进 Indexed（否则会被清单 keep）：%+v", f)
		}
	}
	if s := w.Status(); s.DocsSkipped != 1 {
		t.Fatalf("status.docsSkipped 应为 1，实际 %d", s.DocsSkipped)
	}

	// ③ 再次可用：文档类重新收集并索引（转换服务恢复）
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{
		Endpoint: strp(fc.srv.URL), Token: strp("tk"),
	}); err != nil {
		t.Fatal(err)
	}
	if !w.docsAvailable() {
		t.Fatal("服务恢复后 docsAvailable 应为 true")
	}
	entries, _, err = w.collectFiles()
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(entries); strings.Join(got, ",") != "a.docx,a.go" {
		t.Fatalf("服务恢复：应收 a.docx,a.go，实际 %v", got)
	}
	res, err = w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range res.Indexed {
		if f.Path == "a.docx" && f.Chunks > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("服务恢复：文档类应重新索引（含块）：%+v", res.Indexed)
	}
}

// TestDocsConvertAndCacheHit：首次转换调用服务并落缓存；再次索引命中缓存**不重复转换**。
func TestDocsConvertAndCacheHit(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "汇报.docx", "fake docx bytes")

	fc := newFakeConverter(t, "tok-123", "季度汇报\n支持 Office 与 PDF 的全文索引 chonkpilot-office\n")
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{
		Enabled: &yes, Endpoint: strp(fc.srv.URL), Token: strp("tok-123"),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("initialize #1: %v", err)
	}
	if fc.count() != 1 {
		t.Fatalf("首次索引应转换 1 次，实际 %d", fc.count())
	}
	s := w.Status()
	if s.DocsService != DocServiceRunning {
		t.Fatalf("docsService 应为 running，实际 %q", s.DocsService)
	}
	if s.DocsParserVersion != "markitdown-mcp/0.1.0 (markitdown/x)" {
		t.Fatalf("docsParserVersion 未回写：%q", s.DocsParserVersion)
	}

	// 二次索引（同一文件未变 + 解析器版本未变）→ 走缓存，不再调用服务
	if _, err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatalf("initialize #2: %v", err)
	}
	if fc.count() != 1 {
		t.Fatalf("二次索引应命中缓存（总调用仍 1），实际 %d", fc.count())
	}
	hits, err := w.Query("chonkpilot-office", "", 20, "")
	if err != nil || len(hits) != 1 {
		t.Fatalf("转换正文应命中 1 处：%+v err=%v", hits, err)
	}
	if !strings.HasSuffix(hits[0].Path, "汇报.docx") {
		t.Fatalf("命中文件路径不符：%s", hits[0].Path)
	}
}

// TestDocsSingleFileFailureDegradesToFilename：单文件转换失败 → 降级为「仅文件名」chunk
// （content = 相对路径）+ 计入 docsFailed；不中断整库索引。
func TestDocsSingleFileFailureDegradesToFilename(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "broken.docx", "fake")
	writeDoc(t, dir, "ok.txt", "keep me\n")

	fc := newFakeConverter(t, "", "")
	fc.failCode = "encrypted"
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Enabled: &yes, Endpoint: strp(fc.srv.URL)}); err != nil {
		t.Fatal(err)
	}
	res, err := w.Initialize(nil, nil, nil)
	if err != nil {
		t.Fatalf("转换失败不得中断整库索引：%v", err)
	}
	if s := w.Status(); s.DocsFailed != 1 {
		t.Fatalf("docsFailed 应为 1，实际 %d", s.DocsFailed)
	}
	// 降级 = 1 条仅文件名 chunk（content = 相对路径）
	var got *IndexedFile
	for i := range res.Indexed {
		if res.Indexed[i].Path == "broken.docx" {
			got = &res.Indexed[i]
		}
	}
	if got == nil || got.Chunks != 1 {
		t.Fatalf("broken.docx 应降级为 1 条文件名 chunk：%+v", got)
	}
	if hits, err := w.Query("broken.docx", "", 20, ""); err != nil || len(hits) != 1 {
		t.Fatalf("降级 chunk（文件名）应可命中：%+v err=%v", hits, err)
	}
	if hits, err := w.Query("keep", "", 20, ""); err != nil || len(hits) != 1 {
		t.Fatalf("非文档文件应正常索引：%+v err=%v", hits, err)
	}
}

// TestDocCacheKeyParserVersionInvalidates：缓存键含 parser_version —— 版本变化即换键（缓存失效）。
func TestDocCacheKeyParserVersionInvalidates(t *testing.T) {
	k1 := docCacheKey("a.docx", 100, "v1")
	k2 := docCacheKey("a.docx", 100, "v2")
	k3 := docCacheKey("a.docx", 101, "v1")
	if k1 == k2 {
		t.Fatal("parser_version 变化应改变缓存键")
	}
	if k1 == k3 {
		t.Fatal("mtime 变化应改变缓存键")
	}
	if k1 != docCacheKey("a.docx", 100, "v1") {
		t.Fatal("同输入应得同键")
	}
}

// TestDocLocMappingByByteOffset：loc 映射——chunk 的 loc = 该块首行起点字节偏移落在的
// 最后一个 loc 区间（中文多字节下须按 UTF-8 字节计，而非字符/码点）。
func TestDocLocMappingByByteOffset(t *testing.T) {
	head := "第一页内容\n" // 5 个中文（各 3 字节）+ 换行 = 16 字节；码点数 = 6
	off2 := int64(len(head))
	byteLocs := []docLoc{{Loc: "1", Offset: 0}, {Loc: "2", Offset: off2}}
	charLocs := []docLoc{{Loc: "1", Offset: 0}, {Loc: "2", Offset: int64(len([]rune(head)))}}

	// locAt：按字节偏移取区间
	if got := locAt(byteLocs, 0); got != "1" {
		t.Fatalf("offset 0 应落 loc 1，实际 %q", got)
	}
	if got := locAt(byteLocs, off2); got != "2" {
		t.Fatalf("offset %d 应落 loc 2，实际 %q", off2, got)
	}
	if got := locAt(byteLocs, off2-1); got != "1" {
		t.Fatalf("offset %d 应落 loc 1，实际 %q", off2-1, got)
	}
	if badChars := int64(len([]rune(head))); byteLocs[1].Offset == badChars {
		t.Skip("样本无法区分字符/字节偏移")
	}
	// 反例：若按字符偏移，off2 会落错区间
	if got := locAt(charLocs, off2); got != "2" {
		t.Fatalf("字符偏移样本应与字节偏移不同（off2=%d char=%d）", off2, charLocs[1].Offset)
	}

	// 非文档类（无 locs）：loc 留空
	if cs := splitChunks("a.txt", "hello\n", nil); len(cs) != 1 || cs[0].loc != "" {
		t.Fatalf("非文档类 loc 应为空：%+v", cs)
	}
	// 单块（多行合并）：loc = 块首行（offset 0）区间 → 恒为 "1"
	if cs := splitChunks("a.pdf", "line one\nline two\n", byteLocs); len(cs) != 1 || cs[0].loc != "1" {
		t.Fatalf("多行合并块 loc 应取首行区间：%+v", cs)
	}
}

// TestConvertDocsConcurrentAllProcessed：并发转换（worker 池）覆盖全部文档文件（结果按序可索引）。
func TestConvertDocsConcurrentAllProcessed(t *testing.T) {
	dir := t.TempDir()
	var entries []fileEntry
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("d%d.docx", i)
		writeDoc(t, dir, name, "fake")
		entries = append(entries, fileEntry{path: name})
	}
	fc := newFakeConverter(t, "", "内容")
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Enabled: &yes, Endpoint: strp(fc.srv.URL), Token: strp("tok")}); err != nil {
		t.Fatal(err)
	}
	idx := []int{0, 1, 2, 3, 4, 5, 6, 7}
	out := w.convertDocs(entries, idx, 4)
	if len(out) != 8 {
		t.Fatalf("并发转换应覆盖 8 个文件，实际 %d", len(out))
	}
	for i := 0; i < 8; i++ {
		o, ok := out[i]
		if !ok || len(o.chunks) == 0 || o.degraded {
			t.Fatalf("文件 %d 转换结果异常：%+v ok=%v", i, o, ok)
		}
	}
	if fc.count() != 8 {
		t.Fatalf("应转换 8 次，实际 %d", fc.count())
	}
}

// TestDocsLocRoundTrip：loc 落库并从查询命中原样返回（document → zvec loc 字段 → hit.loc）。
func TestDocsLocRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, "doc.pdf", "%PDF-1.4 fake")

	// 造 80 行填充（= maxChunkLines）使第 2 页行**另起一块**；否则多行会合并为一块、
	// 其 loc 只取首行所在页（符合"chunk loc = 首行起点所在区间"的定稿语义）。
	var b strings.Builder
	for i := 0; i < maxChunkLines; i++ {
		b.WriteString("filler line\n")
	}
	head := b.String()
	text := head + "page two content\n"
	fc := newFakeConverter(t, "", text)
	fc.locKind = "page"
	fc.locs = []docLoc{{Loc: "1", Offset: 0}, {Loc: "2", Offset: int64(len(head))}}
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { Drop(dir); CloseAll() }()
	yes := true
	if err := w.Configure(nil, nil, nil, nil, &DocsConfig{Enabled: &yes, Endpoint: strp(fc.srv.URL), Token: strp("tok")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Initialize(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 查 "two"：应命中第 2 块 → loc = 2
	hits, err := w.Query("two", "", 20, "")
	if err != nil || len(hits) != 1 {
		t.Fatalf("应命中 1 处：%+v err=%v", hits, err)
	}
	if hits[0].Loc != "2" {
		t.Fatalf("命中 loc 应为 2，实际 %q", hits[0].Loc)
	}
	// 查 "filler"：应命中第 1 块 → loc = 1
	hits, err = w.Query("filler", "", 20, "")
	if err != nil || len(hits) != 1 {
		t.Fatalf("filler 应命中 1 处：%+v err=%v", hits, err)
	}
	if hits[0].Loc != "1" {
		t.Fatalf("filler 命中 loc 应为 1，实际 %q", hits[0].Loc)
	}
}

func paths(entries []fileEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.path)
	}
	return out
}

// strp 字符串取址（DocsConfig.Endpoint/Token/CacheDir 为指针语义「键是否下发」）。
func strp(s string) *string { return &s }

// i64p int64 取址（DocsConfig.MaxBytes/TextMaxBytes 同理）。
func i64p(n int64) *int64 { return &n }
