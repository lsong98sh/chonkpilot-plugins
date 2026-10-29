// engine_webview.go — browser 插件的 WebView2 引擎实现。
//
// 约束（来自 fork 实现）：
//   - WebView2 必须绑定真实 HWND（NewWithOptions 内部 CreateWindowExW + Embed）；
//   - 无 headless：headless=true 时用 Hidden 隐藏窗口（HWND 仍存在）；
//   - EvalWithResult / Screenshot 不能在本线程就是 UI 线程时调用（内部会 Dispatch 后等待）。
//
// 因此：在独立 OS 线程跑 NewWithOptions + Run（阻塞），其余 goroutine 只通过
// EvalWithResult / Screenshot / Terminate 与之交互。
package browserplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	webview2 "github.com/jchv/go-webview2"

	"github.com/lsong98sh/chonkpilot-plugins/shared/pluginhost"
)

// evalTimeout 是单次 JS 求值的默认超时。
const evalTimeout = 15 * time.Second

// webviewEngine 是一个 WebView2 会话。
type webviewEngine struct {
	wv      webview2.WebView
	ready   chan struct{}
	done    chan struct{}
	opts    pluginhost.ContextOptions
	dataDir string
}

// NewWebViewEngine 创建并启动一个 WebView2 会话（实现 pluginhost.Options.NewEngine）。
func NewWebViewEngine(opts pluginhost.ContextOptions) (pluginhost.Engine, error) {
	w, h := opts.Width, opts.Height
	if w <= 0 {
		w = 1280
	}
	if h <= 0 {
		h = 800
	}
	dataDir := profileDir(opts.Profile)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create profile dir: %w", err)
	}

	e := &webviewEngine{
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
		opts:    opts,
		dataDir: dataDir,
	}
	initErr := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer close(e.done)
		wv := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:            opts.Debug,
			DevToolsDisabled: !opts.Debug,
			DataPath:         dataDir,
			AutoFocus:        true,
			Hidden:           opts.Headless,
			WindowOptions: webview2.WindowOptions{
				Title:  "Chonkpilot Browser Context",
				Width:  uint(w),
				Height: uint(h),
				Center: true,
			},
		})
		if wv == nil {
			initErr <- fmt.Errorf("webview2 create failed")
			return
		}
		e.wv = wv
		wv.Init(consoleHookJS)
		close(e.ready)
		wv.Run() // 阻塞直到 Terminate
	}()

	select {
	case err := <-initErr:
		return nil, err
	case <-e.ready:
	case <-time.After(20 * time.Second):
		return nil, fmt.Errorf("webview2 init timeout")
	}

	// 初始页
	url := strings.TrimSpace(opts.URL)
	if url == "" {
		url = "about:blank"
	}
	e.wv.Navigate(url)
	e.waitReady(10 * time.Second)
	return e, nil
}

// profileDir 返回某 profile 的 WebView2 用户数据目录。
func profileDir(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	// 只允许安全字符，避免路径穿越
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		b.WriteString("default")
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "chonkpilot", "plugins", "profiles", b.String())
}

