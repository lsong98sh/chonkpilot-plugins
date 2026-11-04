package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// 数据库固定落在 <dir>/chonkpilot-remote-server.db（与 exe 同名，SQLite）。
func TestStoreSQLiteFileAndDefaultAdmin(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	if _, err := os.Stat(filepath.Join(dir, dbFileName)); err != nil {
		t.Fatalf("数据库文件未生成 %s: %v", dbFileName, err)
	}

	// 首启无账号 → 自动创建默认 admin / admin。
	t.Setenv("CK_REMOTE_ADMIN_USER", "")
	t.Setenv("CK_REMOTE_ADMIN_PASSWORD", "")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ensureBootstrapAdmin(st, logger)

	u, err := st.GetUser("admin")
	if err != nil {
		t.Fatalf("默认管理员未创建: %v", err)
	}
	if u.Role != "admin" || u.Disabled {
		t.Fatalf("默认管理员属性不符: %+v", u)
	}
	if !bcryptCheck(u.PasswordHash, "admin") {
		t.Fatalf("默认密码应为 admin")
	}

	// 幂等：已有账号时不再创建。
	ensureBootstrapAdmin(st, logger)
	if n, err := st.CountUsers(); err != nil || n != 1 {
		t.Fatalf("CountUsers = %d err=%v, want 1", n, err)
	}
}

// 环境变量优先于默认值。
func TestBootstrapAdminFromEnv(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer func() { _ = st.Close() }()

	t.Setenv("CK_REMOTE_ADMIN_USER", "root")
	t.Setenv("CK_REMOTE_ADMIN_PASSWORD", "s3cret")
	ensureBootstrapAdmin(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	u, err := st.GetUser("root")
	if err != nil || !bcryptCheck(u.PasswordHash, "s3cret") {
		t.Fatalf("环境变量引导失败: user=%+v err=%v", u, err)
	}
	if _, err := st.GetUser("admin"); err == nil {
		t.Fatalf("不应同时创建默认 admin")
	}
}
