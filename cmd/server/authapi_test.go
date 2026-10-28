package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func authAPILogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newAuthAPIServer builds a real server over a throwaway SQLite DB with the
// auth store, an empty session manager and a broker wired in.
func newAuthAPIServer(t *testing.T) (*server, *authStore) {
	t.Helper()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "auth_api_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st, err := newAuthStore(ctx, db, authAPILogger())
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}
	return &server{
		auth:       st,
		log:        authAPILogger(),
		sessions:   &SessionManager{sessions: map[string]*Session{}},
		broker:     NewBroker(),
		sessionTTL: time.Hour,
	}, st
}

func seedAuthAPIUser(t *testing.T, st *authStore, id, username, password, role string) {
	t.Helper()
	created, err := st.insertUserIfAbsent(context.Background(), id, username, []byte(password), role, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	if !created {
		t.Fatalf("user %q was not created", username)
	}
}

func seedAuthAPISession(t *testing.T, st *authStore, userID, rawToken string, ttl time.Duration) {
	t.Helper()
	now := time.Now()
	if err := st.insertSession(context.Background(), newSessionID(), userID, sha256Hex(rawToken), now.UnixMilli(), now.Add(ttl).UnixMilli()); err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

// authAPILoginResponse mirrors the documented login JSON contract: the session
// token is delivered only through the HttpOnly cookie, never in the body.
type authAPILoginResponse struct {
	ExpiresAt int64 `json:"expiresAt"`
	User      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	} `json:"user"`
}

// sessionCookieFrom returns the session cookie set by rec, failing the test if
// the response did not set one.
func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("response did not set the %q cookie (cookies=%v)", sessionCookieName, rec.Result().Cookies())
	return nil
}

func postAuthAPILogin(t *testing.T, h http.Handler, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeAuthAPILogin(t *testing.T, rec *httptest.ResponseRecorder) authAPILoginResponse {
	t.Helper()
	var out authAPILoginResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode login response %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestLoginSuccess(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-login", "alice", "correct horse battery", "admin")
	h := s.routes()

	rec := postAuthAPILogin(t, h, "alice", "correct horse battery")
	if rec.Code != http.StatusCreated {
		t.Fatalf("login: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("login Cache-Control = %q, want no-store", cc)
	}
	body := decodeAuthAPILogin(t, rec)
	if body.ExpiresAt <= 0 {
		t.Fatalf("expiresAt = %d, want a positive timestamp", body.ExpiresAt)
	}
	if body.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expiresAt = %d, want a future timestamp", body.ExpiresAt)
	}
	if body.User.ID != "user-login" || body.User.Username != "alice" || body.User.Role != "admin" {
		t.Fatalf("unexpected user in response: %+v", body.User)
	}
	// The bearer credential must not be exposed to JavaScript.
	if strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("login response leaks a token in the body: %s", rec.Body.String())
	}

	cookie := sessionCookieFrom(t, rec)
	if cookie.Value == "" {
		t.Fatalf("%q cookie has an empty value", sessionCookieName)
	}
	if !cookie.HttpOnly {
		t.Fatal("session cookie must be HttpOnly")
	}

	// Follow-up requests authenticate with the cookie value from the response.
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/auth/me with login cookie: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var me struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me response %q: %v", rec.Body.String(), err)
	}
	if me.User.ID != "user-login" || me.User.Username != "alice" || me.User.Role != "admin" {
		t.Fatalf("unexpected user from cookie auth: %+v", me.User)
	}
}

