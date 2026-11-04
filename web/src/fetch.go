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
		return nil, fmt.Errorf("url 超过 %d 字符", deepSeekMaxURL)
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
		return nil, fmt.Errorf("抓取响应解析失败: %w", err)
	}
	s.record(1, nil)
	return map[string]any{
		"provider": "deepseek",
		"url":      rawURL,
		"title":    resp.Title,
		"content":  resp.Content,
	}, nil
}
