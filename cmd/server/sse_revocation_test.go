package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func sseTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newSSETestServer builds a real server via newServer over a throwaway SQLite
// DB. keepalive, when >0, overrides the broker's keepalive interval.
func newSSETestServer(t *testing.T, keepalive time.Duration) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	dbPath := filepath.Join(t.TempDir(), "sse_revocation.db")
	s, err := newServer(ctx, serverOptions{
		DBPath:       dbPath,
		AdminUser:    "admin",
		AdminPass:    "pw-admin",
		SessionTTL:   time.Hour,
		CookieSecure: false,
	}, sseTestLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	t.Cleanup(func() {
		s.audit.Close()
		_ = s.auth.db.Close()
	})
	if keepalive > 0 {
		s.broker.keepaliveInterval = keepalive
	}
	return s
}

// seedSSEUser inserts a user and returns its id.
func seedSSEUser(t *testing.T, s *server, id, username string) {
	t.Helper()
	created, err := s.auth.insertUserIfAbsent(context.Background(), id, username, []byte("pw"), "admin", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	if !created {
		t.Fatalf("user %q not created", username)
	}
}

// seedSSESession inserts a session and returns its id and raw token.
func seedSSESession(t *testing.T, s *server, userID, rawToken string) string {
	t.Helper()
	id := newSessionID()
	now := time.Now()
	if err := s.auth.insertSession(context.Background(), id, userID, sha256Hex(rawToken), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

// seedSSEApiKey inserts an API key and returns its id and raw key.
func seedSSEApiKey(t *testing.T, s *server, userID, keyID, rawKey string) {
	t.Helper()
	if err := s.auth.insertApiKey(context.Background(), keyID, userID, "test-key", sha256Hex(rawKey), rawKey[:apiKeyPrefixLen], time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
}

// mintSSETicket exchanges a session cookie for a one-time SSE ticket and
// returns the raw ticket value.
func mintSSETicket(t *testing.T, ts *httptest.Server, rawToken string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/sse-ticket", nil)
	if err != nil {
		t.Fatalf("new sse ticket request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse ticket: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("sse ticket status=%d body=%s", resp.StatusCode, string(body))
	}
	var tb struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tb); err != nil {
		t.Fatalf("decode ticket: %v", err)
	}
	if tb.Ticket == "" {
		t.Fatal("empty ticket")
	}
	return tb.Ticket
}

// eventsTicketStatus issues GET /api/events?ticket=... and returns the status
// code without expecting a successful stream. The body is drained so a 401
// response does not leak a connection.
func eventsTicketStatus(t *testing.T, ts *httptest.Server, ticket string) int {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/events?ticket=" + ticket)
	if err != nil {
		t.Fatalf("events ticket request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// openSSE opens an SSE stream against ts and returns the response plus a
// channel that closes when the body reaches EOF/error.
func openSSE(t *testing.T, ts *httptest.Server, mutate func(*http.Request)) (*http.Response, <-chan struct{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("open sse status=%d body=%s", resp.StatusCode, string(body))
	}
	closed := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				close(closed)
				return
			}
		}
	}()
	return resp, closed
}

func awaitClose(t *testing.T, closed <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatalf("%s: stream did not close within 1s", label)
	}
}

func TestSSELogoutClosesStream(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawToken = "sse-session-token"
	sessionID := seedSSESession(t, s, "sse-user", rawToken)
	_ = sessionID

	resp, closed := openSSE(t, ts, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	})
	defer resp.Body.Close()

	// Revoke via the public logout endpoint.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	lr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d", lr.StatusCode)
	}

	awaitClose(t, closed, "logout")
}

func TestSSEApiKeyDeleteClosesStream(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawKey = "wc_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	const keyID = "sse-key-id"
	seedSSEApiKey(t, s, "sse-user", keyID, rawKey)

	// Admin session for the delete call.
	seedSSEUser(t, s, "admin2", "admin2")
	const adminToken = "sse-admin-token"
	seedSSESession(t, s, "admin2", adminToken)

	resp, closed := openSSE(t, ts, func(r *http.Request) {
		r.Header.Set("X-API-Key", rawKey)
	})
	defer resp.Body.Close()

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/auth/keys/"+keyID, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	dr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete key: %v", err)
	}
	dr.Body.Close()
	if dr.StatusCode != http.StatusNoContent {
		t.Fatalf("delete key status=%d", dr.StatusCode)
	}

	awaitClose(t, closed, "api key delete")
}