// Do 执行一个原子动作。
func (e *webviewEngine) Do(action string, args map[string]any) (map[string]any, error) {
	a := pluginhost.NormalizeAction(action)
	switch a {
	case "navigate", "goto", "open":
		url := argString(args, "url")
		if url == "" {
			return nil, fmt.Errorf("url is required")
		}
		if !isAllowedURL(url) {
			return nil, fmt.Errorf("refused url scheme (only http/https/file/about)")
		}
		e.wv.Navigate(url)
		ok := e.waitReady(15 * time.Second)
		return map[string]any{"url": url, "ready": ok}, nil

	case "set_html":
		html := argString(args, "html")
		if html == "" {
			return nil, fmt.Errorf("html is required")
		}
		e.wv.SetHtml(html)
		return map[string]any{"ready": e.waitReady(10 * time.Second)}, nil

	case "eval", "js":
		script := argString(args, "script")
		if script == "" {
			return nil, fmt.Errorf("script is required")
		}
		out, err := e.evalResult(script, evalTimeout)
		if err != nil {
			return nil, err
		}
		return map[string]any{"value": out}, nil

	case "title":
		return map[string]any{"title": e.evalString("document.title", 5*time.Second)}, nil

	case "url":
		return map[string]any{"url": e.evalString("location.href", 5*time.Second)}, nil

	case "html":
		return map[string]any{"html": e.evalString("document.documentElement.outerHTML", 10*time.Second)}, nil

	case "text":
		sel := argString(args, "selector")
		if sel == "" {
			return nil, fmt.Errorf("selector is required")
		}
		out := e.evalString(fmt.Sprintf(
			`(function(){var el=document.querySelector(%s);return el?el.innerText:null;})()`, jsString(sel)), 10*time.Second)
		if out == "" {
			return nil, fmt.Errorf("element not found: %s", sel)
		}
		return map[string]any{"text": out}, nil

	case "click":
		sel := argString(args, "selector")
		if sel == "" {
			return nil, fmt.Errorf("selector is required")
		}
		out := e.evalString(fmt.Sprintf(
			`(function(){var el=document.querySelector(%s);if(!el)return "missing";el.scrollIntoView({block:"center"});el.click();return "ok";})()`, jsString(sel)), 10*time.Second)
		if out != "ok" {
			return nil, fmt.Errorf("click failed: element not found (%s)", sel)
		}
		return map[string]any{"clicked": sel}, nil

	case "fill", "type":
		sel := argString(args, "selector")
		text := argString(args, "text")
		if sel == "" {
			return nil, fmt.Errorf("selector is required")
		}
		out := e.evalString(fmt.Sprintf(
			`(function(){var el=document.querySelector(%s);if(!el)return "missing";el.focus();el.value=%s;el.dispatchEvent(new Event("input",{bubbles:true}));el.dispatchEvent(new Event("change",{bubbles:true}));return "ok";})()`,
			jsString(sel), jsString(text)), 10*time.Second)
		if out != "ok" {
			return nil, fmt.Errorf("fill failed: element not found (%s)", sel)
		}
		return map[string]any{"filled": sel}, nil

	case "wait":
		sel := argString(args, "selector")
		if sel == "" {
			return nil, fmt.Errorf("selector is required")
		}
		timeout := time.Duration(argInt(args, "timeoutMs", 10000)) * time.Millisecond
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			exists := e.evalString(fmt.Sprintf(
				`(function(){return document.querySelector(%s)?"1":"0";})()`, jsString(sel)), 5*time.Second)
			if exists == "1" {
				return map[string]any{"found": true, "selector": sel}, nil
			}
			time.Sleep(200 * time.Millisecond)
		}
		return nil, fmt.Errorf("wait timeout: %s not found within %s", sel, timeout)

	case "screenshot", "shot":
		path := argString(args, "path")
		if path == "" {
			return nil, fmt.Errorf("path is required")
		}
		sc, ok := e.wv.(webview2.Screenshot)
		if !ok {
			return nil, fmt.Errorf("webview2 screenshot not available")
		}
		png, err := sc.Screenshot(20 * time.Second)
		if err != nil {
			return nil, fmt.Errorf("screenshot: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create dir: %w", err)
		}
		if err := os.WriteFile(path, png, 0o644); err != nil {
			return nil, fmt.Errorf("write screenshot: %w", err)
		}
		return map[string]any{"path": path, "bytes": len(png), "format": "png"}, nil

	case "console_dump":
		out := e.evalString(`(function(){var a=window.__ckConsole||[];window.__ckConsole=[];return JSON.stringify(a);})()`, 10*time.Second)
		return map[string]any{"entries": out, "count": strings.Count(out, `"level"`)}, nil

	case "reload":
		e.wv.Eval("location.reload()")
		return map[string]any{"ready": e.waitReady(15 * time.Second)}, nil

	case "back":
		e.wv.Eval("history.back()")
		return map[string]any{"ready": e.waitReady(10 * time.Second)}, nil

	case "forward":
		e.wv.Eval("history.forward()")
		return map[string]any{"ready": e.waitReady(10 * time.Second)}, nil

	case "debug_open":
		if d, ok := e.wv.(webview2.DevTools); ok {
			d.OpenDevToolsWindow()
			return map[string]any{"opened": true}, nil
		}
		return nil, fmt.Errorf("devtools not available (start with debug=true)")

	default:
		return nil, fmt.Errorf("unsupported action: %s", action)
	}
}