func TestLoginFailures(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-fail", "alice", "right-password", "operator")
	h := s.routes()

	t.Run("wrong password", func(t *testing.T) {
		rec := postAuthAPILogin(t, h, "alice", "wrong-password")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		rec := postAuthAPILogin(t, h, "nobody", "whatever")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("empty credentials", func(t *testing.T) {
		rec := postAuthAPILogin(t, h, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

// loginCookie performs a successful login and returns the session cookie.
func loginCookie(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	rec := postAuthAPILogin(t, h, username, password)
	if rec.Code != http.StatusCreated {
		t.Fatalf("login: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	return sessionCookieFrom(t, rec)
}

func TestLoginCookieAuthenticatesSessionList(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-cookie", "alice", "pw-alice", "operator")
	h := s.routes()

	cookie := loginCookie(t, h, "alice", "pw-alice")

	// No Authorization header: the cookie alone must authenticate.
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions with session cookie: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"sessions\"") {
		t.Fatalf("expected session list body, got %s", rec.Body.String())
	}
}

func TestCookieAuthCSRFOriginGuard(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-csrf", "alice", "pw-alice", "operator")
	h := s.routes()

	cookie := loginCookie(t, h, "alice", "pw-alice")

	// postSession sends a state-changing request authenticated by the cookie
	// only (no Authorization header), with the given Origin.
	postSession := func(origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "https://console.test/api/sessions", strings.NewReader("{not json"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// Cross-origin state change: the CSRF gate answers before the handler.
	rec := postSession("https://evil.example")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST with cookie: got %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// Same-origin: the CSRF gate lets the request through. The body is
	// malformed on purpose so handleSessionCreate answers 400 before touching
	// the (unwired) session manager; reaching the handler at all proves both
	// the cookie auth and the CSRF gate accepted the request.
	rec = postSession("https://console.test")
	if rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin POST was rejected by the CSRF gate: got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("same-origin POST did not accept cookie auth: got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestApiKeyRevokedByDelete(t *testing.T) {
	s, st := newAuthAPIServer(t)
	const adminToken = "revoke-key-admin-token"
	seedAuthAPIUser(t, st, "user-revoke", "admin", "pw-admin", "admin")
	seedAuthAPISession(t, st, "user-revoke", adminToken, time.Hour)
	h := s.routes()

	// Admin mints a key.
	body, _ := json.Marshal(map[string]string{"name": "revoked-twin"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create key response %q: %v", rec.Body.String(), err)
	}
	if created.ID == "" || created.Key == "" {
		t.Fatalf("incomplete create response: %+v", created)
	}

	// Control: the key authenticates before revocation.
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("X-API-Key", created.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("key before revoke: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Revoke it through the public API.
	req = httptest.NewRequest(http.MethodDelete, "/api/auth/keys/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke key: got %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}

	// The same key is now rejected by the middleware.
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("X-API-Key", created.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestLoginRateLimit(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-rate", "alice", "right-password", "operator")
	h := s.routes()

	for i := 0; i < loginMaxAttempts; i++ {
		rec := postAuthAPILogin(t, h, "alice", "wrong-password")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401 (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := postAuthAPILogin(t, h, "alice", "wrong-password")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt %d: got %d, want 429 (body=%s)", loginMaxAttempts+1, rec.Code, rec.Body.String())
	}

	// Even the correct password is refused while the window is saturated: the
	// limiter rejects before any credential lookup.
	rec = postAuthAPILogin(t, h, "alice", "right-password")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password while limited: got %d, want 429", rec.Code)
	}
}

// loginAttemptCount reads the current count for a limiter key; a refunded (or
// expired) bucket reads as zero.
func loginAttemptCount(s *server, key string) int {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if a := s.loginAttempts[key]; a != nil {
		return a.count
	}
	return 0
}

func TestLoginSuccessResetsRateLimit(t *testing.T) {
	s, st := newAuthAPIServer(t)
	seedAuthAPIUser(t, st, "user-reset", "alice", "right-password", "operator")
	h := s.routes()

	// A successful login settles its own reservation: it deletes the
	// (ip,user) bucket and refunds the shared per-IP and global buckets, so the
	// next attempt is not pre-charged.
	rec := postAuthAPILogin(t, h, "alice", "right-password")
	if rec.Code != http.StatusCreated {
		t.Fatalf("login: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}

	probe := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	key := s.loginKey(probe, "alice")
	ipKey := "ip:" + truncateBytes(s.clientIP(probe), loginMaxKeyIP)
	globalKey := "global"

	s.loginMu.Lock()
	_, present := s.loginAttempts[key]
	count := 0
	if a := s.loginAttempts[key]; a != nil {
		count = a.count
	}
	s.loginMu.Unlock()
	if present {
		t.Fatalf("login limiter still has key %q after a successful login (count=%d)", key, count)
	}
	// The shared buckets bound total login work regardless of username, so a
	// success refunds exactly the unit it reserved and leaves nothing behind.
	for _, k := range []string{ipKey, globalKey} {
		if n := loginAttemptCount(s, k); n != 0 {
			t.Fatalf("shared login bucket %q = %d after a successful login, want 0", k, n)
		}
	}

	// Only failed attempts consume the shared buckets: one failure followed by
	// a success keeps exactly that failure charged.
	if rec := postAuthAPILogin(t, h, "alice", "wrong-password"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("failed attempt: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec := postAuthAPILogin(t, h, "alice", "right-password"); rec.Code != http.StatusCreated {
		t.Fatalf("login after failure: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	if n := loginAttemptCount(s, ipKey); n != 1 {
		t.Fatalf("shared IP bucket = %d after one failure and one success, want 1", n)
	}
	s.loginMu.Lock()
	_, present = s.loginAttempts[key]
	s.loginMu.Unlock()
	if present {
		t.Fatalf("login limiter still has key %q after a success", key)
	}

	rec = postAuthAPILogin(t, h, "alice", "wrong-password")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-success failure: got %d, want 401 (not 429)", rec.Code)
	}
}

func TestAuthLogout(t *testing.T) {
	s, st := newAuthAPIServer(t)
	const token = "logout-session-token"
	seedAuthAPIUser(t, st, "user-logout", "alice", "pw", "admin")
	seedAuthAPISession(t, st, "user-logout", token, time.Hour)
	h := s.routes()

	// Authenticated logout revokes the session.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout with token: got %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}

	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, meReq)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// Logout stays public and idempotent without credentials.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout without token: got %d, want 204", rec.Code)
	}
}

func TestAuthMe(t *testing.T) {
	s, st := newAuthAPIServer(t)
	const token = "me-session-token"
	seedAuthAPIUser(t, st, "user-me", "alice", "pw", "operator")
	seedAuthAPISession(t, st, "user-me", token, time.Hour)
	h := s.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		User struct {
			ID       string `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode me response %q: %v", rec.Body.String(), err)
	}
	if out.User.ID != "user-me" || out.User.Username != "alice" || out.User.Role != "operator" {
		t.Fatalf("unexpected user: %+v", out.User)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated me: got %d, want 401", rec.Code)
	}
}

func TestApiKeyLifecycle(t *testing.T) {
	s, st := newAuthAPIServer(t)
	const adminToken = "admin-key-token"
	const operatorToken = "operator-key-token"
	seedAuthAPIUser(t, st, "user-admin", "admin", "pw-admin", "admin")
	seedAuthAPIUser(t, st, "user-op", "operator", "pw-op", "operator")
	seedAuthAPISession(t, st, "user-admin", adminToken, time.Hour)
	seedAuthAPISession(t, st, "user-op", operatorToken, time.Hour)
	h := s.routes()

	// Create: the raw key is returned exactly once.
	body, _ := json.Marshal(map[string]string{"name": "ci-key"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create key: got %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Key    string `json:"key"`
		Prefix string `json:"prefix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create key response %q: %v", rec.Body.String(), err)
	}
	if created.ID == "" || created.Key == "" || created.Name != "ci-key" {
		t.Fatalf("incomplete create response: %+v", created)
	}
	if created.Prefix != created.Key[:apiKeyPrefixLen] {
		t.Fatalf("prefix = %q, want %q", created.Prefix, created.Key[:apiKeyPrefixLen])
	}

	// List: never exposes the raw key or its hash.
	req = httptest.NewRequest(http.MethodGet, "/api/auth/keys", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list keys: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var listed struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list keys response %q: %v", rec.Body.String(), err)
	}
	if len(listed.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(listed.Keys))
	}
	if _, ok := listed.Keys[0]["key"]; ok {
		t.Fatal("list response leaks the raw key")
	}
	for _, forbidden := range []string{"hash", "key_hash", "keyHash"} {
		if _, ok := listed.Keys[0][forbidden]; ok {
			t.Fatalf("list response exposes %q", forbidden)
		}
	}
	if strings.Contains(rec.Body.String(), created.Key) {
		t.Fatal("list response contains the raw key")
	}
	if got, _ := listed.Keys[0]["prefix"].(string); got != created.Prefix {
		t.Fatalf("listed prefix = %q, want %q", got, created.Prefix)
	}

	// Operator cannot mint keys.
	req = httptest.NewRequest(http.MethodPost, "/api/auth/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("operator create key: got %d, want 403", rec.Code)
	}

	// The key authenticates before revocation...
	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("X-API-Key", created.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("key before revoke: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// ...and stops working after revocation.
	req = httptest.NewRequest(http.MethodDelete, "/api/auth/keys/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke key: got %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("X-API-Key", created.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("key after revoke: got %d, want 401", rec.Code)
	}
}
