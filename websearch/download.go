// download.go —— web_download：把远端文件流式下载到本地。
//
// 限制项（全部可在配置页设定，LLM 调用参数可覆盖）：
//
//	默认目录 downloadDir · 单文件上限 downloadMaxMB · 扩展名白名单 downloadExts · 是否覆盖 downloadOverwrite。
//
// 安全：只允许 http/https；文件名净化（去路径分隔符 / 控制字符）；超限即删除半成品。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

const downloadHardMaxMB = 2048

func (s *searcher) download(ctx context.Context, args map[string]any, _ pluginsdk.CallContext) (any, error) {
	cfg := s.cfg.Settings()
	rawURL := strings.TrimSpace(str(args["url"]))
	if rawURL == "" {
		return nil, errors.New("url 不能为空")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("url 必须是 http/https 绝对地址")
	}

	maxMB := cfg.DownloadMaxMB
	if n := toInt(args["maxMB"]); n > 0 {
		maxMB = n
	}
	if maxMB <= 0 {
		maxMB = 100
	}
	if maxMB > downloadHardMaxMB {
		maxMB = downloadHardMaxMB
	}
	maxBytes := int64(maxMB) << 20

	dir := strings.TrimSpace(str(args["dir"]))
	if dir == "" {
		dir = strings.TrimSpace(cfg.DownloadDir)
	}
	if dir == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, fmt.Errorf("无法解析用户目录: %w", herr)
		}
		dir = filepath.Join(home, "Downloads", "chonkpilot")
	}
	if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, fmt.Errorf("无法解析用户目录: %w", herr)
		}
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) {
		return nil, errors.New("dir 必须是绝对路径或以 ~/ 开头")
	}

	timeoutSec := cfg.FetchTimeoutSec
	if n := toInt(args["timeoutSec"]); n > 0 {
		timeoutSec = n
	}
	if timeoutSec < 5 {
		timeoutSec = 5
	}
	if timeoutSec > 3600 {
		timeoutSec = 3600
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
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
		return nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("下载失败（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	name := strings.TrimSpace(str(args["filename"]))
	if name == "" {
		name = filenameFromDisposition(resp.Header.Get("Content-Disposition"))
	}
	if name == "" {
		name = pathBase(u.Path)
	}
	name = sanitizeFilename(name)
	if name == "" {
		name = "download.bin"
	}
	if exts := strings.TrimSpace(cfg.DownloadExts); exts != "" {
		if !extAllowed(name, exts) {
			return nil, fmt.Errorf("扩展名 %q 不在白名单（%s）", strings.ToLower(filepath.Ext(name)), exts)
		}
	}

	overwrite := cfg.DownloadOverwrite
	if v, ok := args["overwrite"].(bool); ok {
		overwrite = v
	}
	path := filepath.Join(dir, name)
	if !overwrite {
		path = uniquePath(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}

	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}
	written, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("写入失败: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("落盘失败: %w", closeErr)
	}
	if written > maxBytes {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("超过单文件上限 %dMB（可在配置页或 maxMB 参数调整）", maxMB)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("重命名失败: %w", err)
	}

	return map[string]any{
		"path":         path,
		"bytes":        written,
		"content_type": resp.Header.Get("Content-Type"),
		"status":       resp.StatusCode,
		"url":          u.String(),
	}, nil
}

func filenameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(cd); err == nil {
		if v := strings.TrimSpace(params["filename"]); v != "" {
			return v
		}
	}
	return ""
}

func pathBase(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// sanitizeFilename 去掉路径分隔符与控制字符，拒绝 .. 与 Windows 保留名。
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "LPT1", "LPT2", "LPT3":
		name = "_" + name
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		name = name[:180-len(ext)] + ext
	}
	return name
}

func extAllowed(name, list string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, item := range strings.Split(list, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if !strings.HasPrefix(item, ".") {
			item = "." + item
		}
		if ext == item {
			return true
		}
	}
	return false
}

func uniquePath(path string) string {
	if _, err := os.Stat(path); err != nil {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", base, i, ext)
		if _, err := os.Stat(candidate); err != nil {
			return candidate
		}
	}
	return fmt.Sprintf("%s-%d%s", base, time.Now().UnixMilli(), ext)
}
