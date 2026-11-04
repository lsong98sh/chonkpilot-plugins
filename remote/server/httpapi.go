package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Server wires the HTTP API, the SSE stream and the static H5 site.
type Server struct {
	st      *Store
	hub     *Hub
	limiter *loginLimiter
	web     fs.FS
	log     *slog.Logger
}

// NewServer builds an HTTP handler around the given store and tunnel hub.
func NewServer(st *Store, hub *Hub, webFS fs.FS, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		st:      st,
		hub:     hub,
		limiter: newLoginLimiter(5, time.Minute),
		web:     webFS,
		log:     logger,
	}
}

type authInfo struct {
	user      *User
	sess      *H5Session
	tokenHash string
}

type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) write(w http.ResponseWriter) {
	writeError(w, e.status, e.code, e.message)
}

func errBadRequest(msg string) *httpError {
	return &httpError{http.StatusBadRequest, "bad_request", msg}
}

func errNoContext() *httpError {
	return &httpError{http.StatusBadRequest, "no_context", "select a device and workspace first"}
}

func errUnauthorized(msg string) *httpError {
	return &httpError{http.StatusUnauthorized, "unauthorized", msg}
}

func errForbidden(msg string) *httpError {
	return &httpError{http.StatusForbidden, "forbidden", msg}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("GET /api/me", s.withAuth(s.handleMe))
	mux.HandleFunc("GET /api/devices", s.withAuth(s.handleDevices))
	mux.HandleFunc("POST /api/context", s.withAuth(s.handleContext))

	mux.HandleFunc("GET /api/sessions", s.withAuth(s.handleSessionsList))
	mux.HandleFunc("POST /api/sessions", s.withAuth(s.handleSessionCreate))
	mux.HandleFunc("GET /api/sessions/{id}/messages", s.withAuth(s.handleSessionHistory))
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.withAuth(s.handleSessionSend))
	mux.HandleFunc("GET /api/sessions/{id}/inbox", s.withAuth(s.handleSessionInbox))
	mux.HandleFunc("POST /api/sessions/{id}/inbox/{action}", s.withAuth(s.handleSessionInboxAction))
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.withAuth(s.handleSessionStop))
	mux.HandleFunc("POST /api/sessions/{id}/answer", s.withAuth(s.handleSessionAnswer))
	mux.HandleFunc("GET /api/sessions/{id}/tasks", s.withAuth(s.handleSessionTasks))
	mux.HandleFunc("POST /api/tasks/{id}/stop", s.withAuth(s.handleTaskStop))

	mux.HandleFunc("GET /api/files", s.withAuth(s.handleFilesList))
	mux.HandleFunc("GET /api/files/content", s.withAuth(s.handleFilesRead))
	mux.HandleFunc("GET /api/events", s.withAuth(s.handleEvents))
	mux.HandleFunc("GET /api/healthz", s.handleHealthz)

	mux.HandleFunc("GET /api/admin/users", s.withAdmin(s.handleAdminListUsers))
	mux.HandleFunc("POST /api/admin/users", s.withAdmin(s.handleAdminCreateUser))
	mux.HandleFunc("PATCH /api/admin/users/{name}/password", s.withAdmin(s.handleAdminSetPassword))
	mux.HandleFunc("POST /api/admin/users/{name}/status", s.withAdmin(s.handleAdminSetUserStatus))
	mux.HandleFunc("DELETE /api/admin/users/{name}", s.withAdmin(s.handleAdminDeleteUser))
	mux.HandleFunc("GET /api/admin/devices", s.withAdmin(s.handleAdminDevices))

	// 后台管理页（独立静态页，非 H5 应用；登录后按 admin 角色调用上面的 API）
	mux.HandleFunc("GET /admin", s.handleAdminPage)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
	})
	mux.HandleFunc("GET /tunnel", s.handleTunnel)
	mux.HandleFunc("/", s.handleStatic)

	return secureHeaders(mux)
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- json helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"encode failed"}}`))
		return
	}
	writeRawJSON(w, status, raw)
}

func writeRawJSON(w http.ResponseWriter, status int, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(status)
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	_, _ = w.Write(raw)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

func decodeBody(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// ---------------------------------------------------------------- auth middleware

func (s *Server) withAuth(fn func(http.ResponseWriter, *http.Request, *authInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ai, herr := s.authenticate(r)
		if herr != nil {
			herr.write(w)
			return
		}
		fn(w, r, ai)
	}
}

func (s *Server) withAdmin(fn func(http.ResponseWriter, *http.Request, *authInfo)) http.HandlerFunc {
	return s.withAuth(func(w http.ResponseWriter, r *http.Request, ai *authInfo) {
		if ai.user.Role != "admin" {
			errForbidden("admin role required").write(w)
			return
		}
		fn(w, r, ai)
	})
}

func (s *Server) authenticate(r *http.Request) (*authInfo, *httpError) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return nil, errUnauthorized("login required")
	}
	tokenHash := hashToken(cookie.Value)
	sess, err := s.st.GetSession(tokenHash)
	if err != nil {
		return nil, errUnauthorized("session expired or invalid")
	}
	user, err := s.st.GetUserByID(sess.UserID)
	if err != nil {
		return nil, errUnauthorized("account no longer exists")
	}
	if user.Disabled {
		return nil, errUnauthorized("account disabled")
	}
	return &authInfo{user: user, sess: sess, tokenHash: tokenHash}, nil
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------- login / session

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	if body.Username == "" || body.Password == "" {
		errBadRequest("username and password are required").write(w)
		return
	}

	key := clientIP(r) + "|" + body.Username
	if s.limiter.blocked(key) {
		writeError(w, http.StatusTooManyRequests, "too_many_requests", "too many failed login attempts, try again later")
		return
	}

	user, err := s.st.GetUser(body.Username)
	if err != nil || user.Disabled || !bcryptCheck(user.PasswordHash, body.Password) {
		s.limiter.recordFailure(key)
		errUnauthorized("invalid username or password").write(w)
		return
	}

	token, err := randomHex(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create session")
		return
	}
	now := time.Now()
	if err := s.st.PutSession(hashToken(token), H5Session{
		UserID:    user.ID,
		ExpiresAt: now.Add(sessionTTL).Unix(),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot create session")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
		Secure:   r.TLS != nil,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"user": map[string]string{"id": user.ID, "username": user.Username, "role": user.Role},
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	if err := s.st.DeleteSession(ai.tokenHash); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot delete session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	resp := map[string]any{
		"user": map[string]string{
			"id":       ai.user.ID,
			"username": ai.user.Username,
			"role":     ai.user.Role,
		},
	}
	if ai.sess.DeviceID != "" {
		resp["context"] = map[string]string{
			"device_id":    ai.sess.DeviceID,
			"workspace_id": ai.sess.WorkspaceID,
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- devices

type workspaceView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type deviceView struct {
	DeviceID   string          `json:"device_id"`
	Name       string          `json:"name"`
	Platform   string          `json:"platform"`
	Version    string          `json:"version"`
	Online     bool            `json:"online"`
	LastSeen   int64           `json:"last_seen"`
	Protocol   string          `json:"protocol"`
	Workspaces []workspaceView `json:"workspaces"`
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	devices, err := s.st.ListDevicesByUser(ai.user.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot list devices")
		return
	}
	out := make([]deviceView, 0, len(devices))
	for _, dev := range devices {
		online, workspaces, protocol := s.hub.Status(dev.ID)
		views := make([]workspaceView, 0, len(workspaces))
		for _, ws := range workspaces {
			views = append(views, workspaceView{ID: ws.ID, Name: ws.Name, Path: ws.Path})
		}
		lastSeen := dev.LastSeen
		if lastSeen == 0 {
			lastSeen = dev.CreatedAt
		}
		out = append(out, deviceView{
			DeviceID:   dev.ID,
			Name:       dev.Name,
			Platform:   dev.Platform,
			Version:    dev.Version,
			Online:     online,
			LastSeen:   lastSeen,
			Protocol:   protocol,
			Workspaces: views,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) requireDevice(userID, deviceID string) (*Device, *httpError) {
	if deviceID == "" {
		return nil, &httpError{http.StatusBadRequest, "bad_request", "device_id is required"}
	}
	dev, err := s.st.GetDevice(deviceID)
	if err != nil {
		return nil, &httpError{http.StatusNotFound, "device_not_found", "device not found"}
	}
	if dev.UserID != userID {
		return nil, errForbidden("device belongs to another user")
	}
	return dev, nil
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	var body struct {
		DeviceID    string `json:"device_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	if _, herr := s.requireDevice(ai.user.ID, body.DeviceID); herr != nil {
		herr.write(w)
		return
	}
	if body.WorkspaceID == "" {
		errBadRequest("workspace_id is required").write(w)
		return
	}
	if online, workspaces, _ := s.hub.Status(body.DeviceID); online {
		found := false
		for _, ws := range workspaces {
			if ws.ID == body.WorkspaceID {
				found = true
				break
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "workspace_not_found", "workspace is not open on the device")
			return
		}
	}

	ai.sess.DeviceID = body.DeviceID
	ai.sess.WorkspaceID = body.WorkspaceID
	if ai.sess.ExpiresAt == 0 {
		ai.sess.ExpiresAt = time.Now().Add(sessionTTL).Unix()
	}
	if err := s.st.PutSession(ai.tokenHash, *ai.sess); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot save context")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- rpc passthrough

