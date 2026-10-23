package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testWorker(t *testing.T) (*webhookStore, *webhookWorker) {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "wh_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := newWebhookStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	w := newWebhookWorker(st, testLogger(), nil)
	// Accelerated backoff so the retry test finishes in milliseconds.
	w.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	return st, w
}

func TestWebhookDeliverySuccessAndSignature(t *testing.T) {
	var mu sync.Mutex
	var lastBody []byte
	var lastSig, lastEvent, lastUA, lastCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = body
		lastSig = r.Header.Get("X-WaCalls-Signature")
		lastEvent = r.Header.Get("X-WaCalls-Event")
		lastUA = r.Header.Get("User-Agent")
		lastCT = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "topsecret", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{
		"type": "incoming", "sessionId": "sess_123", "id": "call_456",
		"peer": "553299999999", "offeredAt": 1761234567890,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := lastBody != nil
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if lastBody == nil {
		t.Fatal("webhook never delivered")
	}
	if lastEvent != "incoming" || lastUA != "WaCalls-Webhook/1.0" || lastCT != "application/json" {
		t.Fatalf("headers: event=%q ua=%q ct=%q", lastEvent, lastUA, lastCT)
	}
	mac := hmac.New(sha256.New, []byte("topsecret"))
	mac.Write(lastBody)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if lastSig != want {
		t.Fatalf("signature mismatch: got %s want %s", lastSig, want)
	}

	var env struct {
		Event     string    `json:"event"`
		Timestamp time.Time `json:"timestamp"`
		Data      struct {
			SessionID string `json:"sessionId"`
			CallID    string `json:"callId"`
			Peer      string `json:"peer"`
			Direction string `json:"direction"`
		} `json:"data"`
	}
	if err := json.Unmarshal(lastBody, &env); err != nil {
		t.Fatalf("envelope not valid JSON: %v", err)
	}
	if env.Event != "incoming" || env.Timestamp.IsZero() {
		t.Fatalf("bad envelope header: %+v", env)
	}
	if env.Data.SessionID != "sess_123" || env.Data.CallID != "call_456" ||
		env.Data.Peer != "553299999999" || env.Data.Direction != "inbound" {
		t.Fatalf("bad data contract: %+v", env.Data)
	}
}

func TestWebhookRetryThreeAttemptsThenGiveUp(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("call-ended", map[string]any{"type": "call-ended", "sessionId": "s1", "id": "c1"})

	deadline := time.Now().Add(3 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond) // ensure a 4th attempt does not fire
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", got)
	}
}

func TestWebhookIgnoresNonWebhookEvents(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("session-qr", map[string]any{"type": "session-qr"})
	w.Dispatch("call-list", map[string]any{"type": "call-list"})
	w.Dispatch("auth-state", map[string]any{"type": "auth-state"})

	time.Sleep(120 * time.Millisecond)
	if got := attempts.Load(); got != 0 {
		t.Fatalf("expected 0 deliveries for non-webhook events, got %d", got)
	}
}

func TestBrokerDispatchesWebhookEvents(t *testing.T) {
	var attempts atomic.Int32
	var mu sync.Mutex
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		mu.Lock()
		events = append(events, r.Header.Get("X-WaCalls-Event"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s",
		[]string{"incoming", "call-status", "call-ended"}); err != nil {
		t.Fatal(err)
	}
	b := NewBroker()
	b.Dispatcher = w
	w.Start(context.Background(), 2)

	b.emitIncoming("sess_1", "call_1", "551199999999")
	b.upsertCall(CallRecord{
		SessionID: "sess_1", CallID: "call_1", Direction: "inbound",
		Peer: "551199999999", StartedAt: time.Now().UnixMilli(), Status: StatusRinging,
	})
	b.endCall("call_1", "busy")

	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := attempts.Load(); got != 3 {
		t.Fatalf("expected 3 webhook deliveries (incoming+call-status+call-ended), got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]bool{"incoming": true, "call-status": true, "call-ended": true}
	for _, e := range events {
		if !want[e] {
			t.Fatalf("unexpected event delivered: %q", e)
		}
		delete(want, e)
	}
	if len(want) != 0 {
		t.Fatalf("missing deliveries: %v", want)
	}
}

func TestWebhookDispatcherNilIsSafe(t *testing.T) {
	b := NewBroker()
	// No dispatcher wired: broadcast must not panic.
	b.emitIncoming("s", "c", "p")
	b.upsertCall(CallRecord{SessionID: "s", CallID: "c", Status: StatusRinging})
	b.endCall("c", "done")
}

func TestWebhookDeliveryIDStableAcrossRetries(t *testing.T) {
	var mu sync.Mutex
	var deliveries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		deliveries = append(deliveries, r.Header.Get("X-WaCalls-Delivery-Id"))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError) // force retries
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "s1", "id": "c1", "peer": "p"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(deliveries)
		mu.Unlock()
		if n >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(deliveries))
	}
	ids := map[string]bool{}
	for _, id := range deliveries {
		if len(id) != 32 {
			t.Fatalf("delivery id should be 32 hex chars, got %q", id)
		}
		ids[id] = true
	}
	if len(ids) != 1 {
		t.Fatalf("delivery id must be stable across retries, got %d distinct: %v", len(ids), ids)
	}
}

