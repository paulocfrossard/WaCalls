package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

// addCredentialedSession registers a session whose in-memory client holds a
// device ID (Store.ID != nil, i.e. "has credentials") and whose store row is
// already flagged ever_paired with a persisted JID. The caller sets the auth
// state that Pair()/handleEvent must react to.
func addCredentialedSession(t *testing.T, m *SessionManager, name string) *Session {
	t.Helper()
	jid, err := types.ParseJID("15557654321@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	device := m.container.NewDevice()
	device.ID = &jid
	client := whatsmeow.NewClient(device, waLog.Noop)
	id := newSessionID()
	if err := m.store.insert(m.appCtx, id, name); err != nil {
		t.Fatal(err)
	}
	if err := m.store.setJID(m.appCtx, id, jid.String()); err != nil {
		t.Fatal(err)
	}
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

// TestRestoreStillDropsBrokenRows keeps the legitimate drop path intact: an
// unparseable JID is corrupt state, not a recoverable logout, so the row is
// still deleted. A missing device row is NOT corrupt state (WhatsApp deletes
// devices on a 401 device_removed), so that row survives as logged_out.
func TestRestoreStillDropsBrokenRows(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
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
	if len(rows) != 1 || rows[0].ID != noDevice || rows[0].JID != "" {
		t.Fatalf("only the missing-device row must survive (JID cleared), got %+v", rows)
	}
	if _, ok := m.Get(badJID); ok {
		t.Fatal("session with unparseable jid must not be registered")
	}
	if _, ok := m.Get(noDevice); !ok {
		t.Fatal("session with missing device material must survive as logged_out")
	}
}

// TestRestoreMissingDeviceBecomesLoggedOut is the recovery guard: a session
// whose device row was deleted (external/session logout) must come back as a
// pairable logged_out session with no stale JID instead of vanishing.
func TestRestoreMissingDeviceBecomesLoggedOut(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)
	ctx := context.Background()

	id := newSessionID()
	if err := m.store.insert(ctx, id, "device removed"); err != nil {
		t.Fatal(err)
	}
	if err := m.store.setJID(ctx, id, "15559876543@s.whatsapp.net"); err != nil {
		t.Fatal(err)
	}

	if err := m.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	rows, err := m.store.list(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("session row must survive Restore, got %+v", rows)
	}
	if rows[0].JID != "" {
		t.Fatalf("stale JID must be cleared, got %q", rows[0].JID)
	}

	s, ok := m.Get(id)
	if !ok {
		t.Fatal("session must stay registered after Restore")
	}
	found := false
	for _, info := range m.infos() {
		if info.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("sessions list must contain the restored session")
	}
	info := s.info()
	if info.State != "logged_out" {
		t.Fatalf("state = %q, want logged_out", info.State)
	}
	if info.Paired {
		t.Fatal("restored session must report paired=false")
	}
	if info.JID != "" {
		t.Fatalf("restored session must expose an empty JID, got %q", info.JID)
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

// TestPairAllowedForLoggedOutSession is the user-visible recovery guard: after
// an EXTERNAL logout the session keeps Store.ID in memory but is `logged_out`,
// so Reactivate must not be rejected as "already paired" — it must replace the
// dead-credential client and try to open a fresh QR channel.
func TestPairAllowedForLoggedOutSession(t *testing.T) {
	m := newTestManager(t)
	blockOutboundWhatsApp(t)
	t.Cleanup(m.disconnectAll)

	s := addCredentialedSession(t, m, "logged out")
	s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})

	s.mu.Lock()
	if s.client == nil || s.client.Store == nil || s.client.Store.ID == nil {
		s.mu.Unlock()
		t.Fatal("precondition: session must hold a device ID in memory")
	}
	oldClient := s.client
	s.mu.Unlock()

	err := m.Pair(s.id)
	if err != nil && strings.Contains(err.Error(), "already paired") {
		t.Fatalf("Pair on a logged_out session must allow recovery, got %v", err)
	}

	s.mu.Lock()
	newClient := s.client
	s.mu.Unlock()
	if newClient == oldClient {
		t.Fatal("Pair must replace the client holding dead credentials")
	}
	if newClient == nil || newClient.Store == nil || newClient.Store.ID != nil {
		t.Fatal("replacement client must start with a fresh, ID-less device store")
	}
}

func TestPairRejectedWhenOpen(t *testing.T) {
	m := newTestManager(t)

	s := addCredentialedSession(t, m, "open")
	s.setAuth(AuthSnapshot{State: "open", Paired: true})

	err := m.Pair(s.id)
	if err == nil || !strings.Contains(err.Error(), "already paired") {
		t.Fatalf("Pair on an open, credentialed session = %v, want already paired", err)
	}
}

// TestLoggedOutClearsPersistedJID: an external logout must clear the persisted
// JID (so a restart treats the session as unscanned) while ever_paired stays 1
// and the session reports paired=false.
func TestLoggedOutClearsPersistedJID(t *testing.T) {
	m := newTestManager(t)

	s := addCredentialedSession(t, m, "external logout")
	s.handleEvent(&events.LoggedOut{})

	rows, err := m.store.list(m.appCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != s.id {
		t.Fatalf("expected the session row to survive, got %+v", rows)
	}
	if rows[0].JID != "" {
		t.Fatalf("logged_out must clear the persisted JID, got %q", rows[0].JID)
	}
	if !rows[0].EverPaired {
		t.Fatal("ever_paired must stay true after a logout")
	}
	if info := s.info(); info.State != "logged_out" || info.Paired {
		t.Fatalf("info() = %+v, want logged_out with paired=false", info)
	}
}