// callDevice enforces context + ownership, forwards one RPC and writes the raw
// device result. It reports whether a response was already written.
func (s *Server) callDevice(w http.ResponseWriter, r *http.Request, ai *authInfo, method string, args any) bool {
	deviceID := ai.sess.DeviceID
	workspaceID := ai.sess.WorkspaceID
	if deviceID == "" || workspaceID == "" {
		errNoContext().write(w)
		return false
	}
	if _, herr := s.requireDevice(ai.user.ID, deviceID); herr != nil {
		herr.write(w)
		return false
	}

	timeout := 20 * time.Second
	if method == "sessions.history" {
		timeout = 60 * time.Second
	}
	result, err := s.hub.Call(r.Context(), deviceID, ai.user.ID, workspaceID, method, args, timeout)
	if err != nil {
		s.writeCallError(w, err)
		return false
	}
	writeRawJSON(w, http.StatusOK, result)
	return true
}

func (s *Server) writeCallError(w http.ResponseWriter, err error) {
	var devErr *DeviceError
	switch {
	case errors.Is(err, ErrDeviceOffline):
		writeError(w, http.StatusServiceUnavailable, "device_offline", "the desktop is not connected")
	case errors.Is(err, ErrMethodNotAllowed):
		writeError(w, http.StatusBadRequest, "method_not_allowed", "rpc method is not allowed")
	case errors.Is(err, ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "device belongs to another user")
	case errors.Is(err, ErrCallTimeout):
		writeError(w, http.StatusBadGateway, "device_timeout", "the desktop did not answer in time")
	case errors.Is(err, context.Canceled):
		// client went away; nothing useful to write
	case errors.As(err, &devErr):
		writeError(w, http.StatusBadGateway, "device_error", devErr.Error())
	default:
		writeError(w, http.StatusBadGateway, "device_error", err.Error())
	}
	s.log.Warn("device rpc failed", "err", err)
}