// Close 终止会话。
func (e *webviewEngine) Close() error {
	if e.wv != nil {
		e.wv.Terminate()
	}
	select {
	case <-e.done:
	case <-time.After(5 * time.Second):
		if e.wv != nil {
			e.wv.Destroy()
		}
	}
	return nil
}

// evalResult 调 EvalWithResult（带类型断言）。
func (e *webviewEngine) evalResult(script string, timeout time.Duration) (string, error) {
	se, ok := e.wv.(webview2.ScriptEval)
	if !ok {
		return "", fmt.Errorf("script eval not available")
	}
	return se.EvalWithResult(script, timeout)
}

// evalString 求值并去掉 JSON 字符串引号（失败返回空串）。
func (e *webviewEngine) evalString(script string, timeout time.Duration) string {
	out, err := e.evalResult(script, timeout)
	if err != nil {
		return ""
	}
	return unquoteJSON(out)
}

// waitReady 轮询 document.readyState == complete。
func (e *webviewEngine) waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.evalString("document.readyState", 3*time.Second) == "complete" {
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// unquoteJSON 去掉 EvalWithResult 可能带的 JSON 引号。
func unquoteJSON(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		var out strings.Builder
		for i := 1; i < len(s)-1; i++ {
			if s[i] == '\\' && i+1 < len(s)-1 {
				i++
				switch s[i] {
				case 'n':
					out.WriteByte('\n')
				case 't':
					out.WriteByte('\t')
				case 'r':
					out.WriteByte('\r')
				case '"':
					out.WriteByte('"')
				case '\\':
					out.WriteByte('\\')
				default:
					out.WriteByte('\\')
					out.WriteByte(s[i])
				}
				continue
			}
			out.WriteByte(s[i])
		}
		return out.String()
	}
	if s == "null" || s == "undefined" {
		return ""
	}
	return s
}

// jsString 把 Go 字符串安全地变成 JS 字符串字面量。
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// argString 取字符串参数。
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	v, _ := args[key].(string)
	return v
}

// argInt 取整数参数。
func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return def
}

// isAllowedURL 只放行 http/https/file/about（拒绝 javascript: / data: 等）。
func isAllowedURL(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	for _, p := range []string{"http://", "https://", "file://", "about:"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// consoleHookJS 是注入到每个新文档的 console 捕获脚本（C 方案：Eval 劫持）。
// A 方案（DevTools Protocol Runtime.consoleAPICalled）待 fork 补齐封装后替换，
// 接口保持 console_dump 不变。
const consoleHookJS = `(function(){
  if (window.__ckConsole) return;
  window.__ckConsole = [];
  window.__ckConsoleMax = 2000;
  ['log','warn','error','info','debug'].forEach(function(lv){
    var orig = console[lv];
    console[lv] = function(){
      try{
        var parts = [];
        for (var i=0;i<arguments.length;i++){
          var a = arguments[i], s;
          try { s = (typeof a === 'string') ? a : JSON.stringify(a); } catch(e) { s = String(a); }
          parts.push(s);
        }
        window.__ckConsole.push({level:lv, text:parts.join(' '), ts:Date.now()});
        if (window.__ckConsole.length > window.__ckConsoleMax) window.__ckConsole.shift();
      }catch(e){}
      return orig.apply(console, arguments);
    };
  });
})();`
