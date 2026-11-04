package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动（无 CGO）
)

// dbFileName 是远端服务数据库文件名（与可执行文件同名，仅扩展名不同）。
const dbFileName = "chonkpilot-remote-server.db"

var (
	// ErrNotFound is returned when a record does not exist (or has expired).
	ErrNotFound = errors.New("not found")
	// ErrUserExists is returned when creating a user with a taken username.
	ErrUserExists = errors.New("user already exists")
)

// User is an account record (table users).
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	CreatedAt    int64  `json:"created_at"`
}

// Device is a desktop binding (table devices).
type Device struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Version   string `json:"version"`
	TokenHash string `json:"token_hash"`
	LastSeen  int64  `json:"last_seen"`
	CreatedAt int64  `json:"created_at"`
}

// H5Session is a browser session stored under sha256(cookie token).
type H5Session struct {
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) `<dir>/chonkpilot-remote-server.db` and migrates the schema.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, dbFileName)) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接：远端服务写少读多，串行化可彻底避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 建表（幂等）。
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            TEXT PRIMARY KEY,
			username      TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user',
			disabled      INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS devices (
			id         TEXT PRIMARY KEY,
			user_id    TEXT NOT NULL,
			name       TEXT NOT NULL DEFAULT '',
			platform   TEXT NOT NULL DEFAULT '',
			version    TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL DEFAULT '',
			last_seen  INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_devices_user ON devices(user_id)`,
		`CREATE TABLE IF NOT EXISTS h5_sessions (
			token_hash   TEXT PRIMARY KEY,
			user_id      TEXT NOT NULL,
			device_id    TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '',
			expires_at   INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_h5_sessions_expires ON h5_sessions(expires_at)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- users

// CreateUser creates a new account with a bcrypt password hash.
func (s *Store) CreateUser(username, password, role string) (*User, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return nil, errors.New("username and password are required")
	}
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		return nil, errors.New("role must be admin or user")
	}
	hash, err := bcryptHash(password)
	if err != nil {
		return nil, err
	}
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	u := &User{
		ID:           id,
		Username:     username,
		PasswordHash: hash,
		Role:         role,
		CreatedAt:    time.Now().Unix(),
	}
	_, err = s.db.Exec(
		`INSERT INTO users (id, username, password_hash, role, disabled, created_at) VALUES (?, ?, ?, ?, 0, ?)`,
		u.ID, u.Username, u.PasswordHash, u.Role, u.CreatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrUserExists
		}
		return nil, err
	}
	return u, nil
}

// GetUser returns the account with the given username.
func (s *Store) GetUser(username string) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, role, disabled, created_at FROM users WHERE username = ?`, username)
	return scanUser(row)
}

// GetUserByID returns the account with the given id.
func (s *Store) GetUserByID(id string) (*User, error) {
	row := s.db.QueryRow(
		`SELECT id, username, password_hash, role, disabled, created_at FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// scanUser 读取一行账号（无行 → ErrNotFound）。
func scanUser(row *sql.Row) (*User, error) {
	var u User
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Disabled = disabled != 0
	return &u, nil
}

// ListUsers returns all accounts sorted by username.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(
		`SELECT id, username, password_hash, role, disabled, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// SetPassword replaces the password hash of an existing account.
func (s *Store) SetPassword(username, password string) error {
	if password == "" {
		return errors.New("password is required")
	}
	hash, err := bcryptHash(password)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE username = ?`, hash, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account together with its devices and H5 sessions.
