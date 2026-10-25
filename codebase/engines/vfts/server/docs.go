// docs.go：文档转换接入（Office / PDF → 文本）——文档类扩展名的收集门槛、转换缓存、
// HTTP 调用与降级规则。
//
// 设计约束（用户定稿）：
//   - **引擎不启动转换服务**：转换服务（src/mcps/markitdown）由用户在 MCP 配置页手动
//     注册/启动；引擎只按 plugin 下发的 endpoint/token 直连其 vfts 内部快通道
//     （`POST {endpoint}/vfts/convert`）。
//   - **文档类扩展名独立成组**（docExts，见 ext.go），仅在 `docs` 开启时参与收集；
//     命中后**不读原文当文本**，改为「查落盘缓存 → 未命中则 HTTP 转换 → 写缓存」。
//   - **降级**：服务不可用 → 整批跳过文档类（不写文件名 chunk，避免噪声）；
//     单文件失败 → 该文件降级为「仅索引文件名」（1 条 content = 相对路径的 chunk，loc 空）。
//     任何失败都不中断整库索引。
//   - **并发**：转换阶段走 worker 池（poolSize 可配），zvec 写入仍串行（调用方保证）。
package server

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DocServiceRunning / DocServiceAbsent —— docsService 状态取值。
	DocServiceRunning = "running"
	DocServiceAbsent  = "absent"

	// 文档类单文件上限（独立于非文档类的 maxFileBytes；多数 Office/PDF 远超 8MB）。
	defaultDocMaxBytes = int64(50) << 20 // 50MiB
	// 单文件转换文本上限（防御性；转换服务自身已按 2MiB 截断）。
	defaultDocTextMaxBytes = int64(2) << 20 // 2MiB
	// 调用转换服务的超时（≥ 转换器单文件 60s + 余量）。
	docConvertTimeout = 90 * time.Second
	// 转换阶段 worker 池默认并发。
	defaultDocWorkers = 6

	docCacheSubdir = "doc_text" // <workdir>/.chonkpilot/vfts/doc_text（.chonkpilot 强制排除）
)

// DocsMeta 文档转换接入的配置与最近一次索引运行态（落 meta.json；**不含 token**）。
type DocsMeta struct {
	Enabled       bool   `json:"enabled,omitempty"`
	Endpoint      string `json:"endpoint,omitempty"` // 形如 http://127.0.0.1:7317
	MaxBytes      int64  `json:"maxBytes,omitempty"`
	TextMaxBytes  int64  `json:"textMaxBytes,omitempty"`
	CacheDir      string `json:"cacheDir,omitempty"`
	ParserVersion string `json:"parserVersion,omitempty"` // 最近一次成功转换上报的解析器版本
	Service       string `json:"service,omitempty"`       // running | absent（最近一次索引时）
	Skipped       int    `json:"skipped,omitempty"`       // 整批跳过的文档数
	Failed        int    `json:"failed,omitempty"`        // 降级为「仅文件名」的文档数
}

// DocsConfig vfts_configure 下发的文档转换配置。
// 各字段**指针语义 = 「键是否下发」**：nil = 未提供（不改）；非 nil = 覆盖（**含空串/零值**）。
// 之所以要求「键存在即覆盖」，是为让「服务不可用」成为可表达状态——插件停服后必须能把
// Endpoint/Token 覆盖为空，否则引擎会残留上次的旧 endpoint、误判服务可用。
type DocsConfig struct {
	Enabled      *bool
	Endpoint     *string
	Token        *string
	MaxBytes     *int64
	TextMaxBytes *int64
	CacheDir     *string
}

// docsServiceOf 由配置推导服务状态（running = 开启且已配置 endpoint）。
func docsServiceOf(d DocsMeta) string {
	if d.Enabled && strings.TrimSpace(d.Endpoint) != "" {
		return DocServiceRunning
	}
	return DocServiceAbsent
}

// docMaxBytes 生效的文档单文件上限（<=0 → 默认 50MiB）。
func docMaxBytes(d DocsMeta) int64 {
	if d.MaxBytes > 0 {
		return d.MaxBytes
	}
	return defaultDocMaxBytes
}

// docTextMax 生效的转换文本上限（<=0 → 默认 2MiB）。
func docTextMax(d DocsMeta) int64 {
	if d.TextMaxBytes > 0 {
		return d.TextMaxBytes
	}
	return defaultDocTextMaxBytes
}

// docsMeta 读取文档接入配置副本（并发安全）。
func (w *Workspace) docsMeta() DocsMeta {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.meta.Docs
}

// docsAvailable 文档转换服务在当前配置下是否可用（开启 + 配置了 endpoint）。
func (w *Workspace) docsAvailable() bool {
	return docsServiceOf(w.docsMeta()) == DocServiceRunning
}

