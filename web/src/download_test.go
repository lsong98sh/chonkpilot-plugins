package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	pluginsdk "github.com/lsong98sh/chonkpilot-plugin-framework/sdk"
)

func newTestSearcher(t *testing.T, values map[string]string) *searcher {
	t.Helper()
	vals := map[string]string{
		KeyTimeoutSec:      "5",
		KeyFetchTimeoutSec: "10",
		// 测试用 httptest 服务监听 127.0.0.1；默认放行内网，SSRF 用例单独关闭。
		KeyAllowPrivate: "true",
	}
	for k, v := range values {
		vals[k] = v
	}
	return &searcher{cfg: newTestManager(t, newMemStore(vals), nil)}
}

// newStrictSearcher 关闭 allow-private（SSRF 防护生效）。
func newStrictSearcher(t *testing.T) *searcher {
	t.Helper()
	return newTestSearcher(t, map[string]string{KeyAllowPrivate: "false"})
}

// web_fetch 走 DeepSeek /v1/tools/fetch：只传 url，解析 {title, content}。
func TestWebFetchDeepSeek(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "示例", "content": "# 正文"})
	}))
	defer srv.Close()

	s := newTestSearcher(t, map[string]string{KeyAPIKey: "sk-test", KeyBaseURL: srv.URL})
	out, err := s.fetch(context.Background(), map[string]any{"url": "https://example.com/a"}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m := out.(map[string]any)
	if m["title"] != "示例" || m["content"] != "# 正文" || m["provider"] != "deepseek" {
		t.Fatalf("out = %#v", m)
	}
	if gotPath != "/v1/tools/fetch" || gotAuth != "Bearer sk-test" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["url"] != "https://example.com/a" {
		t.Fatalf("body=%v", gotBody)
	}
}

// web_fetch 拒绝带账号密码 / 非 http(s) / 无 Key。
func TestWebFetchRejects(t *testing.T) {
	s := newTestSearcher(t, map[string]string{KeyAPIKey: "sk-test"})
	cases := []string{"", "ftp://x/a", "https://u:p@example.com/a", "https://example.com/" + strings.Repeat("a", 9000)}
	for _, u := range cases {
		if _, err := s.fetch(context.Background(), map[string]any{"url": u}, pluginsdk.CallContext{}); err == nil {
			t.Errorf("url=%q 应被拒绝", u)
		}
	}
	noKey := newTestSearcher(t, nil)
	if _, err := noKey.fetch(context.Background(), map[string]any{"url": "https://example.com/a"}, pluginsdk.CallContext{}); err == nil {
		t.Error("未配置 API Key 应报错")
	}
}

// web_download 不带保存路径 → 返回**原始内容**（HTML 不再转纯文本）。
func TestWebDownloadRawContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>raw-html</body></html>"))
	}))
	defer srv.Close()

	s := newTestSearcher(t, nil)
	out, err := s.download(context.Background(), map[string]any{"url": srv.URL}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "<html><body>raw-html</body></html>" {
		t.Fatalf("content = %q（应为原始内容）", m["content"])
	}
	if _, ok := m["text"]; ok {
		t.Fatal("不应再有 text 字段（不再转纯文本）")
	}
}

// 二进制且未给保存路径 → base64（用户口径 2026-10-31）。
func TestWebDownloadBinaryBase64(t *testing.T) {
	raw := []byte{0xff, 0xfe, 0x00, 0x01, 0x80}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()

	s := newTestSearcher(t, nil)
	out, err := s.download(context.Background(), map[string]any{"url": srv.URL}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	m := out.(map[string]any)
	if m["content_base64"] != base64.StdEncoding.EncodeToString(raw) || m["encoding"] != "base64" {
		t.Fatalf("out = %#v", m)
	}
	if _, ok := m["data_uri"]; ok {
		t.Fatalf("不应再返回 data_uri（重复字段，2026-10-31 用户口径）：%#v", m)
	}
}

// 重定向：默认跟随并记录次数；0 = 不跟随；超限 → 明确错误。
func TestWebDownloadRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusFound) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("final-c")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s := newTestSearcher(t, nil)
	out, err := s.download(context.Background(), map[string]any{"url": srv.URL + "/a"}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "final-c" || m["redirects"] != 2 || !strings.HasSuffix(m["final_url"].(string), "/c") {
		t.Fatalf("out = %#v", m)
	}

	// 0 = 不跟随：返回 302，不读跳转后的内容。
	out, err = s.download(context.Background(), map[string]any{"url": srv.URL + "/a", "maxRedirects": 0}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download(no-follow): %v", err)
	}
	if st := out.(map[string]any)["status"]; st != http.StatusFound {
		t.Fatalf("status = %v（0 = 不跟随，应保留 302）", st)
	}

	// 超限：maxRedirects=1，链长 2 → 明确错误。
	if _, err := s.download(context.Background(), map[string]any{"url": srv.URL + "/a", "maxRedirects": 1}, pluginsdk.CallContext{}); err == nil ||
		!strings.Contains(err.Error(), "重定向超过上限") {
		t.Fatalf("err = %v（应提示重定向超限）", err)
	}
}

