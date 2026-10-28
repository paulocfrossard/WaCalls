package main

import (
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

// authMwLogger is a local, silent logger so these tests never write to the
// shared process output and do not depend on helpers from other test files.
func authMwLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newAuthMiddlewareTestServer builds a real server backed by a throwaway
// SQLite database. Only the pieces the auth middleware and the handlers it
// guards touch are wired: auth store, session manager (empty), broker and log.
func newAuthMiddlewareTestServer(t *testing.T) (*server, *authStore) {
	t.Helper()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "auth_middleware_test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st, err := newAuthStore(ctx, db, authMwLogger())
	if err != nil {
		t.Fatalf("new auth store: %v", err)
	}

	s := &server{
		auth:       st,
		log:        authMwLogger(),
		sessions:   &SessionManager{sessions: map[string]*Session{}},
		broker:     NewBroker(),
		sessionTTL: time.Hour,
	}
	return s, st
}

// seedAuthMwUser inserts a user with a bcrypt-hashed password.
func seedAuthMwUser(t *testing.T, st *authStore, id, username, password, role string) {
	t.Helper()
	created, err := st.insertUserIfAbsent(context.Background(), id, username, []byte(password), role, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	if !created {
		t.Fatalf("user %q was not created", username)
	}
}

// seedAuthMwSession inserts a session and returns its id so a caller can
// revoke it. The caller already owns the raw token.
func seedAuthMwSession(t *testing.T, st *authStore, userID, rawToken string, ttl time.Duration) string {
	t.Helper()
	id := newSessionID()
	now := time.Now()
	if err := st.insertSession(context.Background(), id, userID, sha256Hex(rawToken), now.UnixMilli(), now.Add(ttl).UnixMilli()); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

// doAuthMwRequest drives the handler with a recorder and returns it.
func doAuthMwRequest(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// assertAuthMwStatus calls GET /api/sessions with a Bearer token and asserts
// the status code.
func assertAuthMwStatus(t *testing.T, h http.Handler, token string, want int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := doAuthMwRequest(h, req)
	if rec.Code != want {
		t.Fatalf("Bearer %q: got %d, want %d (body=%s)", token, rec.Code, want, rec.Body.String())
	}
}

// cancelOnFlushRecorder cancels a request context the first time the handler
// flushes. serveSSE writes its headers and initial frames before it blocks on
// the context, so this ends the stream deterministically without a sleep.
type cancelOnFlushRecorder struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (r *cancelOnFlushRecorder) Flush() {
	r.cancel()
	r.ResponseRecorder.Flush()
}

func TestAuthMiddlewareRequiresCredentials(t *testing.T) {
	s, _ := newAuthMiddlewareTestServer(t)
	h := s.routes()

	rec := doAuthMwRequest(h, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/sessions without credentials: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("401 content type = %q, want JSON", ct)
	}
}

func TestAuthMiddlewareSessionToken(t *testing.T) {
	s, st := newAuthMiddlewareTestServer(t)
	const token = "valid-session-token"
	seedAuthMwUser(t, st, "user-session", "alice", "pw-alice", "admin")
	seedAuthMwSession(t, st, "user-session", token, time.Hour)
	h := s.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := doAuthMwRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session token: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "\"sessions\"") {
		t.Fatalf("expected session list body, got %s", rec.Body.String())
	}
}

// TestAuthMiddlewareRejectsUnusableSessions is the failing twin of
// TestAuthMiddlewareSessionToken: each subtest presents a credential the store
// knows about but that must not authenticate. Removing authMiddleware, or
// dropping the expiry/revocation checks, turns these responses into 200s and
// fails the assertions.
func TestAuthMiddlewareRejectsUnusableSessions(t *testing.T) {
	t.Run("unknown token", func(t *testing.T) {
		s, st := newAuthMiddlewareTestServer(t)
		seedAuthMwUser(t, st, "user-twin", "twin", "pw-twin", "operator")
		// Control: a live session proves the fixture authenticates at all, so
		// the 401 below is about the credential and not a broken setup.
		seedAuthMwSession(t, st, "user-twin", "live-twin-token", time.Hour)
		h := s.routes()

		assertAuthMwStatus(t, h, "live-twin-token", http.StatusOK)
		assertAuthMwStatus(t, h, "never-issued-token", http.StatusUnauthorized)

		// The rejection must be the middleware's JSON 401, not a stray error.
		req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		req.Header.Set("Authorization", "Bearer never-issued-token")
		rec := doAuthMwRequest(h, req)
		if !strings.Contains(rec.Body.String(), "unauthorized") {
			t.Fatalf("unknown token 401 body = %q, want an unauthorized error", rec.Body.String())
		}
	})

	t.Run("expired session", func(t *testing.T) {
		s, st := newAuthMiddlewareTestServer(t)
		seedAuthMwUser(t, st, "user-expired", "expired", "pw-expired", "operator")
		// expiresAt one hour in the past: a known row that is no longer usable,
		// so this fails if the middleware stops checking expiresAt.
		seedAuthMwSession(t, st, "user-expired", "expired-token", -time.Hour)
		h := s.routes()

		assertAuthMwStatus(t, h, "expired-token", http.StatusUnauthorized)
	})

	t.Run("revoked session", func(t *testing.T) {
		s, st := newAuthMiddlewareTestServer(t)
		seedAuthMwUser(t, st, "user-revoked", "revoked", "pw-revoked", "operator")
		sessionID := seedAuthMwSession(t, st, "user-revoked", "revoked-token", time.Hour)
		if err := st.revokeSession(context.Background(), sessionID); err != nil {
			t.Fatalf("revoke session: %v", err)
		}
		h := s.routes()

		assertAuthMwStatus(t, h, "revoked-token", http.StatusUnauthorized)
	})
}

func TestAuthMiddlewareAPIKey(t *testing.T) {
	s, st := newAuthMiddlewareTestServer(t)
	seedAuthMwUser(t, st, "user-apikey", "bob", "pw-bob", "operator")
	const rawKey = "wc_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.insertApiKey(context.Background(), newSessionID(), "user-apikey", "ci", sha256Hex(rawKey), rawKey[:8], time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	h := s.routes()

	req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	req.Header.Set("X-API-Key", rawKey)
	rec := doAuthMwRequest(h, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid X-API-Key: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddlewareMalformedAuthorization(t *testing.T) {
	s, _ := newAuthMiddlewareTestServer(t)
	h := s.routes()

	for _, value := range []string{"Bearer", "Bearer ", "Basic abc123"} {
		t.Run(value, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
			req.Header.Set("Authorization", value)
			rec := doAuthMwRequest(h, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("Authorization %q: got %d, want 401", value, rec.Code)
			}
		})
	}
}

func TestAuthMiddlewareQueryCredentials(t *testing.T) {
	s, st := newAuthMiddlewareTestServer(t)
	const token = "query-credential-token"
	seedAuthMwUser(t, st, "user-query", "carol", "pw-carol", "operator")
	seedAuthMwSession(t, st, "user-query", token, time.Hour)
	h := s.routes()

	// Control: the same token authenticates through the header, so the 401
	// below comes from the query-credential restriction and not from a dead
	// or mistyped token.
	assertAuthMwStatus(t, h, token, http.StatusOK)

	// Query credentials must be rejected everywhere except GET /api/events,
	// where only API keys and single-use tickets are accepted.
	rec := doAuthMwRequest(h, httptest.NewRequest(http.MethodGet, "/api/sessions?token="+token, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token on /api/sessions: got %d, want 401", rec.Code)
	}

	// A session token in the URL is no longer a valid credential, not even on
	// the SSE endpoint: callers must exchange it for a short-lived ticket.
	rec = doAuthMwRequest(h, httptest.NewRequest(http.MethodGet, "/api/events?token="+token, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("query token on /api/events: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// Exchange the session token for a single-use SSE ticket.
	ticketReq := httptest.NewRequest(http.MethodPost, "/api/auth/sse-ticket", nil)
	ticketReq.Header.Set("Authorization", "Bearer "+token)
	ticketRec := doAuthMwRequest(h, ticketReq)
	if ticketRec.Code != http.StatusCreated {
		t.Fatalf("POST /api/auth/sse-ticket: got %d, want 201 (body=%s)", ticketRec.Code, ticketRec.Body.String())
	}
	var ticketBody struct {
		Ticket    string `json:"ticket"`
		ExpiresAt int64  `json:"expiresAt"`
	}
	if err := json.Unmarshal(ticketRec.Body.Bytes(), &ticketBody); err != nil {
		t.Fatalf("decode sse ticket: %v (body=%s)", err, ticketRec.Body.String())
	}
	if ticketBody.Ticket == "" {
		t.Fatalf("sse ticket is empty (body=%s)", ticketRec.Body.String())
	}
	if ticketBody.ExpiresAt == 0 {
		t.Fatalf("sse ticket expiresAt is missing (body=%s)", ticketRec.Body.String())
	}

	// The ticket authenticates one SSE stream. The context is cancelled on the
	// first flush: serveSSE writes its headers and initial frames before
	// blocking on the context, so this ends the stream without a sleep or a
	// leaked goroutine.
	req := httptest.NewRequest(http.MethodGet, "/api/events?ticket="+ticketBody.Ticket, nil)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	rec = &httptest.ResponseRecorder{}
	streamRec := &cancelOnFlushRecorder{ResponseRecorder: rec, cancel: cancel}
	h.ServeHTTP(streamRec, req)
	cancel() // idempotent; covers a stream that returned before flushing
	if rec.Code != http.StatusOK {
		t.Fatalf("query ticket on /api/events: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("events content type = %q, want text/event-stream", ct)
	}

	// The ticket is single use: replaying the same value must be rejected.
	rec = doAuthMwRequest(h, httptest.NewRequest(http.MethodGet, "/api/events?ticket="+ticketBody.Ticket, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket on /api/events: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestAuthMiddlewareAPIKeyQueryRejected pins the removal of the ?apiKey=
// fallback: an API key authenticates only through X-API-Key or
// Authorization: Bearer. The same key in the query string is not a credential
// anywhere, including the SSE endpoint where a ticket is the only accepted
// query credential.
func TestAuthMiddlewareAPIKeyQueryRejected(t *testing.T) {
	s, st := newAuthMiddlewareTestServer(t)
	seedAuthMwUser(t, st, "user-apikey-query", "dave", "pw-dave", "operator")
	const rawKey = "wc_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := st.insertApiKey(context.Background(), newSessionID(), "user-apikey-query", "ci", sha256Hex(rawKey), rawKey[:apiKeyPrefixLen], time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	h := s.routes()

	// Control: this exact key authenticates through both documented headers,
	// so the rejections below are about the query string and not a dead key.
	headerReq := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	headerReq.Header.Set("X-API-Key", rawKey)
	if rec := doAuthMwRequest(h, headerReq); rec.Code != http.StatusOK {
		t.Fatalf("X-API-Key: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	bearerReq := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	bearerReq.Header.Set("Authorization", "Bearer "+rawKey)
	if rec := doAuthMwRequest(h, bearerReq); rec.Code != http.StatusOK {
		t.Fatalf("Bearer api key: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	for _, target := range []string{
		"/api/sessions?apiKey=" + rawKey,
		"/api/events?apiKey=" + rawKey,
	} {
		rec := doAuthMwRequest(h, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s: got %d, want 401 (body=%s)", target, rec.Code, rec.Body.String())
		}
	}
}

func TestAuthMiddlewarePublicPaths(t *testing.T) {
	s, _ := newAuthMiddlewareTestServer(t)
	h := s.routes()

	// /api/auth/login is public; invalid JSON reaches the handler (400), which
	// proves the middleware did not answer 401 first.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := doAuthMwRequest(h, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("public login with bad JSON: got %d, want 400", rec.Code)
	}

	// CORS preflight is answered before authentication.
	rec = doAuthMwRequest(h, httptest.NewRequest(http.MethodOptions, "/api/sessions", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS /api/sessions: got %d, want 204", rec.Code)
	}
}

func TestAuthMiddlewareRoleEnforcement(t *testing.T) {
	s, st := newAuthMiddlewareTestServer(t)
	const operatorToken = "operator-token"
	const adminToken = "admin-token"
	seedAuthMwUser(t, st, "user-op", "operator1", "pw-op", "operator")
	seedAuthMwUser(t, st, "user-ad", "admin1", "pw-ad", "admin")
	seedAuthMwSession(t, st, "user-op", operatorToken, time.Hour)
	seedAuthMwSession(t, st, "user-ad", adminToken, time.Hour)
	h := s.routes()

	operatorReq := httptest.NewRequest(http.MethodGet, "/api/debug/logs", nil)
	operatorReq.Header.Set("Authorization", "Bearer "+operatorToken)
	if rec := doAuthMwRequest(h, operatorReq); rec.Code != http.StatusForbidden {
		t.Fatalf("operator GET /api/debug/logs: got %d, want 403", rec.Code)
	}

	adminReq := httptest.NewRequest(http.MethodGet, "/api/debug/logs", nil)
	adminReq.Header.Set("Authorization", "Bearer "+adminToken)
	if rec := doAuthMwRequest(h, adminReq); rec.Code != http.StatusOK {
		t.Fatalf("admin GET /api/debug/logs: got %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}