// docCacheDir 缓存目录：显式 CacheDir 优先，否则 <workdir>/.chonkpilot/vfts/doc_text。
func (w *Workspace) docCacheDir() string {
	d := w.docsMeta()
	if s := strings.TrimSpace(d.CacheDir); s != "" {
		return filepath.FromSlash(s)
	}
	return filepath.Join(filepath.FromSlash(w.Store), docCacheSubdir)
}

// docCacheKey 缓存键 = sha1(相对路径 | mtime | parser_version)（mtime 变化或解析器版本变化
// 即换键 → 天然失效重建；同一文件未变且解析器未变则命中）。
func docCacheKey(rel string, mtime int64, parserVersion string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("%s|%d|%s", rel, mtime, parserVersion)))
	return hex.EncodeToString(sum[:])
}

// ───────────────────────────── 缓存读写 ─────────────────────────────

// docLoc 一条定位（loc = 页码 / sheet 名 / slide 序号；Offset = **UTF-8 字节偏移**）。
type docLoc struct {
	Loc    string `json:"loc"`
	Offset int64  `json:"offset"`
}

// docCacheData 缓存条目（文本 + 定位）。
type docCacheData struct {
	Text    string   `json:"text"`
	LocKind string   `json:"locKind,omitempty"`
	Locs    []docLoc `json:"locs,omitempty"`
}

// readDocCache 读缓存（不存在 → ok=false）。缓存文件为 JSON（`.json` 后缀，名实相符）。
func readDocCache(path string) (docCacheData, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return docCacheData{}, false
	}
	var d docCacheData
	if err := json.Unmarshal(b, &d); err != nil {
		return docCacheData{}, false
	}
	return d, true
}