// 保存模式：落盘内容 = 原始字节。
func TestWebDownloadSaveAs(t *testing.T) {
	body := []byte("save-me-原始")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "out.bin")
	s := newTestSearcher(t, nil)
	out, err := s.download(context.Background(), map[string]any{"url": srv.URL, "saveAs": dst}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读回: %v", rerr)
	}
	if string(got) != string(body) {
		t.Fatalf("文件内容 = %q", got)
	}
	if out.(map[string]any)["saved_to"] != dst {
		t.Fatalf("saved_to = %v", out.(map[string]any)["saved_to"])
	}
}

// 配置键迁移：max-redirects 默认 5，越界回落默认。
func TestSettingsMaxRedirects(t *testing.T) {
	if got := defaultSettings().MaxRedirects; got != 5 {
		t.Fatalf("默认 max-redirects = %d", got)
	}
	if got := settingsOf(map[string]string{KeyMaxRedirects: "20"}).MaxRedirects; got != 20 {
		t.Fatalf("显式 20 → %d", got)
	}
	if got := settingsOf(map[string]string{KeyMaxRedirects: "99"}).MaxRedirects; got != 5 {
		t.Fatalf("越界 99 应回落 5，实际 %d", got)
	}
}

// ── P0：SSRF 目标校验 ─────────────────────────────────────────────

func TestCheckURLNotPrivate(t *testing.T) {
	ctx := context.Background()
	blocked := []string{
		"http://127.0.0.1/",
		"http://localhost/",
		"http://10.0.0.1/",
		"http://172.16.0.1/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://0.0.0.0/",
		"http://foo.local/",
		"http://intranet/",
	}
	for _, raw := range blocked {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if err := checkURLNotPrivate(ctx, u); err == nil {
			t.Errorf("%s 应被拒绝（内网/本机）", raw)
		}
	}
	// 公网 IP 字面量：无需 DNS，直接放行。
	u, _ := url.Parse("https://93.184.216.34/")
	if err := checkURLNotPrivate(ctx, u); err != nil {
		t.Errorf("公网 IP 应放行: %v", err)
	}
}

func TestWebDownloadSSRFBlocksLocalByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("should-not-be-reached"))
	}))
	defer srv.Close()

	if _, err := newStrictSearcher(t).download(context.Background(),
		map[string]any{"url": srv.URL}, pluginsdk.CallContext{}); err == nil || !strings.Contains(err.Error(), "安全限制") {
		t.Fatalf("默认应拒绝环回目标，实际 err = %v", err)
	}
}

func TestWebDownloadAllowPrivateSwitch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	out, err := newTestSearcher(t, nil).download(context.Background(),
		map[string]any{"url": srv.URL}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("allow-private=true 应放行内网: %v", err)
	}
	if out.(map[string]any)["content"] != "ok" {
		t.Fatalf("out = %#v", out)
	}
}