func TestWebhookRedirectIsNotFollowed(t *testing.T) {
	var sourceHits, targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceHits.Add(1)
		http.Redirect(w, r, target.URL, http.StatusMovedPermanently)
	}))
	defer source.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), source.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "s", "id": "c"})

	deadline := time.Now().Add(3 * time.Second)
	for sourceHits.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	if got := sourceHits.Load(); got != 3 {
		t.Fatalf("expected 3 attempts against source (3xx = failed attempt), got %d", got)
	}
	if got := targetHits.Load(); got != 0 {
		t.Fatalf("redirect must not be followed, target got %d hits", got)
	}
}

func TestWebhookDrainsResponseBodyForKeepAlive(t *testing.T) {
	var mu sync.Mutex
	var remotes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		remotes = append(remotes, r.RemoteAddr)
		mu.Unlock()
		// Non-empty body so the drain path is exercised.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"detail":"some response payload to drain"}`))
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "s1", "id": "c1", "peer": "p"})
	w.Dispatch("call-status", map[string]any{"type": "call-status", "sessionId": "s1", "id": "c1", "status": "ringing"})

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(remotes)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(remotes) < 2 {
		t.Fatalf("expected 2 deliveries, got %d", len(remotes))
	}
	// With body drain, the connection is reused → same RemoteAddr (ip:port).
	// Without drain, Go opens a new connection → different port.
	if remotes[0] != remotes[1] {
		t.Fatalf("expected keep-alive reuse (same RemoteAddr), got %s vs %s", remotes[0], remotes[1])
	}
}

func TestWebhookDeadHookDoesNotBlockHealthyHook(t *testing.T) {
	// Dead hook: black-hole server (never responds) → client timeout applies.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the client gives up (client timeout is 500ms below).
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer dead.Close()

	var healthyHits atomic.Int32
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	st, w := testWorker(t)
	// Register dead hook FIRST so sequential processing would hit it first.
	if _, err := st.Create(context.Background(), dead.URL, "dead", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(context.Background(), healthy.URL, "healthy", nil); err != nil {
		t.Fatal(err)
	}
	// Force short client timeout so the dead hook fails fast.
	w.client.Timeout = 500 * time.Millisecond
	w.backoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	w.jobTimeout = 2 * time.Second

	start := time.Now()
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "s", "id": "c", "peer": "p"})

	deadline := time.Now().Add(2 * time.Second)
	for healthyHits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	elapsed := time.Since(start)

	if healthyHits.Load() < 1 {
		t.Fatal("healthy hook never received the event")
	}
	// Sequential baseline: dead hook alone would hold the worker ~500ms*3+backoffs
	// ≈ 1.5s+ before healthy gets a turn. Parallel should deliver healthy well
	// under that.
	if elapsed > 750*time.Millisecond {
		t.Fatalf("healthy hook delivery took %v; dead hook appears to be blocking (expected <750ms)", elapsed)
	}
}

func TestWebhookLookupTimeoutOnStalledDB(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "wh_stall.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Single connection so we can hold it.
	db.SetMaxOpenConns(1)
	st, err := newWebhookStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the only connection with an open transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	w := newWebhookWorker(st, testLogger(), nil)
	w.lookupTimeout = 100 * time.Millisecond

	// deliver() should time out on GetForEvent and drop the job (log error),
	// not hang forever.
	done := make(chan struct{})
	go func() {
		w.deliver(ctx, webhookJob{
			event: WebhookEvent{Event: "incoming", DeliveryID: "x"},
			body:  []byte(`{}`),
		})
		close(done)
	}()

	select {
	case <-done:
		// OK — returned after timeout.
	case <-time.After(2 * time.Second):
		t.Fatal("deliver did not return within 2s; lookup timeout not applied")
	}
}

func TestWebhookPermanent4xxNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest) // permanent
	}))
	defer srv.Close()

	st, w := testWorker(t)
	if _, err := st.Create(context.Background(), srv.URL, "s", nil); err != nil {
		t.Fatal(err)
	}
	w.Start(context.Background(), 1)
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "s", "id": "c"})

	time.Sleep(300 * time.Millisecond)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("permanent 4xx must not be retried, got %d attempts", got)
	}
}

func TestWebhookShutdownLogsPendingCount(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	_, w := testWorker(t)
	w.log = log

	// Push 2 jobs without starting workers so they stay buffered.
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "a", "id": "1"})
	w.Dispatch("incoming", map[string]any{"type": "incoming", "sessionId": "b", "id": "2"})
	if n := len(w.jobs); n != 2 {
		t.Fatalf("expected 2 buffered jobs, got %d", n)
	}

	w.logPendingOnShutdown()
	// Second call must be a no-op (sync.Once).
	w.logPendingOnShutdown()

	if got := buf.String(); !strings.Contains(got, "pending=2") {
		t.Fatalf("expected pending=2 in log, got: %s", got)
	}
}