func (s *Server) handleSessionsList(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	s.callDevice(w, r, ai, "sessions.list", map[string]any{})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	var body struct {
		Text string `json:"text"`
	}
	if r.ContentLength != 0 {
		if err := decodeBody(r, &body); err != nil {
			errBadRequest("invalid JSON body").write(w)
			return
		}
	}
	args := map[string]any{}
	if strings.TrimSpace(body.Text) != "" {
		args["text"] = body.Text
	}
	s.callDevice(w, r, ai, "sessions.create", args)
}

func (s *Server) handleSessionHistory(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	id := r.PathValue("id")
	q := r.URL.Query()
	args := map[string]any{"session_id": id}
	if v := q.Get("dialog_id"); v != "" {
		args["dialog_id"] = v
	}
	if v := q.Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			args["before"] = n
		} else {
			args["before"] = v
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			args["limit"] = n
		}
	}
	s.callDevice(w, r, ai, "sessions.history", args)
}

func (s *Server) handleSessionSend(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	var body struct {
		Text     string `json:"text"`
		DialogID string `json:"dialog_id"`
		Insert   bool   `json:"insert"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		errBadRequest("text is required").write(w)
		return
	}
	args := map[string]any{"session_id": r.PathValue("id"), "text": body.Text}
	if body.DialogID != "" {
		args["dialog_id"] = body.DialogID
	}
	if body.Insert {
		args["insert"] = true
	}
	s.callDevice(w, r, ai, "sessions.send", args)
}

// handleSessionInbox：读该会话**全量** pending inbox（2026-11-01：两端共享，不按来源过滤）。
func (s *Server) handleSessionInbox(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	s.callDevice(w, r, ai, "sessions.inbox", map[string]any{"session_id": r.PathValue("id")})
}

// handleSessionInboxAction：inbox 项操作（插话意向 / 取消意向 / 删除·撤回）。
// 路由：POST /api/sessions/{id}/inbox/{action}，action = promote | revert | discard。
func (s *Server) handleSessionInboxAction(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	action := r.PathValue("action")
	method := ""
	switch action {
	case "promote":
		method = "sessions.inbox.promote"
	case "revert":
		method = "sessions.inbox.revert"
	case "discard":
		method = "sessions.inbox.discard"
	default:
		errBadRequest("unknown inbox action: " + action).write(w)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	s.callDevice(w, r, ai, method, map[string]any{"session_id": r.PathValue("id"), "id": body.ID})
}

func (s *Server) handleSessionStop(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	s.callDevice(w, r, ai, "sessions.stop", map[string]any{"session_id": r.PathValue("id")})
}

func (s *Server) handleSessionAnswer(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	var body struct {
		AskID  string          `json:"ask_id"`
		Answer string          `json:"answer"`
		Custom json.RawMessage `json:"custom"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	if body.AskID == "" || body.Answer == "" {
		errBadRequest("ask_id and answer are required").write(w)
		return
	}
	args := map[string]any{
		"session_id": r.PathValue("id"),
		"ask_id":     body.AskID,
		"answer":     body.Answer,
	}
	if len(body.Custom) > 0 && string(body.Custom) != "null" {
		args["custom"] = body.Custom
	}
	s.callDevice(w, r, ai, "ask.answer", args)
}

func (s *Server) handleSessionTasks(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	s.callDevice(w, r, ai, "tasks.tree", map[string]any{"session_id": r.PathValue("id")})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	s.callDevice(w, r, ai, "tasks.stop", map[string]any{"task_id": r.PathValue("id")})
}

func (s *Server) handleFilesList(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	args := map[string]any{}
	if v := r.URL.Query().Get("path"); v != "" {
		args["path"] = v
	}
	s.callDevice(w, r, ai, "files.list", args)
}

func (s *Server) handleFilesRead(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	args := map[string]any{}
	if v := r.URL.Query().Get("path"); v != "" {
		args["path"] = v
	}
	s.callDevice(w, r, ai, "files.read", args)
}

// ---------------------------------------------------------------- SSE

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	if ai.sess.DeviceID == "" || ai.sess.WorkspaceID == "" {
		errNoContext().write(w)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal", "streaming unsupported")
		return
	}

	subID, events := s.hub.Subscribe(ai.user.ID, ai.sess.DeviceID, ai.sess.WorkspaceID)
	defer s.hub.Unsubscribe(subID)

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-events:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- admin

func userView(u *User) map[string]any {
	return map[string]any{
		"id":         u.ID,
		"username":   u.Username,
		"role":       u.Role,
		"disabled":   u.Disabled,
		"created_at": u.CreatedAt,
	}
}

func (s *Server) handleAdminListUsers(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	users, err := s.st.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot list users")
		return
	}
	out := make([]map[string]any, 0, len(users))
	for i := range users {
		out = append(out, userView(&users[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAdminCreateUser(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	if body.Role == "" {
		body.Role = "user"
	}
	if body.Role != "user" && body.Role != "admin" {
		errBadRequest("role must be admin or user").write(w)
		return
	}
	user, err := s.st.CreateUser(body.Username, body.Password, body.Role)
	if err != nil {
		if errors.Is(err, ErrUserExists) {
			writeError(w, http.StatusConflict, "user_exists", "username already exists")
			return
		}
		errBadRequest(err.Error()).write(w)
		return
	}
	writeJSON(w, http.StatusCreated, userView(user))
}

func (s *Server) handleAdminSetPassword(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	name := r.PathValue("name")
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	if body.Password == "" {
		errBadRequest("password is required").write(w)
		return
	}
	if err := s.st.SetPassword(name, body.Password); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "cannot update password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminSetUserStatus 启用 / 禁用账号；禁止禁用自己（避免当场锁死），
// 也禁止禁用最后一个未禁用管理员。
func (s *Server) handleAdminSetUserStatus(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	name := r.PathValue("name")
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		errBadRequest("invalid JSON body").write(w)
		return
	}
	user, err := s.st.GetUser(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "user_not_found", "user not found")
		return
	}
	if body.Disabled {
		if user.Username == ai.user.Username {
			errBadRequest("cannot disable yourself").write(w)
			return
		}
		if user.Role == "admin" && !user.Disabled {
			if n, err := s.st.CountActiveAdmins(); err == nil && n <= 1 {
				errBadRequest("cannot disable the last active admin").write(w)
				return
			}
		}
	}
	if err := s.st.SetUserDisabled(name, body.Disabled); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot update user status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": body.Disabled})
}

// handleAdminDeleteUser 删除账号；禁止删除自己与最后一个未禁用管理员。
func (s *Server) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	name := r.PathValue("name")
	if strings.EqualFold(name, ai.user.Username) {
		errBadRequest("cannot delete yourself").write(w)
		return
	}
	if user, err := s.st.GetUser(name); err == nil && user.Role == "admin" && !user.Disabled {
		if n, err := s.st.CountActiveAdmins(); err == nil && n <= 1 {
			errBadRequest("cannot delete the last active admin").write(w)
			return
		}
	}
	if err := s.st.DeleteUser(name); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusNotFound, "user_not_found", "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "cannot delete user")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAdminDevices 返回全部用户的设备接入情况（含每个工作区的 workdir 路径与在线状态）。
func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request, ai *authInfo) {
	devices, err := s.st.ListAllDevices()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cannot list devices")
		return
	}
	users, _ := s.st.ListUsers()
	byID := make(map[string]string, len(users))
	for i := range users {
		byID[users[i].ID] = users[i].Username
	}
	out := make([]map[string]any, 0, len(devices))
	for _, dev := range devices {
		online, workspaces, protocol := s.hub.Status(dev.ID)
		views := make([]workspaceView, 0, len(workspaces))
		for _, ws := range workspaces {
			views = append(views, workspaceView{ID: ws.ID, Name: ws.Name, Path: ws.Path})
		}
		lastSeen := dev.LastSeen
		if lastSeen == 0 {
			lastSeen = dev.CreatedAt
		}
		out = append(out, map[string]any{
			"device_id":  dev.ID,
			"username":   byID[dev.UserID],
			"name":       dev.Name,
			"platform":   dev.Platform,
			"version":    dev.Version,
			"online":     online,
			"last_seen":  lastSeen,
			"protocol":   protocol,
			"workspaces": views,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAdminPage 输出独立的后台管理页（单文件 HTML，无构建步骤）。
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	data, err := adminPageHTML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "admin page missing")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------- static H5

func (s *Server) serveIndex(w http.ResponseWriter, status int) {
	data, err := fs.ReadFile(s.web, "index.html")
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "index.html not found")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") || p == "/tunnel" || strings.HasPrefix(p, "/tunnel/") {
		writeError(w, http.StatusNotFound, "not_found", "not found")
		return
	}

	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" || clean == "." {
		s.serveIndex(w, http.StatusOK)
		return
	}

	if data, err := fs.ReadFile(s.web, clean); err == nil {
		ct := mime.TypeByExtension(path.Ext(clean))
		if ct == "" {
			ct = "application/octet-stream"
		}
		h := w.Header()
		h.Set("Content-Type", ct)
		if strings.HasPrefix(clean, "assets/") {
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return
	}
	// SPA fallback for client-side routes.
	s.serveIndex(w, http.StatusOK)
}