func TestSSEKeepaliveRevalidationClosesRevokedSession(t *testing.T) {
	s := newSSETestServer(t, 50*time.Millisecond)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawToken = "sse-session-token-ka"
	sessionID := seedSSESession(t, s, "sse-user", rawToken)

	resp, closed := openSSE(t, ts, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	})
	defer resp.Body.Close()

	// Revoke directly in the store (no broker.Revoke) so only the keepalive
	// revalidation can close the stream.
	if err := s.auth.revokeSession(context.Background(), sessionID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	awaitClose(t, closed, "keepalive revalidation")
}

func TestSSETicketRevocableViaOriginSession(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawToken = "sse-origin-token"
	sessionID := seedSSESession(t, s, "sse-user", rawToken)
	_ = sessionID

	// Mint a ticket from the origin session.
	ticket := mintSSETicket(t, ts, rawToken)

	resp, closed := openSSE(t, ts, func(r *http.Request) {
		r.URL.RawQuery = "ticket=" + ticket
	})
	defer resp.Body.Close()

	// Revoke the origin session via logout.
	lreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/logout", nil)
	lreq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	lr, err := http.DefaultClient.Do(lreq)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	lr.Body.Close()

	awaitClose(t, closed, "ticket revocation")
}

// TestSSETransientRevalidationClosesStreamAfterCap pins maxTransientKeepalives:
// a revalidation that keeps reporting authTransient must not keep a stream
// alive forever. The stream is expected to survive at least two inconclusive
// ticks and then close on the third, within the test's 1s budget.
func TestSSETransientRevalidationClosesStreamAfterCap(t *testing.T) {
	const keepalive = 40 * time.Millisecond
	s := newSSETestServer(t, keepalive)
	// Simulate a wedged auth store: every revalidation is inconclusive. The
	// field is overridden before the server starts serving, so there is no
	// concurrent read of the original closure.
	s.broker.authCheck = func(sessionID, keyID string) authVerdict {
		return authTransient
	}
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawToken = "sse-session-token-transient"
	seedSSESession(t, s, "sse-user", rawToken)

	start := time.Now()
	resp, closed := openSSE(t, ts, func(r *http.Request) {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rawToken})
	})
	defer resp.Body.Close()

	awaitClose(t, closed, "transient revalidation cap")

	// A stream that closed on the first tick would still pass awaitClose but
	// would mean transients were not tolerated at all. Require it to have
	// survived at least two ticks before the cap closed it.
	if elapsed := time.Since(start); elapsed < 2*keepalive {
		t.Fatalf("stream closed after %v, want at least %v (tolerate transients before cap)", elapsed, 2*keepalive)
	}
}

// TestSSETicketRejectedAfterSessionRevoked covers the connect-path revalidation
// of a ticket whose underlying session was revoked after minting. The ticket
// must not open a stream, and because a rejected consume leaves the ticket in
// place (single-use is only spent on success), replaying it must fail too.
func TestSSETicketRejectedAfterSessionRevoked(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	seedSSEUser(t, s, "sse-user", "sseuser")
	const rawToken = "sse-ticket-revoked-token"
	sessionID := seedSSESession(t, s, "sse-user", rawToken)

	ticket := mintSSETicket(t, ts, rawToken)

	// Revoke the origin session directly in the store, after the ticket was
	// minted, so only the ticket's connect-path revalidation can reject it.
	if err := s.auth.revokeSession(context.Background(), sessionID); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	if got := eventsTicketStatus(t, ts, ticket); got != http.StatusUnauthorized {
		t.Fatalf("ticket for revoked session: got %d, want 401", got)
	}
	if got := eventsTicketStatus(t, ts, ticket); got != http.StatusUnauthorized {
		t.Fatalf("replayed ticket for revoked session: got %d, want 401", got)
	}
}
