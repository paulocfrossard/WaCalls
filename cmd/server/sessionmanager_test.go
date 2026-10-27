package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func newTestManager(t *testing.T) *SessionManager {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mgr_test.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	container := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := newSessionStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	return newSessionManager(ctx, container, NewBroker(), store, waLog.Noop, slog.Default(), 0)
}

func (m *SessionManager) addUnconnected(t *testing.T, name string) *Session {
	t.Helper()
	id := newSessionID()
	if err := m.store.insert(m.appCtx, id, name); err != nil {
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(m.container.NewDevice(), waLog.Noop)
	s := newSession(m, id, name, client)
	m.register(s)
	return s
}

func TestSessionManagerRegistry(t *testing.T) {
	m := newTestManager(t)

	if len(m.infos()) != 0 {
		t.Fatal("expected no sessions when empty")
	}

	a := m.addUnconnected(t, "Account A")
	b := m.addUnconnected(t, "Account B")

	infos := m.infos()
	if len(infos) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(infos))
	}
	if infos[0].Name != "Account A" || infos[1].Name != "Account B" {
		t.Fatalf("registration order not preserved: %+v", infos)
	}
	if infos[0].Paired {
		t.Fatal("unconnected session should not report paired")
	}

	if got, ok := m.Get(a.id); !ok || got != a {
		t.Fatal("Get did not return registered session")
	}

	m.unregister(b.id)
	if _, ok := m.Get(b.id); ok {
		t.Fatal("session b should be gone after unregister")
	}
	if len(m.infos()) != 1 {
		t.Fatal("expected 1 session after unregister")
	}
}

// blockOutboundWhatsApp keeps Restore's connect() from dialing
// web.whatsapp.com during tests. whatsmeow.NewClient clones
// http.DefaultTransport, so swapping it for a transport with a dead proxy
// makes the dial fail fast (connection refused) instead of opening a real
// pairing socket; Restore logs the error and continues, which is the same
// path taken when WhatsApp is unreachable in production.
func blockOutboundWhatsApp(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "127.0.0.1:1"}),
	}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// TestRestoreKeepsUnscannedSession is the F3 regression guard: a session row
// that was created but never QR-scanned (JID == "") must survive Restore as a
// fresh, non-open, pairable session instead of being deleted on every restart.
func TestRestoreKeepsUnscannedSession(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
	ctx := context.Background()

	id := newSessionID()
	if err := m.store.insert(ctx, id, "never scanned"); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id || rows[0].JID != "" {
		t.Fatalf("unscanned session row must survive Restore, got %+v", rows)
	}

	s, ok := m.Get(id)
	if !ok {
		t.Fatal("unscanned session must stay registered after Restore")
	}
	info := s.info()
	if info.State != "logged_out" {
		t.Fatalf("restored unscanned session must be logged_out, got %q", info.State)
	}
	if info.Paired != false {
		t.Fatal("restored unscanned session must report paired=false")
	}
	if s.qrActive != false {
		t.Fatal("restored unscanned session must not have an active QR channel")
	}
}

// TestRestoreUnscannedIsLoggedOut: a single unscanned session restored must end
// up in the logged_out state with no pairing QR emitted (P1-1).
func TestRestoreUnscannedIsLoggedOut(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
	ctx := context.Background()

	id := newSessionID()
	if err := m.store.insert(ctx, id, "never scanned"); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	s, ok := m.Get(id)
	if !ok {
		t.Fatal("unscanned session must stay registered after Restore")
	}
	info := s.info()
	if info.State != "logged_out" {
		t.Fatalf("state = %q, want logged_out", info.State)
	}
	if info.Paired != false {
		t.Fatal("paired must be false")
	}
	if s.qrActive != false {
		t.Fatal("qrActive must be false")
	}
	for _, ev := range m.snapshotEvents() {
		frame, ok := ev.(map[string]any)
		if ok && frame["type"] == "session-qr" {
			t.Fatalf("unexpected session-qr frame: %#v", frame)
		}
	}
	if sessionAuth(s).QR != "" {
		t.Fatalf("QR must be empty, got %q", sessionAuth(s).QR)
	}
}

