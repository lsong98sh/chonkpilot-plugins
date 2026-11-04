// Command chonkpilot-remote-server is the central service for ChonkPilot
// remote access: account management, device binding, the desktop tunnel, the
// H5 REST/SSE API and the embedded H5 site.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chonkpilot/chonkpilot-remote-server/web"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(runServe(nil))
	}
	switch args[0] {
	case "serve":
		os.Exit(runServe(args[1:]))
	case "user":
		os.Exit(runUser(args[1:]))
	case "help", "-h", "--help":
		usage()
	default:
		if strings.HasPrefix(args[0], "-") {
			os.Exit(runServe(args))
		}
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ChonkPilot Remote Server

Usage:
  chonkpilot-remote-server [serve] [-addr 127.0.0.1:8080] [-tls-cert file] [-tls-key file]
  chonkpilot-remote-server user add   -u <name> -p <password> [-role admin|user]
  chonkpilot-remote-server user list
  chonkpilot-remote-server user passwd -u <name> -p <password>
  chonkpilot-remote-server user delete -u <name>

Data:
  数据库固定为 <exe 所在目录>\chonkpilot-remote-server.db（SQLite），无 -data 参数。

Environment:
  CK_REMOTE_ADMIN_USER / CK_REMOTE_ADMIN_PASSWORD  bootstrap the first admin
  （未设置时首启自动创建 admin / admin，请登录后立即改密）
`)
}

// exeDataDir 返回可执行文件所在目录（数据库固定落在这里，不再接受 -data 参数）。
func exeDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// ensureBootstrapAdmin 在**没有任何账号**时创建首个管理员：
//   - 环境变量 CK_REMOTE_ADMIN_USER / CK_REMOTE_ADMIN_PASSWORD 优先；
//   - 都未设置（或只给了用户名）→ 默认 admin / admin，并打印告警要求立即改密。
func ensureBootstrapAdmin(st *Store, logger *slog.Logger) {
	count, err := st.CountUsers()
	if err != nil {
		logger.Error("count users failed", "err", err)
		return
	}
	if count > 0 {
		return
	}
	name := strings.TrimSpace(os.Getenv("CK_REMOTE_ADMIN_USER"))
	password := os.Getenv("CK_REMOTE_ADMIN_PASSWORD")
	if name == "" || password == "" {
		name, password = "admin", "admin"
		logger.Warn("no account found, created DEFAULT admin with password 'admin' — change it immediately at /admin")
	}
	if _, err := st.CreateUser(name, password, "admin"); err != nil {
		logger.Error("bootstrap admin failed", "username", name, "err", err)
		return
	}
	logger.Info("bootstrap admin created", "username", name)
}

func runServe(args []string) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", "127.0.0.1:8080", "listen address")
	tlsCert := flags.String("tls-cert", "", "TLS certificate file (optional)")
	tlsKey := flags.String("tls-key", "", "TLS key file (optional)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	logger := newLogger()

	dataDir := exeDataDir()
	st, err := OpenStore(dataDir)
	if err != nil {
		logger.Error("open store failed", "dir", dataDir, "err", err)
		return 1
	}
	defer func() { _ = st.Close() }()
	logger.Info("database opened", "path", filepath.Join(dataDir, dbFileName))

	if err := st.CleanupExpiredSessions(time.Now()); err != nil {
		logger.Warn("cleanup expired sessions failed", "err", err)
	}

	// 首个管理员引导：环境变量优先；都未设置时创建默认 admin / admin（要求登录后立即改密）。
	ensureBootstrapAdmin(st, logger)

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		logger.Error("embedded web assets unavailable", "err", err)
		return 1
	}

	hub := NewHub(logger)
	srv := NewServer(st, hub, dist, logger)

	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if *tlsCert != "" && *tlsKey != "" {
			logger.Info("remote server listening (tls)", "addr", *addr)
			errCh <- httpSrv.ListenAndServeTLS(*tlsCert, *tlsKey)
			return
		}
		logger.Info("remote server listening", "addr", *addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server stopped", "err", err)
			return 1
		}
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}
	return 0
}

func runUser(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	sub, rest := args[0], args[1:]
	flags := flag.NewFlagSet("user "+sub, flag.ContinueOnError)

	openStore := func() (*Store, bool) {
		st, err := OpenStore(exeDataDir())
		if err != nil {
			fmt.Fprintf(os.Stderr, "open store: %v\n", err)
			return nil, false
		}
		return st, true
	}

	switch sub {
	case "add":
		username := flags.String("u", "", "username")
		password := flags.String("p", "", "password")
		role := flags.String("role", "user", "role: admin|user")
		if err := flags.Parse(rest); err != nil {
			return 2
		}
		if *username == "" || *password == "" {
			fmt.Fprintln(os.Stderr, "user add: -u and -p are required")
			return 2
		}
		st, ok := openStore()
		if !ok {
			return 1
		}
		defer func() { _ = st.Close() }()
		user, err := st.CreateUser(*username, *password, *role)
		if err != nil {
			fmt.Fprintf(os.Stderr, "user add: %v\n", err)
			return 1
		}
		fmt.Printf("created user %s (id=%s role=%s)\n", user.Username, user.ID, user.Role)

	case "list":
		if err := flags.Parse(rest); err != nil {
			return 2
		}
		st, ok := openStore()
		if !ok {
			return 1
		}
		defer func() { _ = st.Close() }()
		users, err := st.ListUsers()
		if err != nil {
			fmt.Fprintf(os.Stderr, "user list: %v\n", err)
			return 1
		}
		if len(users) == 0 {
			fmt.Println("no users")
			return 0
		}
		fmt.Printf("%-24s %-8s %-9s %s\n", "USERNAME", "ROLE", "DISABLED", "CREATED")
		for _, u := range users {
			fmt.Printf("%-24s %-8s %-9t %s\n", u.Username, u.Role, u.Disabled, time.Unix(u.CreatedAt, 0).Format(time.RFC3339))
		}

	case "passwd":
		username := flags.String("u", "", "username")
		password := flags.String("p", "", "password")
		if err := flags.Parse(rest); err != nil {
			return 2
		}
		if *username == "" || *password == "" {
			fmt.Fprintln(os.Stderr, "user passwd: -u and -p are required")
			return 2
		}
		st, ok := openStore()
		if !ok {
			return 1
		}
		defer func() { _ = st.Close() }()
		if err := st.SetPassword(*username, *password); err != nil {
			fmt.Fprintf(os.Stderr, "user passwd: %v\n", err)
			return 1
		}
		fmt.Printf("password updated for %s\n", *username)

	case "delete":
		username := flags.String("u", "", "username")
		if err := flags.Parse(rest); err != nil {
			return 2
		}
		if *username == "" {
			fmt.Fprintln(os.Stderr, "user delete: -u is required")
			return 2
		}
		st, ok := openStore()
		if !ok {
			return 1
		}
		defer func() { _ = st.Close() }()
		if err := st.DeleteUser(*username); err != nil {
			fmt.Fprintf(os.Stderr, "user delete: %v\n", err)
			return 1
		}
		fmt.Printf("deleted user %s\n", *username)

	default:
		fmt.Fprintf(os.Stderr, "unknown user subcommand %q\n", sub)
		usage()
		return 2
	}
	return 0
}
