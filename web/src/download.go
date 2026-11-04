// download.go —— web_download：本地直连抓取（**不走上游 API**）。
//
// 两种用法：
//   - 不传 saveAs / dir / filename → **返回原始内容**：文本原样返回 `content`；二进制返回 `content_base64`；
//   - 传 saveAs / dir / filename → **落盘下载**（流式写 .part，超限即删半成品）。
//
// 重定向：默认跟随插件配置 `max-redirects` 次（默认 5，边界 0-20；0 = 不跟随），
// 调用参数 maxRedirects 可覆盖。跟随过程中的请求方法/请求头保持一致，响应体只在最终地址读取
// （避免中间跳转把内容读断），落盘/返回二选一，不会出现「链断在半路」。
//
// 限制项（全部可在配置页设定，LLM 调用参数可覆盖）：
//
//	默认目录 downloadDir · 单文件上限 downloadMaxMB · 扩展名白名单 downloadExts · 是否覆盖 downloadOverwrite；
//	返回内容上限 fetchMaxMB · 超时 fetchTimeoutSec。
//
// 安全：只允许 http/https；文件名净化（去路径分隔符 / 控制字符）；超限即删除半成品。
package main

import (
	"context"
	"encoding/base64"
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
	"unicode/utf8"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
	"github.com/lsong98sh/chonkpilot-util/namex"
)

const (
	downloadHardMaxMB = 2048
	maxRedirectsMin   = 0
	maxRedirectsMax   = 20
)

func (s *searcher) download(ctx context.Context, args map[string]any, cx pluginsdk.CallContext) (any, error) {
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

	saveAs := strings.TrimSpace(str(args["saveAs"]))
	dirArg := strings.TrimSpace(str(args["dir"]))
	nameArg := strings.TrimSpace(str(args["filename"]))
	saveMode := saveAs != "" || dirArg != "" || nameArg != ""

	// 返回内容上限（非下载模式）：maxBytes 优先，其次配置 fetchMaxMB。
	maxBytes := int64(cfg.FetchMaxMB) << 20
	if n := toInt(args["maxBytes"]); n > 0 {
		maxBytes = int64(n)
	}
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}
	// 下载模式上限：maxMB 优先，其次配置 downloadMaxMB。
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

	maxRedirects := cfg.MaxRedirects
	if _, has := args["maxRedirects"]; has {
		maxRedirects = toInt(args["maxRedirects"])
	}
	if maxRedirects < maxRedirectsMin {
		maxRedirects = maxRedirectsMin
	}
	if maxRedirects > maxRedirectsMax {
		maxRedirects = maxRedirectsMax
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

	redirects := 0
	client := &http.Client{
		Timeout: time.Duration(timeoutSec) * time.Second,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if maxRedirects == 0 {
				return http.ErrUseLastResponse // 不跟随：原样返回 3xx
			}
			if len(via) > maxRedirects {
				return fmt.Errorf("重定向超过上限 %d 次", maxRedirects)
			}
			redirects = len(via)
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	finalURL := u.String()
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}

	out := func() map[string]any {
		return map[string]any{
			"url":          u.String(),
			"final_url":    finalURL,
			"status":       resp.StatusCode,
			"content_type": resp.Header.Get("Content-Type"),
			"redirects":    redirects,
		}
	}

	if saveMode {
		path, name := "", ""
		if saveAs != "" {
			p, werr := resolveSavePath(saveAs, u.Path)
			if werr != nil {
				return nil, werr
			}
			path = p
			name = filepath.Base(p)
		} else {
			dir := dirArg
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
			d, derr := resolveDir(dir)
			if derr != nil {
				return nil, derr
			}
			name = nameArg
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
			path = filepath.Join(d, name)
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
		if !overwrite {
			path = uniquePath(path)
		}
		// C6（2026-11-03）：按**最终落盘路径**校验可写白名单（saveAs / dir+filename / 默认目录
		// 一律判定；未启用沙箱 = 不校验）。默认目录不在白名单时明确失败，不自动放行。
		if err := checkSandboxWritable(cx, path); err != nil {
			return nil, err
		}
		written, werr := writeStream(path, resp.Body, int64(maxMB)<<20)
		if werr != nil {
			return nil, werr
		}
		res := out()
		res["path"] = path
		res["bytes"] = written
		res["saved_to"] = path
		return res, nil
	}

	// 非下载模式：返回原始内容（不做 HTML 转纯文本）。
	limit := maxBytes
	if limit > 0 {
		limit++
	}
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, limit))
	if rerr != nil {
		return nil, fmt.Errorf("读取响应失败: %w", rerr)
	}
	truncated := int64(len(data)) > maxBytes
	if truncated {
		data = data[:maxBytes]
	}
	res := out()
	res["bytes"] = len(data)
	res["truncated"] = truncated
	if utf8.Valid(data) {
		res["content"] = string(data)
	} else {
		// 二进制（未指定保存路径）→ base64（用户口径 2026-10-31）。
		res["content_base64"] = base64.StdEncoding.EncodeToString(data)
		res["encoding"] = "base64"
	}
	return res, nil
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
	return namex.Sanitize(name)
}

func extAllowed(name, list string) bool {
	return namex.ExtAllowed(name, strings.Split(list, ","))
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

// writeStream 把响应体流式写入 path（先写 .part，超限/失败即清理）。
func writeStream(path string, r io.Reader, maxBytes int64) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("创建目录失败: %w", err)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, fmt.Errorf("创建文件失败: %w", err)
	}
	written, copyErr := io.Copy(f, io.LimitReader(r, maxBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("写入失败: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("落盘失败: %w", closeErr)
	}
	if written > maxBytes {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("超过单文件上限 %dMB（可在配置页或 maxMB 参数调整）", maxBytes>>20)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("重命名失败: %w", err)
	}
	return written, nil
}

// resolveDir 解析并校验下载目录（绝对路径 / ~/ 开头）。
func resolveDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("无法解析用户目录: %w", err)
		}
		dir = filepath.Join(home, dir[2:])
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New("dir 必须是绝对路径或以 ~/ 开头")
	}
	return filepath.Clean(dir), nil
}

// resolveSavePath 解析并校验落盘路径（绝对路径 / ~/ 开头）；目录自动按 URL 文件名拼接。
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
