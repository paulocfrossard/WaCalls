package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServerWithWebhooks(t *testing.T, allowHosts []string) (*server, *webhookStore) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "wh_api_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := newWebhookStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{
		webhooks:          st,
		log:               testLogger(),
		webhookAllowHosts: allowHosts,
	}
	return s, st
}

func postJSON(t *testing.T, h http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookCreateValidation(t *testing.T) {
	s, _ := testServerWithWebhooks(t, nil)
	h := s.routes()

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"valid", map[string]any{"url": "https://example.com/hook", "secret": "s"}, 201},
		{"empty url", map[string]any{"url": "", "secret": "s"}, 400},
		{"httpx scheme", map[string]any{"url": "httpx://example.com", "secret": "s"}, 400},
		{"no host", map[string]any{"url": "http://", "secret": "s"}, 400},
		{"ftp scheme", map[string]any{"url": "ftp://example.com", "secret": "s"}, 400},
		{"missing secret", map[string]any{"url": "https://example.com"}, 400},
		{"unknown event", map[string]any{"url": "https://e.com", "secret": "s", "events": []string{"incomming"}}, 400},
		{"empty event", map[string]any{"url": "https://e.com", "secret": "s", "events": []string{""}}, 400},
		{"event with surrounding space is trimmed then accepted", map[string]any{"url": "https://e.com", "secret": "s", "events": []string{" incoming "}}, 201},
		{"event with internal typo", map[string]any{"url": "https://e.com", "secret": "s", "events": []string{"incom ing"}}, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, h, "/api/webhooks", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("got %d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestWebhookCreateNormalizesEventsAndURL(t *testing.T) {
	s, st := testServerWithWebhooks(t, nil)
	h := s.routes()

	rec := postJSON(t, h, "/api/webhooks", map[string]any{
		"url":    "  https://example.com/hook  ",
		"secret": " s3cret ",
		"events": []string{" incoming ", "call-ended"},
	})
	if rec.Code != 201 {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	hooks, err := st.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	if hooks[0].URL != "https://example.com/hook" {
		t.Fatalf("url not trimmed: %q", hooks[0].URL)
	}
	if hooks[0].Secret != "s3cret" {
		t.Fatalf("secret not trimmed: %q", hooks[0].Secret)
	}
	if len(hooks[0].Events) != 2 || hooks[0].Events[0] != "incoming" || hooks[0].Events[1] != "call-ended" {
		t.Fatalf("events not normalized: %+v", hooks[0].Events)
	}
}

func TestWebhookListOmitsSecret(t *testing.T) {
	s, st := testServerWithWebhooks(t, nil)
	if _, err := st.Create(context.Background(), "https://example.com/hook", "topsecret", nil); err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	req := httptest.NewRequest(http.MethodGet, "/api/webhooks", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Webhooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(resp.Webhooks))
	}
	if _, present := resp.Webhooks[0]["secret"]; present {
		t.Fatal("secret must not be returned by GET /api/webhooks")
	}
	if resp.Webhooks[0]["url"] != "https://example.com/hook" {
		t.Fatalf("unexpected url: %v", resp.Webhooks[0]["url"])
	}
}

func TestWebhookDeleteStatusCodes(t *testing.T) {
	s, st := testServerWithWebhooks(t, nil)
	h := s.routes()

	// Delete unknown → 404.
	req := httptest.NewRequest(http.MethodDelete, "/api/webhooks/nonexistent", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("expected 404 for unknown delete, got %d", rec.Code)
	}

	// Create then delete → 204, then re-delete → 404.
	id, err := st.Create(context.Background(), "https://example.com/hook", "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+id, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/webhooks/"+id, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("expected 404 on re-delete, got %d", rec.Code)
	}
}

func TestWebhookDeleteDBErrorReturns500(t *testing.T) {
	s, _ := testServerWithWebhooks(t, nil)
	h := s.routes()
	// Close the underlying DB to force a real error (not ErrNoRows).
	// testServerWithWebhooks registers cleanup to close it; close now.
	// We need access to the DB — reopen a fresh store pointing at a closed handle.
	// Simpler: call handler after closing via the store's db field.
	s.webhooks.db.Close()
	req := httptest.NewRequest(http.MethodDelete, "/api/webhooks/anyid", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("expected 500 on DB error, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("expected error body, got %s", rec.Body.String())
	}
}

func TestWebhookCreateAllowlist(t *testing.T) {
	s, _ := testServerWithWebhooks(t, []string{"chatwoot.local"})
	h := s.routes()

	// Allowed host → 201.
	rec := postJSON(t, h, "/api/webhooks", map[string]any{
		"url": "http://chatwoot.local:3000/hook", "secret": "s",
	})
	if rec.Code != 201 {
		t.Fatalf("allowed host: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Foreign host → 400.
	rec = postJSON(t, h, "/api/webhooks", map[string]any{
		"url": "http://evil.example.com/hook", "secret": "s",
	})
	if rec.Code != 400 {
		t.Fatalf("foreign host: expected 400, got %d", rec.Code)
	}

	// Case-insensitive match.
	rec = postJSON(t, h, "/api/webhooks", map[string]any{
		"url": "http://Chatwoot.LOCAL/hook", "secret": "s",
	})
	if rec.Code != 201 {
		t.Fatalf("case-insensitive: expected 201, got %d", rec.Code)
	}

	// Subdomain is NOT the same host.
	rec = postJSON(t, h, "/api/webhooks", map[string]any{
		"url": "http://sub.chatwoot.local/hook", "secret": "s",
	})
	if rec.Code != 400 {
		t.Fatalf("subdomain: expected 400, got %d", rec.Code)
	}
}

func TestWebhookEventsSubsetFilterEndToEnd(t *testing.T) {
	// Two receivers: A subscribes only to "incoming"; B is catch-all.
	var mu struct {
		sync.Mutex
		a []string
		b []string
	}
	got := make(chan string, 16)
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		mu.a = append(mu.a, r.Header.Get("X-WaCalls-Event"))
		mu.Unlock()
		got <- "a:" + r.Header.Get("X-WaCalls-Event")
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		mu.b = append(mu.b, r.Header.Get("X-WaCalls-Event"))
		mu.Unlock()
		got <- "b:" + r.Header.Get("X-WaCalls-Event")
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	_, st := testServerWithWebhooks(t, nil)
	if _, err := st.Create(context.Background(), srvA.URL, "sa", []string{"incoming"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(context.Background(), srvB.URL, "sb", nil); err != nil {
		t.Fatal(err)
	}

	// Wire a real worker + broker and drive the three eligible broadcasts.
	w := newWebhookWorker(st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	w.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	b := NewBroker()
	b.Dispatcher = w
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.Start(ctx, 2)

	b.emitIncoming("sess_1", "call_1", "551199999999")
	b.upsertCall(CallRecord{
		SessionID: "sess_1", CallID: "call_1", Direction: "inbound",
		Peer: "551199999999", StartedAt: time.Now().UnixMilli(), Status: StatusRinging,
	})
	b.endCall("call_1", "busy")

	// Expect exactly: A gets 1 (incoming only); B gets 3 (all).
	deadline := time.After(3 * time.Second)
	counts := map[string]int{}
	for counts["a:incoming"] < 1 || counts["b:incoming"] < 1 || counts["b:call-status"] < 1 || counts["b:call-ended"] < 1 {
		select {
		case ev := <-got:
			counts[ev]++
		case <-deadline:
			t.Fatalf("timed out waiting for deliveries, counts=%v", counts)
		}
	}
	// Give a short grace period for any extra deliveries.
	time.Sleep(200 * time.Millisecond)
	if counts["a:call-status"] != 0 || counts["a:call-ended"] != 0 {
		t.Fatalf("hook A (incoming-only) must not receive call-status/call-ended, got %v", counts)
	}
	if counts["b:incoming"] != 1 || counts["b:call-status"] != 1 || counts["b:call-ended"] != 1 {
		t.Fatalf("hook B (catch-all) must receive exactly 3, got %v", counts)
	}
}

func TestCallEndedWebhookCarriesPeerAndDirection(t *testing.T) {
	var mu struct {
		sync.Mutex
		body []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		mu.body = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	_, st := testServerWithWebhooks(t, nil)
	if _, err := st.Create(context.Background(), srv.URL, "s", []string{"call-ended"}); err != nil {
		t.Fatal(err)
	}
	w := newWebhookWorker(st, testLogger(), nil)
	w.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	b := NewBroker()
	b.Dispatcher = w
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.Start(ctx, 1)

	owner := "agent1"
	b.upsertCall(CallRecord{
		SessionID: "s1", CallID: "c1", Owner: &owner, Direction: "inbound",
		Peer: "551188887777", StartedAt: time.Now().UnixMilli(), Status: StatusConnected,
	})
	b.endCall("c1", "user-hangup")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := mu.body != nil
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if mu.body == nil {
		t.Fatal("call-ended webhook never delivered")
	}
	var env struct {
		Data struct {
			Peer      string `json:"peer"`
			Direction string `json:"direction"`
			EndReason string `json:"endReason"`
		} `json:"data"`
	}
	if err := json.Unmarshal(mu.body, &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Peer != "551188887777" {
		t.Fatalf("peer missing/incorrect: %q", env.Data.Peer)
	}
	if env.Data.Direction != "inbound" {
		t.Fatalf("direction missing/incorrect: %q", env.Data.Direction)
	}
	if env.Data.EndReason != "user-hangup" {
		t.Fatalf("endReason incorrect: %q", env.Data.EndReason)
	}
}
