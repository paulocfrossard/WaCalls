package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Call-visibility tests for canSeeCall and its two call sites: the session
// listing endpoint (GET /api/sessions/{sid}/history) and the SSE call-list
// (initial snapshot written on connect + broadcast to existing subscribers).
//
// They reuse the existing harnesses instead of building a second one: the
// real-server builder from sse_revocation_test.go (newSSETestServer →
// newServer → real authStore + broker + session manager) and the user/session
// seeding helpers from authapi_test.go.

// visibilitySID is the session registered for the history tests.
const visibilitySID = "sess-visibility"

// seedVisibilityUser inserts a user with a live session token and returns the
// raw bearer token for it.
func seedVisibilityUser(t *testing.T, s *server, userID, username, role string) string {
	t.Helper()
	seedAuthAPIUser(t, s.auth, userID, username, "pw-"+username, role)
	token := "visibility-token-" + userID
	seedAuthAPISession(t, s.auth, userID, token, time.Hour)
	return token
}

// seedVisibilitySession registers a session so the history handler's
// sessionByID resolves it; handleHistory itself only needs sess.id.
func seedVisibilitySession(t *testing.T, s *server, sid string) {
	t.Helper()
	s.sessions.register(&Session{id: sid})
}

// seedActiveCall records a still-active call for sid under the given conflict
// bucket; active calls are what the SSE call-list carries.
func seedActiveCall(t *testing.T, s *server, sid, callID, ownerKey string, owner *string) {
	t.Helper()
	s.broker.upsertCall(CallRecord{
		SessionID: sid,
		CallID:    callID,
		Owner:     owner,
		ownerKey:  ownerKey,
		Direction: "outbound",
		Peer:      "551199999999",
		StartedAt: time.Now().UnixMilli(),
		Status:    StatusConnected,
	})
}

// seedEndedCall is seedActiveCall plus endCall, so the row lands in the broker
// history that /history serves. owner (the UI-facing client id) may be nil.
func seedEndedCall(t *testing.T, s *server, sid, callID, ownerKey string, owner *string) {
	t.Helper()
	seedActiveCall(t, s, sid, callID, ownerKey, owner)
	s.broker.endCall(callID, "normal")
}