// writeDocCache 原子写缓存（临时文件 + rename）；失败仅告警、不影响本次索引结果。
func writeDocCache(path string, d docCacheData) {
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// ───────────────────────────── 转换服务调用 ─────────────────────────────

// convertRequest / vfts/convert 请求体。
// Root = 允许根（本 workspace 根）：转换服务据此校验 Path 的 realpath 不越界（防任意文件读）。
type convertRequest struct {
	Path     string `json:"path"`
	MaxBytes int64  `json:"max_bytes,omitempty"`
	Root     string `json:"root,omitempty"`
}

// convertResponse / vfts/convert 响应体（成功与失败共形，见 src/mcps/markitdown/server.py）。
type convertResponse struct {
	OK            bool     `json:"ok"`
	Text          string   `json:"text"`
	LocKind       string   `json:"loc_kind"`
	Locs          []docLoc `json:"locs"`
	Truncated     bool     `json:"truncated"`
	ParserVersion string   `json:"parser_version"`
	ElapsedMS     int      `json:"elapsed_ms"`
	ErrorCode     string   `json:"error_code"`
	Message       string   `json:"message"`
}

var docHTTPClient = &http.Client{Timeout: docConvertTimeout}

// callConvert 调转换服务（POST {endpoint}/vfts/convert，带 X-Chonk-Token）。
// 返回结构化结果（失败返回 error，调用方据此降级）。
func callConvert(ctx context.Context, endpoint, token string, req convertRequest) (convertResponse, error) {
	var out convertResponse
	url := strings.TrimRight(endpoint, "/") + "/vfts/convert"
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("X-Chonk-Token", token)
	resp, err := docHTTPClient.Do(hreq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("convert 应答解析失败 (http %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("convert http %d: %s", resp.StatusCode, out.Message)
	}
	if !out.OK {
		return out, fmt.Errorf("convert 失败: %s (%s)", out.Message, out.ErrorCode)
	}
	return out, nil
}

// ───────────────────────────── 分块与 loc 映射 ─────────────────────────────

// splitChunks 按行切块（与既有纯文本通道同一口径）；locs 非空时给每块打 loc。
// loc 映射 = 该块**首行起点字节偏移**所落在的最后一个 loc 区间（locs 按 offset 升序）。
func splitChunks(path, text string, locs []docLoc) []chunk {
	text = normalizeText(text)
	lines := strings.Split(text, "\n")
	// 每行起始的 UTF-8 字节偏移（含结尾换行计 1 字节，'\n' 即 1 字节）。
	starts := make([]int64, len(lines))
	var off int64
	for i, ln := range lines {
		starts[i] = off
		off += int64(len(ln)) + 1
	}
	var out []chunk
	i := 0
	for i < len(lines) {
		start := i
		var buf strings.Builder
		for i < len(lines) {
			line := lines[i]
			if buf.Len() > 0 && (buf.Len()+len(line) > maxChunkChars || i-start >= maxChunkLines) {
				break
			}
			buf.WriteString(line)
			buf.WriteString("\n")
			i++
			if buf.Len() >= maxChunkChars {
				break
			}
		}
		txt := buf.String()
		if strings.TrimSpace(txt) == "" {
			continue
		}
		out = append(out, chunk{path: path, line: start + 1, text: txt, loc: locAt(locs, starts[start])})
	}
	return out
}

// locAt 返回 byteOff 落在的最后一个 loc 区间（locs 须按 offset 升序；无 ≤byteOff 的 → ""）。
func locAt(locs []docLoc, byteOff int64) string {
	res := ""
	for _, l := range locs {
		if l.Offset <= byteOff {
			res = l.Loc
			continue
		}
		break
	}
	return res
}

// filenameChunk 降级 chunk：仅索引文件名（content = 相对路径，loc 空）。
func filenameChunk(rel string) []chunk {
	return []chunk{{path: rel, line: 1, text: rel}}
}

// ───────────────────────────── 文档分块（缓存 → 转换）─────────────────────────────

// chunksOfDoc 文档类文件分块：先查落盘缓存（命中不调 HTTP），未命中则转换并写缓存。
// 返回 degraded=true 表示该文件转换失败、已降级为「仅文件名」。
func (w *Workspace) chunksOfDoc(e fileEntry) (chunks []chunk, degraded bool, err error) {
	d := w.docsMeta()
	abs := filepath.FromSlash(joinPath(w.Dir, e.path))
	fi, serr := os.Stat(abs)
	if serr != nil {
		return filenameChunk(e.path), true, nil // 读不到 stat → 降级
	}
	mtime := fi.ModTime().UnixNano()

	// 1) 缓存命中（键含 mtime + parser_version）→ 直接用，不调 HTTP
	cachePath := filepath.Join(w.docCacheDir(), docCacheKey(e.path, mtime, d.ParserVersion)+".json")
	if data, ok := readDocCache(cachePath); ok {
		return splitChunks(e.path, data.Text, data.Locs), false, nil
	}

	// 2) 服务不可用 → 降级（不发起请求）
	if !w.docsAvailable() {
		return filenameChunk(e.path), true, nil
	}
	token := w.docsTokenValue()
	if strings.TrimSpace(token) == "" {
		return filenameChunk(e.path), true, nil
	}

	// 3) HTTP 转换 → 写缓存（键用**返回的** parser_version，保证下次命中）
	ctx, cancel := context.WithTimeout(context.Background(), docConvertTimeout)
	defer cancel()
	resp, cerr := callConvert(ctx, d.Endpoint, token, convertRequest{
		Path: abs, MaxBytes: docMaxBytes(d), Root: filepath.FromSlash(w.Dir),
	})
	if cerr != nil {
		return filenameChunk(e.path), true, nil
	}
	if resp.ParserVersion != "" && resp.ParserVersion != d.ParserVersion {
		w.setDocsParserVersion(resp.ParserVersion)
	}
	locs := resp.Locs
	sort.SliceStable(locs, func(i, j int) bool { return locs[i].Offset < locs[j].Offset })
	text := resp.Text
	if max := docTextMax(d); max > 0 && int64(len(text)) > max {
		text = truncateBytes(text, max)
	}
	writeDocCache(filepath.Join(w.docCacheDir(), docCacheKey(e.path, mtime, resp.ParserVersion)+".json"),
		docCacheData{Text: text, LocKind: resp.LocKind, Locs: locs})
	return splitChunks(e.path, text, locs), false, nil
}

// docsTokenValue 读内存 token（并发安全）。
func (w *Workspace) docsTokenValue() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.docsToken
}

// setDocsParserVersion 记录最近一次成功转换的解析器版本。
func (w *Workspace) setDocsParserVersion(v string) {
	w.mu.Lock()
	w.meta.Docs.ParserVersion = v
	w.mu.Unlock()
}

// truncateBytes 按字节上限截断（回退到最近的有效 UTF-8 边界，不切断多字节字符）。
func truncateBytes(s string, max int64) string {
	if max <= 0 || int64(len(s)) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// ───────────────────────────── 并发转换（worker 池）─────────────────────────────

// convertDocs 用 worker 池并发处理文档类文件的转换（把文本切块结果填入 entries）。
// 返回值 = 每个 index（entries 下标）对应的高层切块结果；调用方随后**串行**写入 zvec。
// workers<=0 → defaultDocWorkers。
func (w *Workspace) convertDocs(entries []fileEntry, indices []int, workers int) map[int]docChunkOut {
	if workers <= 0 {
		workers = defaultDocWorkers
	}
	out := make(map[int]docChunkOut, len(indices))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for _, idx := range indices {
		e := entries[idx]
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, e fileEntry) {
			defer wg.Done()
			defer func() { <-sem }()
			cs, degraded, _ := w.chunksOfDoc(e)
			mu.Lock()
			out[idx] = docChunkOut{chunks: cs, degraded: degraded}
			mu.Unlock()
		}(idx, e)
	}
	wg.Wait()
	return out
}

// docChunkOut 文档分块结果（degraded = 降级为「仅文件名」）。
type docChunkOut struct {
	chunks   []chunk
	degraded bool
}
