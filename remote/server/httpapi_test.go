package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testServer(t *testing.T) (*Store, *Hub, *httptest.Server) {
	t.Helper()
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(logger)
	webFS := fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte("<!doctype html><html><body>remote-h5</body></html>")},
		"assets/app-abc123.js": &fstest.MapFile{Data: []byte("console.log('h5')")},
	}
	srv := NewServer(st, hub, webFS, logger)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, hub, ts
}

func newTestClient(t *testing.T, ts *httptest.Server, username, password string) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}
	resp := doRequest(t, client, http.MethodPost, ts.URL+"/api/login", map[string]string{
		"username": username,
		"password": password,
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login %s failed: status=%d body=%s", username, resp.StatusCode, body)
	}
	return client
}

func doRequest(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, want, body)
	}
	_ = resp.Body.Close()
}

// expectJSON asserts the status code and decodes the body into v.
func expectJSON(t *testing.T, resp *http.Response, want int, v any) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status = %d, want %d, body=%s", resp.StatusCode, want, body)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func TestLoginMeLogout(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	anon := &http.Client{}
	resp := doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "wrong"})
	expectStatus(t, resp, http.StatusUnauthorized)

	resp = doRequest(t, anon, http.MethodGet, ts.URL+"/api/me", nil)
	var apiErrBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	expectJSON(t, resp, http.StatusUnauthorized, &apiErrBody)
	if apiErrBody.Error.Code != "unauthorized" || apiErrBody.Error.Message == "" {
		t.Fatalf("unexpected error body: %+v", apiErrBody)
	}

	client := newTestClient(t, ts, "alice", "secret")
	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/me", nil)
	var me struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
		Context *struct {
			DeviceID    string `json:"device_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"context"`
	}
	expectJSON(t, resp, http.StatusOK, &me)
	if me.User.Username != "alice" || me.User.Role != "user" || me.User.ID == "" {
		t.Fatalf("unexpected /api/me: %+v", me)
	}
	if me.Context != nil {
		t.Fatalf("fresh session should not have context: %+v", me.Context)
	}

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/logout", nil)
	expectStatus(t, resp, http.StatusOK)

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/me", nil)
	expectStatus(t, resp, http.StatusUnauthorized)
}

func TestLoginRateLimit(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	anon := &http.Client{}
	for i := 0; i < 5; i++ {
		resp := doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "wrong"})
		expectStatus(t, resp, http.StatusUnauthorized)
	}
	resp := doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "alice", "password": "wrong"})
	expectStatus(t, resp, http.StatusTooManyRequests)

	// A different username is unaffected by alice's failures.
	if _, err := st.CreateUser("bob", "secret", "user"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	resp = doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "bob", "password": "wrong"})
	expectStatus(t, resp, http.StatusUnauthorized)
}

func TestContextOwnership(t *testing.T) {
	st, _, ts := testServer(t)
	alice, err := st.CreateUser("alice", "secret", "user")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := st.CreateUser("bob", "secret", "user")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if err := st.UpsertDevice(&Device{ID: "dev-bob", UserID: bob.ID, Name: "Bob PC"}); err != nil {
		t.Fatalf("upsert bob device: %v", err)
	}
	if err := st.UpsertDevice(&Device{ID: "dev-alice", UserID: alice.ID, Name: "Alice PC"}); err != nil {
		t.Fatalf("upsert alice device: %v", err)
	}

	client := newTestClient(t, ts, "alice", "secret")

	resp := doRequest(t, client, http.MethodPost, ts.URL+"/api/context", map[string]string{"device_id": "dev-bob", "workspace_id": "ws-1"})
	expectStatus(t, resp, http.StatusForbidden)

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/context", map[string]string{"device_id": "nope", "workspace_id": "ws-1"})
	expectStatus(t, resp, http.StatusNotFound)

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/context", map[string]string{"device_id": "dev-alice", "workspace_id": "ws-1"})
	expectStatus(t, resp, http.StatusOK)

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/me", nil)
	var me struct {
		Context struct {
			DeviceID    string `json:"device_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"context"`
	}
	expectJSON(t, resp, http.StatusOK, &me)
	if me.Context.DeviceID != "dev-alice" || me.Context.WorkspaceID != "ws-1" {
		t.Fatalf("context not persisted: %+v", me.Context)
	}

	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/devices", nil)
	var devices []map[string]any
	expectJSON(t, resp, http.StatusOK, &devices)
	if len(devices) != 1 || devices[0]["device_id"] != "dev-alice" {
		t.Fatalf("expected only alice's device, got %+v", devices)
	}
	if devices[0]["online"] != false {
		t.Fatalf("device should be offline: %+v", devices[0])
	}

	// With context bound but no live tunnel the RPC must fail with device_offline.
	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/sessions", nil)
	var offline struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	expectJSON(t, resp, http.StatusServiceUnavailable, &offline)
	if offline.Error.Code != "device_offline" {
		t.Fatalf("error code = %q, want device_offline", offline.Error.Code)
	}
}

func TestAdminAPI(t *testing.T) {
	st, _, ts := testServer(t)
	if _, err := st.CreateUser("root", "rootpw", "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.CreateUser("alice", "secret", "user"); err != nil {
		t.Fatalf("create alice: %v", err)
	}

	client := newTestClient(t, ts, "root", "rootpw")

	resp := doRequest(t, client, http.MethodGet, ts.URL+"/api/admin/users", nil)
	var users []map[string]any
	expectJSON(t, resp, http.StatusOK, &users)
	if len(users) != 2 {
		t.Fatalf("len(users) = %d, want 2", len(users))
	}

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/admin/users", map[string]string{"username": "carol", "password": "pw1"})
	expectStatus(t, resp, http.StatusCreated)

	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/admin/users", map[string]string{"username": "carol", "password": "pw2"})
	expectStatus(t, resp, http.StatusConflict)

	resp = doRequest(t, client, http.MethodPatch, ts.URL+"/api/admin/users/carol/password", map[string]string{"password": "pw3"})
	expectStatus(t, resp, http.StatusOK)

	carolClient := newTestClient(t, ts, "carol", "pw3")
	if carolClient == nil {
		t.Fatalf("carol cannot log in with new password")
	}

	resp = doRequest(t, client, http.MethodDelete, ts.URL+"/api/admin/users/carol", nil)
	expectStatus(t, resp, http.StatusOK)

	anon := &http.Client{}
	resp = doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "carol", "password": "pw3"})
	expectStatus(t, resp, http.StatusUnauthorized)

	// Non-admin users cannot use the admin API.
	aliceClient := newTestClient(t, ts, "alice", "secret")
	resp = doRequest(t, aliceClient, http.MethodGet, ts.URL+"/api/admin/users", nil)
	expectStatus(t, resp, http.StatusForbidden)
}