// fetchHistory GETs /api/sessions/{sid}/history as bearer ("" = no credential
// at all) and returns the decoded rows plus the raw response body.
func fetchHistory(t *testing.T, h http.Handler, bearer, sid string) ([]CallRecord, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sid+"/history", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET history (bearer=%q): got %d, want 200 (body=%s)", bearer, rec.Code, rec.Body.String())
	}
	var out struct {
		Rows []CallRecord `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode history body %q: %v", rec.Body.String(), err)
	}
	return out.Rows, rec.Body.String()
}

// assertVisibleCallIDs asserts that rows carry exactly the given call ids,
// order-independent: anything extra means a visibility leak, anything missing
// means an over-filter.
func assertVisibleCallIDs(t *testing.T, rows []CallRecord, want ...string) {
	t.Helper()
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.CallID)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("visible call ids = %v, want %v", got, want)
	}
}

// TestCanSeeCallMatrix pins the closed decision table: auth-disabled and
// admin principals see every bucket, everyone else sees only "user:<userID>"
// buckets — which leaves orphans ("client:*") and empty owner keys admin-only
// without special-casing.
func TestCanSeeCallMatrix(t *testing.T) {
	cases := []struct {
		name         string
		role         string
		userID       string
		authDisabled bool
		ownerKey     string
		want         bool
	}{
		// admin: independent of the owner bucket.
		{"admin sees own user call", "admin", "user-A", false, "user:user-A", true},
		{"admin sees another user's call", "admin", "user-A", false, "user:user-B", true},
		{"admin sees orphan call", "admin", "user-A", false, "client:x", true},
		{"admin sees call without owner key", "admin", "user-A", false, "", true},
		{"admin sees orphan with empty userID", "admin", "", false, "client:x", true},

		// operator: own bucket only.
		{"operator sees own call", "operator", "user-A", false, "user:user-A", true},
		{"operator does not see another user's call", "operator", "user-A", false, "user:user-B", false},
		{"operator does not see orphan call", "operator", "user-A", false, "client:x", false},
		{"operator does not see call without owner key", "operator", "user-A", false, "", false},

		// empty role is not admin, so it follows rule 3.
		{"empty role sees own call", "", "user-A", false, "user:user-A", true},
		{"empty role does not see another user's call", "", "user-A", false, "user:user-B", false},
		{"empty role does not see orphan call", "", "user-A", false, "client:x", false},
		{"empty role and empty userID see nothing", "", "", false, "", false},
		{"empty role and empty userID never see user calls", "", "", false, "user:user-A", false},
		{"empty userID matches only the empty user bucket", "", "", false, "user:", true},

		// authDisabled: no principals to separate.
		{"auth disabled operator sees orphan call", "operator", "user-A", true, "client:x", true},
		{"auth disabled operator sees another user's call", "operator", "user-A", true, "user:user-B", true},
		{"auth disabled sees call without owner key", "", "", true, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := canSeeCall(tc.role, tc.userID, tc.authDisabled, tc.ownerKey)
			if got != tc.want {
				t.Fatalf("canSeeCall(%q, %q, %v, %q) = %v, want %v",
					tc.role, tc.userID, tc.authDisabled, tc.ownerKey, got, tc.want)
			}
		})
	}
}

// TestHistoryOperatorSeesOnlyOwnCalls: the session listing returned to
// operator A contains exactly A's ended call, never B's.
func TestHistoryOperatorSeesOnlyOwnCalls(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-a", "user:user-A", ownerPtr("tab-a"))
	seedEndedCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))

	rows, _ := fetchHistory(t, s.routes(), tokenA, visibilitySID)
	assertVisibleCallIDs(t, rows, "call-a")
}

// TestHistoryAdminSeesAllIncludingOrphans: an admin's session listing carries
// every row — both users' calls and the anonymous "client:*" orphan.
func TestHistoryAdminSeesAllIncludingOrphans(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenAdmin := seedVisibilityUser(t, s, "user-admin", "root", "admin")
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-a", "user:user-A", ownerPtr("tab-a"))
	seedEndedCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))
	seedEndedCall(t, s, visibilitySID, "call-orphan", "client:x", ownerPtr("tab-x"))

	rows, _ := fetchHistory(t, s.routes(), tokenAdmin, visibilitySID)
	assertVisibleCallIDs(t, rows, "call-a", "call-b", "call-orphan")
}

// TestHistoryOperatorHidesOrphanCalls: an orphan bucket ("client:x") is
// admin-only — a non-admin operator's listing keeps its own row and drops it.
func TestHistoryOperatorHidesOrphanCalls(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-a", "user:user-A", ownerPtr("tab-a"))
	seedEndedCall(t, s, visibilitySID, "call-orphan", "client:x", ownerPtr("tab-x"))

	rows, body := fetchHistory(t, s.routes(), tokenA, visibilitySID)
	assertVisibleCallIDs(t, rows, "call-a")
	if strings.Contains(body, "call-orphan") {
		t.Fatalf("operator session listing leaks the orphan call: %s", body)
	}
}

// TestHistoryOperatorHidesOtherOperatorsCalls: a listing whose rows all belong
// to operator B comes back empty for operator A.
func TestHistoryOperatorHidesOtherOperatorsCalls(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))

	rows, _ := fetchHistory(t, s.routes(), tokenA, visibilitySID)
	assertVisibleCallIDs(t, rows)
}

// TestHistoryAuthDisabledReturnsFullList covers the -auth-disabled surface:
// with the flag on, a request carrying no credential at all gets the full
// listing. The unauthenticated 401 first pins the enabled-auth baseline for
// the very same server and request.
func TestHistoryAuthDisabledReturnsFullList(t *testing.T) {
	s := newSSETestServer(t, 0)
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-a", "user:user-A", ownerPtr("tab-a"))
	seedEndedCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))
	seedEndedCall(t, s, visibilitySID, "call-orphan", "client:x", ownerPtr("tab-x"))
	h := s.routes()

	// Baseline: authentication enabled, no credential → rejected upstream.
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+visibilitySID+"/history", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("credential-less history with auth enabled: got %d, want 401 (body=%s)", rec.Code, rec.Body.String())
	}

	// -auth-disabled sets exactly this field (serverOptions.AuthDisabled);
	// no principal is presented, so the full list must come back.
	s.authDisabled = true
	rows, _ := fetchHistory(t, h, "", visibilitySID)
	assertVisibleCallIDs(t, rows, "call-a", "call-b", "call-orphan")
}

// TestHistorySessionListingScopedBySessionAndOwner: the session listing IS
// GET /api/sessions/{sid}/history, so a row must survive only when it belongs
// to both the requested session and the requesting principal.
func TestHistorySessionListingScopedBySessionAndOwner(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	tokenAdmin := seedVisibilityUser(t, s, "user-admin", "root", "admin")
	seedVisibilitySession(t, s, "sess-1")
	seedVisibilitySession(t, s, "sess-2")
	seedEndedCall(t, s, "sess-1", "call-a1", "user:user-A", ownerPtr("tab-a1"))
	seedEndedCall(t, s, "sess-1", "call-b1", "user:user-B", ownerPtr("tab-b1"))
	seedEndedCall(t, s, "sess-2", "call-b2", "user:user-B", ownerPtr("tab-b2"))
	h := s.routes()

	rowsA1, _ := fetchHistory(t, h, tokenA, "sess-1")
	assertVisibleCallIDs(t, rowsA1, "call-a1")

	// sess-2 holds only operator B's call: another session must not leak it
	// into A's listing.
	rowsA2, _ := fetchHistory(t, h, tokenA, "sess-2")
	assertVisibleCallIDs(t, rowsA2)

	rowsAdmin2, _ := fetchHistory(t, h, tokenAdmin, "sess-2")
	assertVisibleCallIDs(t, rowsAdmin2, "call-b2")
}

// TestHistoryKeepsOwnerFieldIntact guards the UI contract: filtering happens
// on ownerKey, and the serialized Owner (raw client id) survives untouched
// for both a set pointer and a nil one.
func TestHistoryKeepsOwnerFieldIntact(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	seedVisibilitySession(t, s, visibilitySID)
	seedEndedCall(t, s, visibilitySID, "call-owner-set", "user:user-A", ownerPtr("tab-raw"))
	seedEndedCall(t, s, visibilitySID, "call-owner-nil", "user:user-A", nil)

	rows, body := fetchHistory(t, s.routes(), tokenA, visibilitySID)
	assertVisibleCallIDs(t, rows, "call-owner-nil", "call-owner-set")

	seenSet, seenNil := false, false
	for _, r := range rows {
		switch r.CallID {
		case "call-owner-set":
			seenSet = true
			if r.Owner == nil || *r.Owner != "tab-raw" {
				t.Fatalf("Owner of call-owner-set = %v, want %q", r.Owner, "tab-raw")
			}
		case "call-owner-nil":
			seenNil = true
			if r.Owner != nil {
				t.Fatalf("Owner of call-owner-nil = %v, want nil", *r.Owner)
			}
		}
	}
	if !seenSet || !seenNil {
		t.Fatalf("missing rows in history response: %s", body)
	}
	// A nil owner must still be present in the payload as an explicit null,
	// not silently dropped by the filtering pass.
	if !strings.Contains(body, `"owner":null`) {
		t.Fatalf("history body drops the null owner key: %s", body)
	}
}

// TestHistoryPredicateAppliedBeforeCap pins that the visibility predicate runs
// BEFORE the 50-row cap in historyRows: a failed row consumes no cap slot, so
// the listing returns up to 50 *visible* rows. Seeded 55 rows for one session:
// 52 owned by operator A plus 3 orphans ("client:x", newest). A regression to
// post-limit filtering would take the newest 50 (47 A + 3 orphans), filter the
// orphans out, and return 47 — this asserts exactly 50 rows, all A's.
func TestHistoryPredicateAppliedBeforeCap(t *testing.T) {
	s := newSSETestServer(t, 0)
	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	seedVisibilitySession(t, s, visibilitySID)
	const aCalls, orphanCalls = 52, 3
	wantIDs := make([]string, 0, aCalls)
	for i := range aCalls {
		id := "call-a-" + strconv.Itoa(i)
		seedEndedCall(t, s, visibilitySID, id, "user:user-A", ownerPtr("tab-a"))
		wantIDs = append(wantIDs, id)
	}
	// Orphans are seeded last so they land in the newest rows — the exact
	// window a post-cap filter would consume before dropping them.
	for i := range orphanCalls {
		seedEndedCall(t, s, visibilitySID, "call-orphan-"+strconv.Itoa(i), "client:x", ownerPtr("tab-x"))
	}

	rows, body := fetchHistory(t, s.routes(), tokenA, visibilitySID)
	if len(rows) != 50 {
		t.Fatalf("history rows = %d, want exactly the cap of 50 (predicate must run before the cap); body=%s", len(rows), body)
	}
	// Every returned row must be one of A's calls: an orphan in the payload
	// means the predicate ran after the cap and dropped the wrong slots.
	for _, r := range rows {
		if !slices.Contains(wantIDs, r.CallID) {
			t.Fatalf("row %q is not operator A's call (owner=%v): %s", r.CallID, r.Owner, body)
		}
		if r.Owner == nil || *r.Owner != "tab-a" {
			t.Fatalf("row %q owner = %v, want %q", r.CallID, r.Owner, "tab-a")
		}
	}
}

// openVisibilitySSE opens GET /api/events authenticated with a bearer token
// and returns a channel of decoded SSE data frames plus a stop function that
// closes the stream.
func openVisibilitySSE(t *testing.T, ts *httptest.Server, bearer string) (<-chan map[string]json.RawMessage, func()) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("new sse request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("open sse status=%d body=%s", resp.StatusCode, string(body))
	}
	frames := make(chan map[string]json.RawMessage, 64)
	go func() {
		defer close(frames)
		scanner := bufio.NewScanner(resp.Body)
		var pending string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				pending = strings.TrimPrefix(line, "data: ")
				continue
			}
			if line != "" || pending == "" {
				continue
			}
			var m map[string]json.RawMessage
			if json.Unmarshal([]byte(pending), &m) == nil {
				frames <- m
			}
			pending = ""
		}
	}()
	return frames, func() { resp.Body.Close() }
}

// awaitCallList returns the calls carried by the next call-list frame,
// skipping snapshot/keepalive frames of any other type.
func awaitCallList(t *testing.T, frames <-chan map[string]json.RawMessage) []CallRecord {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m, ok := <-frames:
			if !ok {
				t.Fatal("sse stream closed before a call-list frame arrived")
			}
			var typ string
			_ = json.Unmarshal(m["type"], &typ)
			if typ != "call-list" {
				continue
			}
			var calls []CallRecord
			if err := json.Unmarshal(m["calls"], &calls); err != nil {
				t.Fatalf("decode call-list frame: %v", err)
			}
			return calls
		case <-deadline:
			t.Fatal("timed out waiting for a call-list frame")
		}
	}
}

// TestSSEInitialCallListFilteredByPrincipal: the call-list written to a
// connecting subscriber is scoped to that subscriber — operator A gets only
// its own active call, the admin gets all of them including the orphan.
func TestSSEInitialCallListFilteredByPrincipal(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	tokenAdmin := seedVisibilityUser(t, s, "user-admin", "root", "admin")
	seedActiveCall(t, s, visibilitySID, "call-a", "user:user-A", ownerPtr("tab-a"))
	seedActiveCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))
	seedActiveCall(t, s, visibilitySID, "call-orphan", "client:x", ownerPtr("tab-x"))

	framesA, stopA := openVisibilitySSE(t, ts, tokenA)
	defer stopA()
	framesAdmin, stopAdmin := openVisibilitySSE(t, ts, tokenAdmin)
	defer stopAdmin()

	assertVisibleCallIDs(t, awaitCallList(t, framesA), "call-a")
	assertVisibleCallIDs(t, awaitCallList(t, framesAdmin), "call-a", "call-b", "call-orphan")
}

// TestSSEBroadcastCallListFilteredPerSubscriber: when a call starts, the
// broadcast snapshot is filtered per subscriber — the owning operator receives
// the new call, the other one receives an empty list instead of a leak.
func TestSSEBroadcastCallListFilteredPerSubscriber(t *testing.T) {
	s := newSSETestServer(t, 0)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()

	tokenA := seedVisibilityUser(t, s, "user-A", "opa", "operator")
	tokenB := seedVisibilityUser(t, s, "user-B", "opb", "operator")

	framesA, stopA := openVisibilitySSE(t, ts, tokenA)
	defer stopA()
	framesB, stopB := openVisibilitySSE(t, ts, tokenB)
	defer stopB()

	// Both streams start from an empty active-call list.
	assertVisibleCallIDs(t, awaitCallList(t, framesA))
	assertVisibleCallIDs(t, awaitCallList(t, framesB))

	seedActiveCall(t, s, visibilitySID, "call-b", "user:user-B", ownerPtr("tab-b"))

	assertVisibleCallIDs(t, awaitCallList(t, framesA))
	assertVisibleCallIDs(t, awaitCallList(t, framesB), "call-b")
}
