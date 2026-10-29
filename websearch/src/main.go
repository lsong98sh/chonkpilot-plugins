// chonkpilot-websearch —— 联网能力插件（三个 web 工具共用一个配置页）。
//
// 形态：manifest 插件 + 进程内 Streamable HTTP MCP。
// 工具（namespace="-" → LLM 可见名即原名）：
//   - web_search   检索（DeepSeek Search）
//   - web_fetch    抓单 URL → 可读文本（见 fetch.go）
//   - web_download 下载文件到本地（默认目录 / 上限 / 扩展名白名单可配，参数可覆盖；见 download.go）
//
// 配置：宿主 config.get/set 管理，密钥只掩码回显。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const pluginVersion = "1.0.0"

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
		ID:      "websearch",
		Name:    "联网搜索",
		Version: pluginVersion,
		Features: map[string]any{
			"mcp": "http",
			"ui":  true,
		},
	})
	if err != nil {
		slog.Error("websearch: 连接插件桥失败", "err", err)
		os.Exit(1)
	}
	defer p.Close()

	s := &searcher{p: p, cfg: newConfigManager(p)}
	p.Tool("web_search", "联网搜索（DeepSeek Search）。返回标题/链接/摘要/正文片段。", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query":        map[string]any{"type": "string", "description": "检索词（1-128 字符）"},
			"count":        map[string]any{"type": "integer", "description": "返回条数（1-50）"},
			"need_content": map[string]any{"type": "boolean", "description": "是否返回正文"},
			"ori_question": map[string]any{"type": "string", "description": "原始问题（供检索改写参考）"},
		},
		"required": []string{"query"},
	}, s.search)

	p.Tool("web_fetch", "抓取单个 URL 并返回可读文本（HTML 自动转纯文本；二进制请用 web_download）。"+
		"限制（体积 / 超时）来自插件配置，可用 maxBytes / timeoutSec 覆盖。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":        map[string]any{"type": "string", "description": "http/https 绝对地址"},
				"method":     map[string]any{"type": "string", "description": "GET（默认）/POST/PUT/DELETE/HEAD"},
				"headers":    map[string]any{"type": "object", "description": "附加请求头"},
				"body":       map[string]any{"type": "string", "description": "请求体（POST/PUT）"},
				"saveAs":     map[string]any{"type": "string", "description": "落盘路径（绝对或 ~/ 开头；目录则按 URL 文件名）"},
				"maxBytes":   map[string]any{"type": "integer", "description": "本次读取上限（字节，<=32MB）"},
				"timeoutSec": map[string]any{"type": "integer", "description": "本次超时（5-600 秒）"},
			},
			"required": []string{"url"},
		}, s.fetch)
	p.Tool("web_download", "把远端文件下载到本地目录（默认目录/单文件上限/扩展名白名单/是否覆盖均可配置，参数可覆盖）。",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url":        map[string]any{"type": "string", "description": "http/https 绝对地址"},
				"dir":        map[string]any{"type": "string", "description": "目标目录（绝对或 ~/ 开头；缺省用配置）"},
				"filename":   map[string]any{"type": "string", "description": "目标文件名（缺省取 Content-Disposition / URL 文件名）"},
				"maxMB":      map[string]any{"type": "integer", "description": "本次单文件上限（MB）"},
				"overwrite":  map[string]any{"type": "boolean", "description": "同名是否覆盖（缺省用配置；否则自动加 -1）"},
				"headers":    map[string]any{"type": "object", "description": "附加请求头"},
				"timeoutSec": map[string]any{"type": "integer", "description": "本次超时（5-3600 秒）"},
			},
			"required": []string{"url"},
		}, s.download)

	p.OnRequest("test", s.test)
	p.OnRequest("status", s.status)
	p.OnRequest("download-dir", s.downloadDir)
	// 宿主资源通道（PLUGIN-UI-1）：插件自持 webui（embed），宿主经 UDS 逐文件拉取
	// （GET /plugins/websearch/<path> → plugin/v1/ui.asset.get）。
	p.OnRequest("plugin/v1/ui.asset.get", uiAssetGet)
	p.HotTool("web_search")
	p.HotTool("web_fetch")
	p.HotTool("web_download")

	// 配置面（PLUGIN-UI-1 第三段）：`load` / `save` / `reset` 动作 + 一次性键迁移。
	// 配置写入口**只有**动作面（页面不再直调 config.set / 宿主方法）。
	if err := s.cfg.Init(context.Background()); err != nil {
		slog.Warn("websearch: 读取配置失败（按默认值运行）", "err", err)
	}
	for name, fn := range s.cfg.Actions() {
		p.OnRequest(name, fn)
	}

	if err := p.Run(context.Background()); err != nil {
		slog.Error("websearch: 运行结束", "err", err)
	}
}

// ── web_search ─────────────────────────

type result struct {
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Snippet   string  `json:"snippet,omitempty"`
	Summary   string  `json:"summary,omitempty"`
	Content   string  `json:"content,omitempty"`
	SiteName  string  `json:"site_name,omitempty"`
	Date      string  `json:"date,omitempty"`
	Author    string  `json:"author,omitempty"`
	Authority float64 `json:"authority,omitempty"`
	Score     float64 `json:"score,omitempty"`
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
		return nil, errors.New("websearch 未配置 DeepSeek API Key：请在 插件 → 联网能力 中填写")
	}
	count := cfg.Count
	if n := toInt(args["count"]); n > 0 {
		count = n
	}
	if count < 1 {
		count = 1
	}
	if count > 50 {
		count = 50
	}
	needContent := cfg.NeedContent
	if v, ok := args["need_content"].(bool); ok {
		needContent = v
	}
	body := map[string]any{"query": query, "count": count, "need_content": needContent}
	if q := strings.TrimSpace(str(args["ori_question"])); q != "" {
		body["ori_question"] = q
	}
	raw, err := s.do(ctx, apiKey, cfg, body)
	if err != nil {
		s.record(0, err)
		return nil, err
	}
	var resp searchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
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
	s.record(len(items), nil)
	return map[string]any{"provider": "deepseek", "results": items, "count": len(items)}, nil
}

func (s *searcher) do(ctx context.Context, apiKey string, cfg settings, body map[string]any) ([]byte, error) {
	base := strings.TrimRight(cfg.BaseURL, "/")
	path := cfg.SearchPath
	u, err := url.Parse(base + path)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("baseUrl 非法（必须 http/https）")
	}
	payload, _ := json.Marshal(body)
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
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
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, doErr := client.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
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
		lastErr = errors.New("搜索服务不可用")
	}
	return nil, lastErr
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
		return "检索参数被拒绝（400）：" + snippet
	default:
		if code >= 500 {
			return fmt.Sprintf("搜索服务暂不可用（%d），已重试", code)
		}
		return fmt.Sprintf("搜索失败（%d）：%s", code, snippet)
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
	raw, err := s.do(ctx, cfg.APIKey, cfg, map[string]any{
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
		"dir":        dir,
		"maxMB":      cfg.DownloadMaxMB,
		"exts":       cfg.DownloadExts,
		"overwrite":  cfg.DownloadOverwrite,
		"fetchMaxMB": cfg.FetchMaxMB,
	}, nil
}

// ── helpers ─────────────────────────

// 复用层别名：参数取值统一由 pluginsdk 提供（三个 web 工具共用）。
var (
	str   = pluginsdk.Str
	toInt = pluginsdk.Int
)
