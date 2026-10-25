// chonkpilot-codegraph-mcp-server 独立 exe（console；CGO 允许——内嵌官方 tree-sitter go binding）。
// 进程内多语言符号索引工具面，全部工具经官方 go-sdk AddTool 直挂，不经 .tool.md、不 spawn executor。
// 必须显式指定运行形态（无参数打印 usage 退出）：
//   - --http[=<addr>]：前台 Streamable HTTP（端点 /mcp）；addr 缺省 127.0.0.1:5701
//   - --stdio：MCP 客户端（Trae 等）spawn 本 exe，经 stdin/stdout 全双工 JSON-RPC（常驻）
//   - -probe <dir>：自检——索引目录并打印符号汇总后退出（开发/冒烟用，非 MCP 形态）
//   - -dump <dir>：只读自检——从落盘 bbolt 索引库打印命中文件集合（JSON）后退出（非 MCP 形态）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	cg "github.com/lsong98sh/chonkpilot-plugins/codebase/engines/codegraph/server"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultAddr = "127.0.0.1:5701"

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
	fmt.Fprintf(os.Stderr, `chonkpilot-codegraph-mcp-server — ChonkPilot codegraph MCP server（进程内 tree-sitter 索引）

必须指定一种运行形态：
  -http[=<addr>]   前台 Streamable HTTP（默认 %s，端点 /mcp）
  -stdio           stdio transport（MCP 客户端 spawn 本 exe）

其他：
  -probe <dir>     自检：索引目录并打印符号汇总后退出（非 MCP）
  -dump <dir>      只读自检：从落盘 bbolt 索引库打印命中文件集合（JSON）后退出（非 MCP；不重建）
  -stack-gitignore 自检：额外应用 .gitignore / .git/info/exclude / 全局 ignore（默认关）
`, defaultAddr)
}

func main() {
	httpMode := &optionalString{}
	flag.Var(httpMode, "http", "run Streamable HTTP at [addr] (default "+defaultAddr+")")
	stdioMode := flag.Bool("stdio", false, "run as stdio transport (spawned by MCP client)")
	probeDir := flag.String("probe", "", "self-test: index dir and print symbol summary")
	dumpDir := flag.String("dump", "", "self-test: print indexed file list (JSON) from the bbolt store")
	stackGitignore := flag.Bool("stack-gitignore", false, "self-test: also apply .gitignore / .git/info/exclude / global ignore")
	flag.Parse()

	if *dumpDir != "" {
		w, err := cg.Open(*dumpDir)
		if err != nil {
			log.Fatalf("dump open: %v", err)
		}
		loaded, err := w.LoadIndex()
		if err != nil {
			log.Fatalf("dump load: %v", err)
		}
		files := w.IndexedFiles()
		b, err := json.MarshalIndent(map[string]any{
			"workdir": w.Dir, "state": w.State(), "loaded": loaded,
			"count": len(files), "files": files,
		}, "", "  ")
		if err != nil {
			log.Fatalf("dump encode: %v", err)
		}
		fmt.Println(string(b))
		return
	}
	if *probeDir != "" {
		start := time.Now()
		w, err := cg.Open(*probeDir)
		if err != nil {
			log.Fatalf("probe open: %v", err)
		}
		if err := w.Initialize(nil, nil, stackGitignore); err != nil {
			log.Fatalf("probe initialize: %v", err)
		}
		s := w.Status()
		sum := w.ModuleSummary("")
		fmt.Printf("workdir: %s\nfiles: %d\nsymbols: %d\nlangs: %v\nstackGitignore: %v\nelapsedMs: %d\nstore: %s\n",
			w.Dir, s.IndexedFiles, s.IndexedSymbols, sum["langCounts"], s.StackGitignore,
			time.Since(start).Milliseconds(), w.Store)
		for i, sym := range w.SearchSymbol("", "", "", 30) {
			if i >= 30 {
				fmt.Println("...(截断)")
				break
			}
			fmt.Printf("%s:%d [%s] %s cc=%d  %s\n", sym.File, sym.Line, sym.Kind, sym.Name, sym.Complexity, sym.Signature)
		}
		return
	}
	if !httpMode.set && !*stdioMode {
		usage()
		os.Exit(2)
	}

	ms := mcp.NewServer(&mcp.Implementation{Name: "chonkpilot-codegraph-mcp-server", Version: "0.1.0"}, nil)
	if err := cg.RegisterTools(ms); err != nil {
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

// runStdio 以 stdio transport 常驻（直到客户端关闭 stdin）。
func runStdio(ms *mcp.Server) error {
	log.Printf("[codegraph] stdio mode")
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
	log.Printf("[codegraph] listening on %s", addr)
	errCh := make(chan error, 1)
	go func() { errCh <- l.Start(context.Background()) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
		log.Printf("[codegraph] signal received, graceful shutdown")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return l.Stop(ctx)
	case err := <-errCh:
		return err
	}
}
