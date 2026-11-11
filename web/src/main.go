// chonkpilot-web —— 联网能力插件（web_search / web_fetch / web_download 共用一个配置页）。
//
// 形态：manifest 插件 + 进程内 Streamable HTTP MCP。
// 工具（namespace="-" → LLM 可见名即原名）：
//   - web_search   DeepSeek 网页搜索（`POST /v1/tools/search`，见 search.go）
//   - web_fetch    DeepSeek 网页抓取（`POST /v1/tools/fetch`，返回标题 + Markdown 正文，见 fetch.go）
//   - web_download 本地直连抓取：**返回原始内容**（文本原样 / 二进制 base64）或落盘下载；
//     重定向可配置上限（见 download.go）
//
// 配置：宿主 config.get/set 管理（`plugins.web.*`），密钥只掩码回显。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/contract"
	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "2.0.0"

// apiResponseMaxBytes 是 web_search / web_fetch 上游响应的读取上限（8 MiB）。
// 超限不再静默截断：直接返回明确错误（截断会让 JSON 解析必然失败）。
const apiResponseMaxBytes = 8 << 20

// contentHardMaxBytes 是 web_fetch / web_download 单次返回内容的硬上限（8 MiB）。
// 参数 maxBytes 超过该值直接报错：内容模式最终会进 LLM 上下文（二进制再 base64 放大 4/3），
// 更大文件应走 web_download 落盘模式（maxMB/流式写盘）。
const contentHardMaxBytes = 8 << 20

// web_search 正文预算：need_content=true 时按总字节预算截断各条 content。
const (
	searchContentBudgetDefault = 256 << 10 // 256 KiB
	searchContentBudgetMax     = 8 << 20   // 8 MiB
)

// apiAttemptTimeoutMax 单次上游尝试的超时上限：3 次尝试 + 1s/2s 退避 <= 168s，
// 保证在 manifest 的 180s 网关超时内完成，避免必然转后台。
const apiAttemptTimeoutMax = 55 * time.Second

// toolsFS 是工具契约（`*.tool.md`）的 embed 源：插件自持，产物不带契约目录。
//
//go:embed all:tools
var toolsFS embed.FS

// toolsRoot 是 embed 内契约目录名（`tools/web/`）。
const toolsRoot = "tools/web"

type searcher struct {
	p   *pluginsdk.Plugin
	cfg *configManager

	mu      sync.Mutex
	lastAt  time.Time
	lastErr string
	lastN   int
}