func TestWebDownloadRejectsHostHeaderOverride(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	_, err := newTestSearcher(t, nil).download(context.Background(), map[string]any{
		"url":     srv.URL,
		"headers": map[string]any{"Host": "evil.example.com"},
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "Host") {
		t.Fatalf("headers 覆盖 Host 应被拒绝: %v", err)
	}
}

// ── P1：4xx/5xx 置错误、不落盘 ───────────────────────────────────

func TestWebDownload4xxIsErrorAndNotSaved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>not found</html>"))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "should-not-exist.bin")
	_, err := newTestSearcher(t, nil).download(context.Background(), map[string]any{
		"url": srv.URL, "saveAs": dst,
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("4xx 应返回错误: %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("4xx 不应落盘：%v", statErr)
	}
	// 内容模式同样不把错误页当成功返回。
	if _, err := newTestSearcher(t, nil).download(context.Background(),
		map[string]any{"url": srv.URL}, pluginsdk.CallContext{}); err == nil {
		t.Fatal("4xx 内容模式也应返回错误")
	}
}

// ── P1：maxBytes 硬上限 ──────────────────────────────────────────

func TestWebDownloadMaxBytesHardCap(t *testing.T) {
	_, err := newTestSearcher(t, nil).download(context.Background(), map[string]any{
		"url": "https://example.com/", "maxBytes": contentHardMaxBytes + 1,
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "硬上限") {
		t.Fatalf("超过 maxBytes 硬上限应报错: %v", err)
	}
}

// ── P1：按 Content-Type charset 解码 ─────────────────────────────

func TestWebDownloadGBKDecode(t *testing.T) {
	body, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("中文内容"))
	if err != nil {
		t.Fatalf("GBK 编码: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=GBK")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	out, err := newTestSearcher(t, nil).download(context.Background(),
		map[string]any{"url": srv.URL}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "中文内容" || m["encoding"] != "gbk" {
		t.Fatalf("GBK 应解码为文本: %#v", m)
	}
	if _, ok := m["content_base64"]; ok {
		t.Fatalf("GBK 文本不应走 base64: %#v", m)
	}
}

func TestWebDownloadUTF16BOMDecode(t *testing.T) {
	// "中文" 的 UTF-16LE 字节 + BOM。
	body := []byte{0xFF, 0xFE, 0x2D, 0x4E, 0x87, 0x65}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	out, err := newTestSearcher(t, nil).download(context.Background(),
		map[string]any{"url": srv.URL}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != "中文" || m["encoding"] != "utf-16le" {
		t.Fatalf("UTF-16 BOM 应解码为文本: %#v", m)
	}
}

// ── P1：web_search / web_fetch 8MiB 明确报错 + 正文截断 ──────────

func TestWebSearchOversizeIsExplicitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", apiResponseMaxBytes+1)))
	}))
	defer srv.Close()

	s := newTestSearcher(t, map[string]string{KeyAPIKey: "sk-test", KeyBaseURL: srv.URL})
	_, err := s.search(context.Background(), map[string]any{"query": "x"}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "响应超过上限") {
		t.Fatalf("8MiB 超限应明确报错: %v", err)
	}
}

func TestWebFetchTruncatesContent(t *testing.T) {
	content := strings.Repeat("a", 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"title": "t", "content": content})
	}))
	defer srv.Close()

	s := newTestSearcher(t, map[string]string{KeyAPIKey: "sk-test", KeyBaseURL: srv.URL})
	out, err := s.fetch(context.Background(), map[string]any{
		"url": "https://example.com/a", "maxBytes": 10,
	}, pluginsdk.CallContext{})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	m := out.(map[string]any)
	if m["content"] != strings.Repeat("a", 10) || m["truncated"] != true || m["bytes"] != 10 {
		t.Fatalf("正文应按 maxBytes 截断: %#v", m)
	}
}

func TestWebFetchMaxBytesHardCap(t *testing.T) {
	s := newTestSearcher(t, map[string]string{KeyAPIKey: "sk-test"})
	_, err := s.fetch(context.Background(), map[string]any{
		"url": "https://example.com/a", "maxBytes": contentHardMaxBytes + 1,
	}, pluginsdk.CallContext{})
	if err == nil || !strings.Contains(err.Error(), "硬上限") {
		t.Fatalf("fetch maxBytes 超硬上限应报错: %v", err)
	}
}

func TestSettingsAllowPrivate(t *testing.T) {
	if defaultSettings().AllowPrivate {
		t.Fatal("allow-private 默认必须为 false")
	}
	if settingsOf(nil).AllowPrivate {
		t.Fatal("缺键时 allow-private 必须为 false")
	}
	if !settingsOf(map[string]string{KeyAllowPrivate: "true"}).AllowPrivate {
		t.Fatal("显式 true 应生效")
	}
}
