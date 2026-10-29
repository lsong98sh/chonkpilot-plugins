// fetch.go —— web_fetch：单次 HTTP 请求取回文本（或按 saveAs 落盘）。
//
// 与 web_search 同工程、同一配置页；限制项（体积 / 超时）来自配置，LLM 调用参数可覆盖。
// 只做「取回并转成可读文本」，不执行 JS（需要渲染请用 browser_run 插件）。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const fetchHardMaxBytes = 32 << 20

var (
	// Go regexp（RE2）不支持反向引用 `\1`（MustCompile 会 panic）→ 按标签拆成四条。
	reDropBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script[^>]*>.*?</script\s*>`),
		regexp.MustCompile(`(?is)<style[^>]*>.*?</style\s*>`),
		regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript\s*>`),
		regexp.MustCompile(`(?is)<template[^>]*>.*?</template\s*>`),
	}
	reBreakTags = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6])[^>]*>`)
	reTag       = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces    = regexp.MustCompile(`[ \t\f\v]+`)
	reBlankLine = regexp.MustCompile(`\n{3,}`)
	reLinePad   = regexp.MustCompile(` *\n *`)
	htmlEscaper = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">",
		"&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&middot;", "·",
	)
)

func (s *searcher) fetch(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := s.cfg.Settings()
	rawURL := strings.TrimSpace(str(args["url"]))
	if rawURL == "" {
		return nil, errors.New("url 不能为空")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("url 必须是 http/https 绝对地址")
	}

	method := strings.ToUpper(strings.TrimSpace(str(args["method"])))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodPut &&
		method != http.MethodDelete && method != http.MethodHead {
		return nil, fmt.Errorf("不支持的 method: %s", method)
	}

	maxBytes := int64(cfg.FetchMaxMB) << 20
	if n := toInt(args["maxBytes"]); n > 0 {
		maxBytes = int64(n)
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	if maxBytes > fetchHardMaxBytes {
		maxBytes = fetchHardMaxBytes
	}
	timeoutSec := cfg.FetchTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeoutSec = n
	}
	if timeoutSec < 5 {
		timeoutSec = 5
	}
	if timeoutSec > 600 {
		timeoutSec = 600
	}

	var body io.Reader
	if b := str(args["body"]); b != "" {
		body = strings.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "ChonkPilot/1.0")
	if hs, ok := args["headers"].(map[string]any); ok {
		for k, v := range hs {
			if sv, ok := v.(string); ok && k != "" {
				req.Header.Set(k, sv)
			}
		}
	}

	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}

	ct := resp.Header.Get("Content-Type")
	out := map[string]any{
		"url":          u.String(),
		"status":       resp.StatusCode,
		"content_type": ct,
		"bytes":        len(data),
		"truncated":    truncated,
	}
	if saveAs := strings.TrimSpace(str(args["saveAs"])); saveAs != "" {
		path, werr := resolveSavePath(saveAs, u.Path)
		if werr != nil {
			return nil, werr
		}
		if err := writeFileMkdir(path, data); err != nil {
			return nil, fmt.Errorf("写入 %s 失败: %w", path, err)
		}
		out["saved_to"] = path
		return out, nil
	}

	text := string(data)
	if looksHTML(ct, data) {
		text = htmlToText(text)
	} else if !utf8.ValidString(text) {
		return nil, fmt.Errorf("响应不是 UTF-8 文本（content-type=%s，%d 字节）；二进制请用 web_download", ct, len(data))
	}
	out["text"] = text
	return out, nil
}

func looksHTML(ct string, data []byte) bool {
	lc := strings.ToLower(ct)
	if strings.Contains(lc, "text/html") || strings.Contains(lc, "application/xhtml") {
		return true
	}
	head := strings.ToLower(string(data[:min(len(data), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// htmlToText 做保守的标签剥离（不追求完整解析）：去脚本/样式、块级标签转换行、压空白。
func htmlToText(s string) string {
	for _, re := range reDropBlocks {
		s = re.ReplaceAllString(s, " ")
	}
	s = reBreakTags.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = htmlEscaper.Replace(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "\r", "")
	s = reLinePad.ReplaceAllString(s, "\n")
	s = reBlankLine.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// resolveSavePath 解析并校验落盘路径（绝对路径 / ~/ 开头）；目录自动创建。
func resolveSavePath(p, urlPath string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法解析用户目录: %w", err)
		}
		p = filepath.Join(home, p[2:])
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("saveAs 必须是绝对路径或以 ~/ 开头")
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		name := filepath.Base(urlPath)
		if name == "" || name == "/" || name == "." {
			name = "index.html"
		}
		p = filepath.Join(p, name)
	}
	return filepath.Clean(p), nil
}

func writeFileMkdir(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