func TestAdminUserStatusGuardsAndDevices(t *testing.T) {
	st, _, ts := testServer(t)
	root, err := st.CreateUser("root", "rootpw", "admin")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := st.CreateUser("bob", "bobpw", "user"); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	// 一台已绑定设备（workdir 经 hub 在线状态返回；离线时 workspaces 为空）。
	if err := st.UpsertDevice(&Device{ID: "dev-1", UserID: root.ID, Name: "PC-1", Platform: "windows", Version: "1.0"}); err != nil {
		t.Fatalf("upsert device: %v", err)
	}

	client := newTestClient(t, ts, "root", "rootpw")

	// 禁用 bob → 不能登录；启用后可登录。
	resp := doRequest(t, client, http.MethodPost, ts.URL+"/api/admin/users/bob/status", map[string]any{"disabled": true})
	expectStatus(t, resp, http.StatusOK)
	anon := &http.Client{}
	resp = doRequest(t, anon, http.MethodPost, ts.URL+"/api/login", map[string]string{"username": "bob", "password": "bobpw"})
	expectStatus(t, resp, http.StatusUnauthorized)
	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/admin/users/bob/status", map[string]any{"disabled": false})
	expectStatus(t, resp, http.StatusOK)

	// 不能禁用 / 删除自己；不能删除最后一个未禁用管理员。
	resp = doRequest(t, client, http.MethodPost, ts.URL+"/api/admin/users/root/status", map[string]any{"disabled": true})
	expectStatus(t, resp, http.StatusBadRequest)
	resp = doRequest(t, client, http.MethodDelete, ts.URL+"/api/admin/users/root", nil)
	expectStatus(t, resp, http.StatusBadRequest)

	// 设备接入：管理端可跨用户看到设备与归属账号。
	resp = doRequest(t, client, http.MethodGet, ts.URL+"/api/admin/devices", nil)
	var devices []map[string]any
	expectJSON(t, resp, http.StatusOK, &devices)
	if len(devices) != 1 || devices[0]["device_id"] != "dev-1" || devices[0]["username"] != "root" {
		t.Fatalf("devices = %#v", devices)
	}
	if _, ok := devices[0]["workspaces"]; !ok {
		t.Fatalf("devices 缺 workspaces 字段：%#v", devices[0])
	}

	// 后台管理页可直接访问（登录态由页面自己走 /api/login）。
	resp, err = anon.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("admin page status=%d ct=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestStaticServing(t *testing.T) {
	_, _, ts := testServer(t)
	client := &http.Client{}

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "remote-h5") {
		t.Fatalf("GET / status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("index Cache-Control = %q, want no-store", got)
	}

	resp, err = client.Get(ts.URL + "/sessions/abc")
	if err != nil {
		t.Fatalf("GET /sessions/abc: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "remote-h5") {
		t.Fatalf("SPA fallback status=%d body=%s", resp.StatusCode, body)
	}

	resp, err = client.Get(ts.URL + "/assets/app-abc123.js")
	if err != nil {
		t.Fatalf("GET asset: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("asset status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "max-age=31536000") {
		t.Fatalf("asset Cache-Control = %q, want long cache", got)
	}

	resp, err = client.Get(ts.URL + "/api/does-not-exist")
	if err != nil {
		t.Fatalf("GET unknown api: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown api status = %d, want 404", resp.StatusCode)
	}

	resp, err = client.Get(ts.URL + "/api/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
}