// TestRestoreStillDropsBrokenRows keeps the two legitimate drop paths intact:
// an unparseable JID and a JID whose device material is gone.
func TestRestoreStillDropsBrokenRows(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	badJID := newSessionID()
	if err := m.store.insert(ctx, badJID, "bad jid"); err != nil {
		t.Fatal(err)
	}
	// Three dots in the user part: types.ParseJID rejects it.
	if err := m.store.setJID(ctx, badJID, "a.b.c@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}

	noDevice := newSessionID()
	if err := m.store.insert(ctx, noDevice, "no device"); err != nil {
		t.Fatal(err)
	}
	if err := m.store.setJID(ctx, noDevice, "15551234567@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("broken rows must still be dropped, got %+v", rows)
	}
	if _, ok := m.Get(badJID); ok {
		t.Fatal("session with unparseable jid must not be registered")
	}
	if _, ok := m.Get(noDevice); ok {
		t.Fatal("session with missing device material must not be registered")
	}
}

// TestSnapshotEventsIncludeAuthStateWithQR is the F5 guard: the SSE replay
// snapshot must carry an auth-state frame (same shape as Broker.emitAuthState)
// so a late-connecting client recovers the live pairing QR without waiting
// for the next code rotation.
func TestSnapshotEventsIncludeAuthStateWithQR(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "QR session")
	s.setAuth(AuthSnapshot{State: "qr", Paired: false, QR: "live-qr-code"})

	evs := m.snapshotEvents()
	if len(evs) < 2 {
		t.Fatalf("snapshot must carry session-list plus auth-state, got %d frames", len(evs))
	}

	first, ok := evs[0].(map[string]any)
	if !ok || first["type"] != "session-list" {
		t.Fatalf("first snapshot frame must be session-list, got %#v", evs[0])
	}
	list, ok := first["sessions"].([]SessionInfo)
	if !ok || len(list) != 1 || list[0].ID != s.id {
		t.Fatalf("session-list must contain the session, got %#v", first["sessions"])
	}

	var auth map[string]any
	for _, ev := range evs[1:] {
		frame, ok := ev.(map[string]any)
		if !ok {
			t.Fatalf("snapshot frame must be a map, got %#v", ev)
		}
		if frame["type"] == "auth-state" && frame["sessionId"] == s.id {
			auth = frame
		}
	}
	if auth == nil {
		t.Fatal("snapshot must contain an auth-state frame for a qr session")
	}
	if auth["state"] != "qr" {
		t.Fatalf("auth-state frame state = %#v, want \"qr\"", auth["state"])
	}
	if auth["paired"] != false {
		t.Fatalf("auth-state frame paired = %#v, want false", auth["paired"])
	}
	if auth["qr"] != "live-qr-code" {
		t.Fatalf("auth-state frame qr = %#v, want the live QR string", auth["qr"])
	}

	// The frame must serialize to plain JSON (the SSE writer prefixes "data: "
	// itself) with the same keys Broker.emitAuthState emits.
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatalf("marshal auth-state frame: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("auth-state frame is not valid JSON: %v", err)
	}
	for _, key := range []string{"type", "sessionId", "state", "paired", "qr"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("auth-state frame missing %q key: %s", key, raw)
		}
	}
	if decoded["type"] != "auth-state" {
		t.Fatalf("auth-state frame type = %#v", decoded["type"])
	}
}

// insertPairedSession creates a session row backed by a real device so it
// survives Restore's GetDevice lookup (ever_paired=1, JID set).
func insertPairedSession(t *testing.T, m *SessionManager, ctx context.Context) string {
	t.Helper()
	jid, err := types.ParseJID("15551234567@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	dev := m.container.NewDevice()
	dev.ID = &jid
	dev.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := dev.Save(ctx); err != nil {
		t.Fatal(err)
	}
	id := newSessionID()
	if err := m.store.insert(ctx, id, "paired"); err != nil {
		t.Fatal(err)
	}
	if err := m.store.setJID(ctx, id, jid.String()); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRestorePrunesOldUnscanned(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
	ctx := context.Background()

	ids := make([]string, 12)
	for i := 0; i < 12; i++ {
		id := newSessionID()
		ids[i] = id
		if err := m.store.insert(ctx, id, "unscanned"); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 10 {
		t.Fatalf("expected 10 sessions after prune, got %d", len(rows))
	}
	if len(m.infos()) != 10 {
		t.Fatalf("expected 10 registered sessions, got %d", len(m.infos()))
	}

	for i := 0; i < 2; i++ {
		if _, ok := m.Get(ids[i]); ok {
			t.Fatalf("oldest session %d should have been pruned", i)
		}
		for _, r := range rows {
			if r.ID == ids[i] {
				t.Fatalf("pruned session %s still in store", ids[i])
			}
		}
	}
	for i := 2; i < 12; i++ {
		found := false
		for _, r := range rows {
			if r.ID == ids[i] {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("surviving session %s missing from store", ids[i])
		}
	}

	for _, s := range m.orderedSessions() {
		if s.info().State != "logged_out" {
			t.Fatalf("restored unscanned session must be logged_out, got %q", s.info().State)
		}
	}
}

func TestRestorePrunesOnlyUnscanned(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
	ctx := context.Background()

	unscannedIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		id := newSessionID()
		unscannedIDs[i] = id
		if err := m.store.insert(ctx, id, "unscanned"); err != nil {
			t.Fatal(err)
		}
	}
	pairedID := insertPairedSession(t, m, ctx)

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 {
		t.Fatalf("expected 6 sessions after first restore, got %d", len(rows))
	}
	pairedFound := false
	for _, r := range rows {
		if r.ID == pairedID {
			pairedFound = true
		}
	}
	if !pairedFound {
		t.Fatal("paired session must survive first restore")
	}

	// Insert 8 more unscanned (total 13 unscanned).
	for i := 0; i < 8; i++ {
		id := newSessionID()
		if err := m.store.insert(ctx, id, "unscanned"); err != nil {
			t.Fatal(err)
		}
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err = m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unscannedCount := 0
	pairedCount := 0
	for _, r := range rows {
		if r.ID == pairedID {
			pairedCount++
		} else if !r.EverPaired {
			unscannedCount++
		}
	}
	if unscannedCount != 10 {
		t.Fatalf("expected 10 unscanned sessions, got %d", unscannedCount)
	}
	if pairedCount != 1 {
		t.Fatalf("expected 1 paired session, got %d", pairedCount)
	}
	if len(rows) != 11 {
		t.Fatalf("expected 11 total sessions, got %d", len(rows))
	}
}