func main() {
	p, err := pluginsdk.New(pluginsdk.Options{
		ID:      "web",
		Name:    "联网能力",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("web: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	s := &searcher{p: p, cfg: newConfigManager(p)}

	// 契约 → 工具注册：schema / description / 策略（async/timeout/on_timeout/hot）均取自 md，
	// `_meta.source="md"` 由 contract.MetaFor 写入。
	contracts, err := loadContracts()
	if err != nil {
		slog.Error("web: 加载工具契约失败", "err", err)
		os.Exit(1)
	}
	if missing := registerTools(p, s, contracts); len(missing) > 0 {
		slog.Error("web: 契约缺少实现", "tools", strings.Join(missing, ","))
		os.Exit(1)
	}

	p.OnRequest("test", s.test)
	p.OnRequest("status", s.status)
	p.OnRequest("download-dir", s.downloadDir)
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/web/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := s.cfg.Init(context.Background()); err != nil {
		slog.Warn("web: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range s.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("web: 运行结束", "err", err)
	}
}

// loadContracts 读取 embed 内的契约（`tools/web/*.tool.md`，按名称排序）。
func loadContracts() ([]contract.Tool, error) {
	entries, err := fs.ReadDir(toolsFS, toolsRoot)
	if err != nil {
		return nil, fmt.Errorf("读取契约目录 %s: %w", toolsRoot, err)
	}
	out := make([]contract.Tool, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tool.md") {
			continue
		}
		raw, err := fs.ReadFile(toolsFS, toolsRoot+"/"+e.Name())
		if err != nil {
			return nil, err
		}
		tool, err := contract.Parse(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if tool.Name == "" {
			continue
		}
		out = append(out, tool)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// registerTools 按契约注册工具（handler 表按工具名分派），返回缺少实现的工具名。
// hot 由契约 `hot: true` 经 `_meta` 传给 SDK（SDK 读取 meta.hot），无需再调用 HotTool。
func registerTools(p *pluginsdk.Plugin, s *searcher, tools []contract.Tool) []string {
	handlers := map[string]pluginsdk.ToolFunc{
		"web_search":   s.search,
		"web_fetch":    s.fetch,
		"web_download": s.download,
	}
	seen := make(map[string]bool, len(handlers))
	for _, tool := range tools {
		fn, ok := handlers[tool.Name]
		if !ok {
			slog.Warn("web: 契约无对应实现，跳过", "tool", tool.Name)
			continue
		}
		p.ToolWithMeta(tool.Name, tool.Description, tool.Schema, contract.MetaFor(tool), fn)
		seen[tool.Name] = true
	}
	var missing []string
	for name := range handlers {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// ── web_search ─────────────────────────

type result struct {
	Title            string  `json:"title"`
	URL              string  `json:"url"`
	Snippet          string  `json:"snippet,omitempty"`
	Summary          string  `json:"summary,omitempty"`
	Content          string  `json:"content,omitempty"`
	ContentTruncated bool    `json:"content_truncated,omitempty"`
	SiteName         string  `json:"site_name,omitempty"`
	SiteIcon         string  `json:"site_icon,omitempty"`
	Date             string  `json:"date,omitempty"`
	Author           string  `json:"author,omitempty"`
	Authority        float64 `json:"authority,omitempty"`
	Score            float64 `json:"score,omitempty"`
	RerankScore      float64 `json:"rerank_score,omitempty"`
}

type searchResponse struct {
	Results   []result `json:"results"`
	Data      []result `json:"data"`
	WebSearch []result `json:"web_search"`
}

func (s *searcher) search(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := s.cfg.Settings()
	query := strings.TrimSpace(str(args["query"]))
	if query == "" {
		return nil, errors.New("query 不能为空")
	}
	if len([]rune(query)) > 128 {
		return nil, errors.New("query 超过 128 字符")
	}
	apiKey := cfg.APIKey
	if apiKey == "" {
		return nil, errors.New("web 未配置 DeepSeek API Key：请在 插件 → 联网能力 中填写")
	}
	count := cfg.Count
	requestedCount := count
	if n := toInt(args["count"]); n > 0 {
		count = n
		requestedCount = n
	}
	if count < 1 {
		count = 1
	}
	if count > 50 {
		count = 50
	}
	budget := searchContentBudgetDefault
	if n := toInt(args["max_content_bytes"]); n > 0 {
		budget = n
	}
	if budget > searchContentBudgetMax {
		budget = searchContentBudgetMax
	}
	needContent := cfg.NeedContent
	if v, ok := args["need_content"].(bool); ok {
		needContent = v
	}
	body := map[string]any{"query": query, "count": count, "need_content": needContent}
	if q := strings.TrimSpace(str(args["ori_question"])); q != "" {
		body["ori_question"] = q
	}
	if loc := strings.TrimSpace(str(args["localization"])); loc != "" {
		body["localization"] = loc
	}
	raw, err := s.doAPI(ctx, cfg, cfg.SearchPath, body)
	if err != nil {
		s.record(0, err)
		return nil, err
	}
	var resp searchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		s.record(0, err)
		return nil, fmt.Errorf("搜索响应解析失败: %w", err)
	}
	items := resp.Results
	if len(items) == 0 {
		items = resp.Data
	}
	if len(items) == 0 {
		items = resp.WebSearch
	}
	if items == nil {
		items = []result{}
	}
	contentBytes, contentTruncated := applySearchContentBudget(items, budget)
	s.record(len(items), nil)
	out := map[string]any{
		"provider": "deepseek", "results": items,
		"count": len(items), "count_requested": requestedCount,
		"content_bytes": contentBytes, "content_truncated": contentTruncated,
	}
	if len(items) == 0 {
		out["note"] = "no_results"
		out["hint"] = "未检索到结果，可放宽或改写 query，或降低 count 后重试。"
	}
	return out, nil
}

// applySearchContentBudget 按总字节预算截断各条 content（仅 need_content=true 时有内容），
// 返回实际保留的正文总字节数与是否有条目被截断。snippet/summary 不受影响。
func applySearchContentBudget(items []result, budget int) (int, bool) {
	if budget <= 0 {
		for i := range items {
			if items[i].Content != "" {
				items[i].Content = ""
				items[i].ContentTruncated = true
			}
		}
		return 0, len(items) > 0
	}
	remaining := budget
	used := 0
	truncated := false
	for i := range items {
		c := items[i].Content
		if c == "" {
			continue
		}
		if len(c) <= remaining {
			used += len(c)
			remaining -= len(c)
			continue
		}
		truncated = true
		if remaining <= 0 {
			items[i].Content = ""
			items[i].ContentTruncated = true
			continue
		}
		items[i].Content = truncateUTF8(c, remaining)
		items[i].ContentTruncated = true
		used += len(items[i].Content)
		remaining = 0
	}
	return used, truncated
}

// doAPI 调用 DeepSeek 工具 API（搜索 / 抓取共用）：Bearer 鉴权 + 可重试错误重试 3 次。
func (s *searcher) doAPI(ctx context.Context, cfg settings, path string, body map[string]any) ([]byte, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	u, err := url.Parse(base + path)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("baseUrl 非法（必须 http/https）")
	}
	payload, _ := json.Marshal(body)
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > apiAttemptTimeoutMax {
		timeout = apiAttemptTimeoutMax
	}
	client := &http.Client{Timeout: timeout}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(payload)))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		resp, doErr := client.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, apiResponseMaxBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("读取联网响应失败: %w", readErr)
		}
		if len(raw) > apiResponseMaxBytes {
			if path == cfg.FetchPath {
				return nil, fmt.Errorf("响应超过上限 %dMiB：该页面过大，请改用 web_download 下载后本地读取", apiResponseMaxBytes>>20)
			}
			return nil, fmt.Errorf("响应超过上限 %dMiB：请降低 count 或关闭 need_content，大文件请改用 web_download", apiResponseMaxBytes>>20)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return raw, nil
		}
		lastErr = fmt.Errorf("%s", friendlyError(resp.StatusCode, raw))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired ||
			resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound ||
			resp.StatusCode == http.StatusBadRequest {
			return nil, lastErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("联网服务不可用")
	}
	return nil, fmt.Errorf("%w（可稍后重试；页面较大或需要原始内容请改用 web_download）", lastErr)
}

func friendlyError(code int, raw []byte) string {
	snippet := strings.TrimSpace(string(raw))
	if len(snippet) > 300 {
		snippet = snippet[:300]
	}
	switch code {
	case http.StatusUnauthorized:
		return "DeepSeek API Key 无效或未配置（401）"
	case http.StatusPaymentRequired:
		return "DeepSeek 余额不足（402）"
	case http.StatusTooManyRequests:
		return "DeepSeek 请求过于频繁（429），已重试"
	case http.StatusBadRequest:
		return "请求参数被拒绝（400）：" + snippet
	default:
		if code >= 500 {
			return fmt.Sprintf("联网服务暂不可用（%d），已重试", code)
		}
		return fmt.Sprintf("请求失败（%d）：%s", code, snippet)
	}
}

func (s *searcher) record(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAt = time.Now()
	s.lastN = n
	if err != nil {
		s.lastErr = err.Error()
	} else {
		s.lastErr = ""
	}
}

// ── UI RPC ─────────────────────────

func (s *searcher) test(ctx context.Context, _ map[string]any) (any, error) {
	cfg := s.cfg.Settings()
	if cfg.APIKey == "" {
		return map[string]any{"ok": false, "error": "未配置 API Key"}, nil
	}
	start := time.Now()
	raw, err := s.doAPI(ctx, cfg, cfg.SearchPath, map[string]any{
		"query": "DeepSeek", "count": 1, "need_content": false,
	})
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "ms": ms}, nil
	}
	var resp searchResponse
	_ = json.Unmarshal(raw, &resp)
	title := ""
	if len(resp.Results) > 0 {
		title = resp.Results[0].Title
	}
	return map[string]any{"ok": true, "ms": ms, "title": title}, nil
}

func (s *searcher) status(_ context.Context, _ map[string]any) (any, error) {
	cfg := s.cfg.Settings()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{
		"configured": cfg.APIKey != "",
		"last_at":    "",
		"last_count": s.lastN,
		"last_error": s.lastErr,
		"base_url":   cfg.BaseURL,
	}
	if !s.lastAt.IsZero() {
		out["last_at"] = s.lastAt.Format(time.RFC3339)
	}
	return out, nil
}

// downloadDir 返回 web_download 的生效默认值（配置页展示 / 「打开目录」按钮用）。
func (s *searcher) downloadDir(_ context.Context, _ map[string]any) (any, error) {
	cfg := s.cfg.Settings()
	dir := strings.TrimSpace(cfg.DownloadDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return map[string]any{"dir": "", "error": err.Error()}, nil
		}
		dir = filepath.Join(home, "Downloads", "chonkpilot")
	}
	if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	return map[string]any{
		"dir":          dir,
		"maxMB":        cfg.DownloadMaxMB,
		"exts":         cfg.DownloadExts,
		"overwrite":    cfg.DownloadOverwrite,
		"fetchMaxMB":   cfg.FetchMaxMB,
		"maxRedirects": cfg.MaxRedirects,
		"allowPrivate": cfg.AllowPrivate,
	}, nil
}

// ── helpers ─────────────────────────

// 复用层别名：参数取值统一由 pluginsdk 提供（三个 web 工具共用）。
var (
	str   = pluginsdk.Str
	toInt = pluginsdk.Int
)
