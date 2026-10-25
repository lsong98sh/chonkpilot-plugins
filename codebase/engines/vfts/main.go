// chonkpilot-vfts-mcp-server 独立 exe（console；CGO 允许——链接 zvec C-API）。
// 基于 zvec FTS 的全文索引工具面，全部工具经官方 go-sdk AddTool 直挂，不经 .tool.md、不 spawn executor。
// 必须显式指定运行形态（无参数打印 usage 退出）：
//   - --http[=<addr>]：前台 Streamable HTTP（端点 /mcp）；addr 缺省 127.0.0.1:5702
//   - --stdio：MCP 客户端（plugin/Trae 等）spawn 本 exe，经 stdin/stdout 全双工 JSON-RPC（常驻）
//   - -probe <dir>：自检——对该目录建索引并打印汇总后退出（开发/冒烟用，非 MCP 形态）；
//     可附 -match / -query / -topk 在自检时执行一次检索并打印命中。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	vf "github.com/lsong98sh/chonkpilot-plugins/codebase/engines/vfts/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultAddr = "127.0.0.1:5702"

// optionalString 支持 `--http`（无值）与 `--http=addr`（有值）。
type optionalString struct {
	set bool
	val string
}

func (o *optionalString) String() string { return o.val }
func (o *optionalString) Set(s string) error {
	o.set = true
	if s == "true" {
		o.val = ""
	} else {
		o.val = s
	}
	return nil
}

func (o *optionalString) IsBoolFlag() bool { return true }

func usage() {
	fmt.Fprintf(os.Stderr, `chonkpilot-vfts-mcp-server — ChonkPilot vfts MCP server（zvec FTS 全文索引）

必须指定一种运行形态：
  -http[=<addr>]   前台 Streamable HTTP（默认 %s，端点 /mcp）
  -stdio           stdio transport（MCP 客户端 spawn 本 exe）

其他：
  -probe <dir>     自检：对目录建索引并打印汇总后退出（非 MCP）
  -stack-gitignore 自检：额外应用 .gitignore / .git/info/exclude / 全局 ignore（默认关）
  -match <str>     自检时附带执行自然语言匹配检索
  -query <expr>    自检时附带执行布尔/高级表达式检索
  -topk <n>        自检检索返回上限（默认 20）

运行时需保证 zvec_c_api.dll 可被找到（与本 exe 同目录，或在 PATH 中）。
`, defaultAddr)
}

func main() {
	httpMode := &optionalString{}
	flag.Var(httpMode, "http", "run Streamable HTTP at [addr] (default "+defaultAddr+")")
	stdioMode := flag.Bool("stdio", false, "run as stdio transport (spawned by MCP client)")
	probeDir := flag.String("probe", "", "self-test: index dir and print summary")
	stackGitignore := flag.Bool("stack-gitignore", false, "self-test: also apply .gitignore / .git/info/exclude / global ignore")
	match := flag.String("match", "", "self-test: run a natural-language match query")
	query := flag.String("query", "", "self-test: run a boolean/advanced expression query")
	topK := flag.Int("topk", 20, "self-test: query result limit")
	flag.Parse()

	defer vf.CloseAll()

	if *probeDir != "" {
		runProbe(*probeDir, *stackGitignore, *match, *query, *topK)
		return
	}
	if !httpMode.set && !*stdioMode {
		usage()
		os.Exit(2)
	}

	ms := mcp.NewServer(&mcp.Implementation{Name: "chonkpilot-vfts-mcp-server", Version: "0.1.0"}, nil)
	if err := vf.RegisterTools(ms); err != nil {
		log.Fatalf("register tools: %v", err)
	}

	if *stdioMode {
		if err := runStdio(ms); err != nil {
			log.Fatalf("stdio server: %v", err)
		}
		return
	}

	addr := httpMode.val
	if addr == "" {
		addr = defaultAddr
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return ms }, nil)
	httpSrv := &http.Server{Addr: addr, Handler: handler}
	life := &httpLifecycle{srv: httpSrv}
	if err := runConsole(life, addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// runProbe 自检：建索引 + 可选检索，打印真实结果（非 MCP 形态）。
func runProbe(dir string, stackGitignore bool, match, query string, topK int) {
	start := time.Now()
	w, err := vf.Open(dir)
	if err != nil {
		log.Fatalf("probe open: %v", err)
	}
	if _, err := w.Initialize(nil, nil, &stackGitignore); err != nil {
		log.Fatalf("probe initialize: %v", err)
	}
	s := w.Status()
	fmt.Printf("workdir: %s\nstore: %s\nfiles: %d\nchunks: %d\nstate: %s\ntokenizer: %s\nstackGitignore: %v\nelapsedMs: %d\n",
		w.Dir, w.Store, s.IndexedFiles, s.ChunkCount, s.State, s.Tokenizer, s.StackGitignore, time.Since(start).Milliseconds())
	if match == "" && query == "" {
		fmt.Println("tools:", vf.ToolNames())
		return
	}
	hits, err := w.Query(match, query, topK, "")
	if err != nil {
		log.Fatalf("probe query: %v", err)
	}
	fmt.Printf("hits: %d\n", len(hits))
	for _, h := range hits {
		fmt.Printf("  %.4f %s:%d  %s\n", h.Score, h.Path, h.Line, h.Snippet)
	}
}

// runStdio 以 stdio transport 常驻（直到客户端关闭 stdin）。
func runStdio(ms *mcp.Server) error {
	log.Printf("[vfts] stdio mode")
	return ms.Run(context.Background(), &mcp.StdioTransport{})
}

// httpLifecycle Streamable HTTP 启停。
type httpLifecycle struct {
	srv *http.Server
}

func (l *httpLifecycle) Start(context.Context) error {
	return l.srv.ListenAndServe()
}

func (l *httpLifecycle) Stop(ctx context.Context) error {
	return l.srv.Shutdown(ctx)
}

// runConsole 前台运行：阻塞监听，Ctrl+C/SIGTERM 优雅关闭。
func runConsole(l *httpLifecycle, addr string) error {
	log.Printf("[vfts] listening on %s", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- l.Start(context.Background()) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		log.Printf("[vfts] signal received, graceful shutdown")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return l.Stop(ctx)
	case err := <-errCh:
		return err
	}
}