func (s *Store) DeleteUser(username string) error {
	u, err := s.GetUser(username)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM devices WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM h5_sessions WHERE user_id = ?`, u.ID); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, u.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// SetUserDisabled 启用 / 禁用账号（禁用后登录与隧道鉴权都会被拒）。
func (s *Store) SetUserDisabled(username string, disabled bool) error {
	flag := 0
	if disabled {
		flag = 1
	}
	res, err := s.db.Exec(`UPDATE users SET disabled = ? WHERE username = ?`, flag, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CountActiveAdmins 统计**未禁用**的管理员数量（防止把最后一个管理员删掉 / 禁用）。
func (s *Store) CountActiveAdmins() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- devices

// UpsertDevice creates or replaces a device record.
func (s *Store) UpsertDevice(d *Device) error {
	_, err := s.db.Exec(
		`INSERT INTO devices (id, user_id, name, platform, version, token_hash, last_seen, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   user_id = excluded.user_id, name = excluded.name, platform = excluded.platform,
		   version = excluded.version, token_hash = excluded.token_hash,
		   last_seen = excluded.last_seen, created_at = excluded.created_at`,
		d.ID, d.UserID, d.Name, d.Platform, d.Version, d.TokenHash, d.LastSeen, d.CreatedAt,
	)
	return err
}

// GetDevice returns a device by id.
func (s *Store) GetDevice(id string) (*Device, error) {
	row := s.db.QueryRow(
		`SELECT id, user_id, name, platform, version, token_hash, last_seen, created_at FROM devices WHERE id = ?`, id)
	var d Device
	if err := row.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.Version, &d.TokenHash, &d.LastSeen, &d.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// ListDevicesByUser returns all devices owned by userID, most recent first.
func (s *Store) ListDevicesByUser(userID string) ([]Device, error) {
	return s.queryDevices(
		`SELECT id, user_id, name, platform, version, token_hash, last_seen, created_at
		 FROM devices WHERE user_id = ? ORDER BY last_seen DESC, id`, userID)
}

// ListAllDevices 返回全部用户下的设备（后台管理用；按 last_seen 倒序）。
func (s *Store) ListAllDevices() ([]Device, error) {
	return s.queryDevices(
		`SELECT id, user_id, name, platform, version, token_hash, last_seen, created_at
		 FROM devices ORDER BY last_seen DESC, id`)
}

// queryDevices 执行设备查询并扫描（args 可变）。
func (s *Store) queryDevices(q string, args ...any) ([]Device, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.Version, &d.TokenHash, &d.LastSeen, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TouchDevice updates the last_seen timestamp of a device (best effort).
func (s *Store) TouchDevice(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE devices SET last_seen = ? WHERE id = ?`, ts, id)
	return err
}

// ---------------------------------------------------------------- h5 sessions

// PutSession stores (or replaces) an H5 session keyed by sha256 token.
func (s *Store) PutSession(tokenHash string, sess H5Session) error {
	_, err := s.db.Exec(
		`INSERT INTO h5_sessions (token_hash, user_id, device_id, workspace_id, expires_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(token_hash) DO UPDATE SET
		   user_id = excluded.user_id, device_id = excluded.device_id,
		   workspace_id = excluded.workspace_id, expires_at = excluded.expires_at`,
		tokenHash, sess.UserID, sess.DeviceID, sess.WorkspaceID, sess.ExpiresAt,
	)
	return err
}

// GetSession returns a live H5 session; expired records are deleted.
func (s *Store) GetSession(tokenHash string) (*H5Session, error) {
	var sess H5Session
	err := s.db.QueryRow(
		`SELECT user_id, device_id, workspace_id, expires_at FROM h5_sessions WHERE token_hash = ?`, tokenHash,
	).Scan(&sess.UserID, &sess.DeviceID, &sess.WorkspaceID, &sess.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if sess.ExpiresAt <= time.Now().Unix() {
		_ = s.DeleteSession(tokenHash)
		return nil, ErrNotFound
	}
	return &sess, nil
}

// DeleteSession removes an H5 session.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM h5_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

// CleanupExpiredSessions removes all expired H5 sessions.
func (s *Store) CleanupExpiredSessions(now time.Time) error {
	_, err := s.db.Exec(`DELETE FROM h5_sessions WHERE expires_at <= ?`, now.Unix())
	return err
}

// isUniqueViolation 判断 SQLite 唯一约束冲突（用户名重复 → ErrUserExists）。
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}
