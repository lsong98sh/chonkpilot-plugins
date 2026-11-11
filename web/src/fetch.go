// fetch.go —— web_fetch：DeepSeek 网页抓取（`POST /v1/tools/fetch`）。
//
// 返回 `{title, content}`（content = Markdown 正文）。需要原始 HTML / 二进制或落盘下载请用 web_download。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

// deepSeekMaxURL 是 DeepSeek fetch API 的 url 长度上限（与官方文档一致）。
const deepSeekMaxURL = 8192

func (s *searcher) fetch(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := s.cfg.Settings()
	rawURL := strings.TrimSpace(str(args["url"]))
	if rawURL == "" {
		return nil, errors.New("url 不能为空")
	}
	if len(rawURL) > deepSeekMaxURL {
		return nil, fmt.Errorf("url 超过 %d 字节", deepSeekMaxURL)
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("url 必须是含 host 的 http/https 绝对地址")
	}
	if u.User != nil {
		return nil, errors.New("url 不能携带用户名或密码")
	}
	if cfg.APIKey == "" {
		return nil, errors.New("web 未配置 DeepSeek API Key：请在 插件 → 联网能力 中填写")
	}

	// 正文大小上限：参数优先，其次配置 fetch-max-mb（默认 4MiB）；硬上限 8MiB。
	// 先于网络请求校验，避免超限参数白跑一次上游调用。
	maxBytes := int64(cfg.FetchMaxMB) << 20
	if n := toInt(args["maxBytes"]); n > 0 {
		maxBytes = int64(n)
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	if maxBytes > contentHardMaxBytes {
		return nil, fmt.Errorf("maxBytes 超过硬上限 %dMiB（收到 %d 字节）；如需更大内容请用 web_download 落盘", contentHardMaxBytes>>20, maxBytes)
	}

	raw, err := s.doAPI(ctx, cfg, cfg.FetchPath, map[string]any{"url": rawURL})
	if err != nil {
		s.record(0, err)
		return nil, err
	}
	var resp struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		s.record(0, err)
		return nil, fmt.Errorf("抓取响应解析失败: %w（上游可能返回超限或非 JSON；请改用 web_download 取原始内容）", err)
	}

	content := resp.Content
	truncated := false
	if int64(len(content)) > maxBytes {
		content = truncateUTF8(content, int(maxBytes))
		truncated = true
	}

	s.record(1, nil)
	out := map[string]any{
		"provider":  "deepseek",
		"url":       rawURL,
		"title":     resp.Title,
		"content":   content,
		"bytes":     len(content),
		"truncated": truncated,
	}
	if content == "" {
		out["note"] = "empty_content"
		out["hint"] = "未提取到正文：可能是登录墙/反爬/纯 JS 渲染页面；可改用 web_download 获取原始 HTML。"
	}
	return out, nil
}

// truncateUTF8 按字节上限截断，并回退到完整 UTF-8 字符边界（避免半个 rune）。
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s)[:maxBytes]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
