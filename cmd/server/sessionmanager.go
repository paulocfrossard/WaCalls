package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const maxUnscannedSessions = 10

type SessionManager struct {
	appCtx    context.Context
	container *sqlstore.Container
	broker    *Broker
	store     *sessionStore
	waLogger  waLog.Logger
	log       *slog.Logger
	maxCalls  int

	mu sync.RWMutex
	// sessions has no cap on paired sessions: paired/logged-out accounts are
	// user-managed (created/deleted explicitly), so the only retention limit
	// is maxUnscannedSessions for never-scanned rows. By design.
	sessions  map[string]*Session
	order     []string
	restoring bool
}

func newSessionManager(ctx context.Context, container *sqlstore.Container, broker *Broker, store *sessionStore, waLogger waLog.Logger, log *slog.Logger, maxCalls int) *SessionManager {
	return &SessionManager{
		appCtx:    ctx,
		container: container,
		broker:    broker,
		store:     store,
		waLogger:  waLogger,
		log:       log,
		maxCalls:  maxCalls,
		sessions:  map[string]*Session{},
	}
}

func (m *SessionManager) register(s *Session) {
	m.mu.Lock()
	m.sessions[s.id] = s
	m.order = append(m.order, s.id)
	m.mu.Unlock()
}

func (m *SessionManager) unregister(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	for i, x := range m.order {
		if x == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
}

// isRestoring reports whether Restore is in progress. Used by Session.setAuth
// to suppress per-session broadcasts during boot (P2).
func (m *SessionManager) isRestoring() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.restoring
}

func (m *SessionManager) Get(id string) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	return s, ok
}

// orderedSessions returns the registered sessions in creation order. The read
// lock only covers the map/order walk; Session fields are read afterwards
// under each session's own lock (see Session.info / sessionAuth).
// Lock order (reads): m.mu (RLock) -> connMu (if needed) -> s.mu. Create
// holds connMu then acquires m.mu via register/infos (connMu -> m.mu -> s.mu),
// but broadcasts happen inside that window by design; no inverse path exists
// (m.mu never acquires connMu), so there is no cycle with writes
// (connMu -> s.mu).
func (m *SessionManager) orderedSessions() []*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ordered := make([]*Session, 0, len(m.order))
	for _, id := range m.order {
		if s, ok := m.sessions[id]; ok {
			ordered = append(ordered, s)
		}
	}
	return ordered
}

func (m *SessionManager) infos() []SessionInfo {
	ordered := m.orderedSessions()
	out := make([]SessionInfo, 0, len(ordered))
	for _, s := range ordered {
		out = append(out, s.info())
	}
	return out
}

// sessionAuth reads a session's auth snapshot. Session.auth is guarded by
// Session.mu (session.go); the SSE replay snapshot needs the live QR string,
// which SessionInfo does not carry.
func sessionAuth(s *Session) AuthSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

// snapshotEvents is Broker.SnapshotFn: the replay written directly to an SSE
// subscriber before the live loop starts. It keeps the existing session-list
// frame and appends one auth-state frame per session with meaningful auth
// state (anything but a bare "connecting" with no QR), so a client that
// connects late still recovers pairing state and the current QR code instead
// of waiting for the next code rotation. Frame shape matches
// Broker.emitAuthState: {"type":"auth-state","sessionId","paired","state","qr"}.
// These frames are written straight to the response (broker.go writeSSE) and
// never enter the subscriber's 32-slot channel, so they cannot starve live
// call events.
func (m *SessionManager) snapshotEvents() []any {
	ordered := m.orderedSessions()
	sessions := make([]SessionInfo, 0, len(ordered))
	for _, s := range ordered {
		sessions = append(sessions, s.info())
	}
	evs := make([]any, 0, 1+len(ordered))
	evs = append(evs, map[string]any{"type": "session-list", "sessions": sessions})
	for _, s := range ordered {
		a := sessionAuth(s)
		if a.State == "" || a.State == "connecting" {
			// Bare "connecting" carries nothing beyond session-list (no QR
			// exists yet); qr/open/logged_out and any unknown state do.
			continue
		}
		evs = append(evs, map[string]any{
			"type": "auth-state", "sessionId": s.id,
			"paired": a.Paired, "state": a.State, "qr": a.QR,
		})
	}
	return evs
}

func (m *SessionManager) Restore(ctx context.Context) error {
	m.mu.Lock()
	m.restoring = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.restoring = false
		m.mu.Unlock()
	}()
	rows, err := m.store.list(ctx)
	if err != nil {
		return err
	}
	// Prune old unscanned sessions (ever_paired == false) beyond the
	// retention cap. Paired/logged-out accounts (ever_paired == true) are
	// never touched.
	var unscanned []sessionRow
	for _, r := range rows {
		if !r.EverPaired {
			unscanned = append(unscanned, r)
		}
	}
	if len(unscanned) > maxUnscannedSessions {
		toDelete := unscanned[:len(unscanned)-maxUnscannedSessions]
		deleted := make(map[string]bool, len(toDelete))
		for _, r := range toDelete {
			_ = m.store.delete(ctx, r.ID)
			deleted[r.ID] = true
			m.log.Info("pruned unscanned session", "session", r.ID)
		}
		kept := rows[:0]
		for _, r := range rows {
			if !deleted[r.ID] {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	unscannedRestored := make([]*Session, 0)
	for _, row := range rows {
		if _, ok := m.Get(row.ID); ok {
			// Already registered (e.g. Restore called twice): skip to avoid
			// duplicating m.order and leaking *Session.
			continue
		}
		if row.JID == "" {
			// Never scanned (or logged out): no device row exists to look up,
			// but the session itself is legitimate. Restore it as a fresh,
			// non-open, pairable session instead of deleting it — it enters
			// connect() in state "connecting" (never "open", paired=false)
			// with a fresh auth store, and the state machine in session.go
			// owns everything from there. Deleting here made every dev
			// restart wipe sessions that had simply never been QR-scanned.
			client := whatsmeow.NewClient(m.container.NewDevice(), m.waLogger)
			s := newSession(m, row.ID, row.Name, client)
			m.register(s)
			unscannedRestored = append(unscannedRestored, s)
			s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
			continue
		}
		jid, err := types.ParseJID(row.JID)
		if err != nil {
			m.log.Warn("dropping session with unparseable jid", "session", row.ID, "jid", row.JID)
			_ = m.store.delete(ctx, row.ID)
			continue
		}
		device, err := m.container.GetDevice(ctx, jid)
		if err != nil || device == nil {
			m.log.Warn("dropping session with no stored device", "session", row.ID, "jid", row.JID, "err", err)
			_ = m.store.delete(ctx, row.ID)
			continue
		}
		client := whatsmeow.NewClient(device, m.waLogger)
		s := newSession(m, row.ID, row.Name, client)
		m.register(s)
		if err := s.connect(ctx); err != nil {
			m.log.Error("session connect failed", "session", row.ID, "err", err)
		}
	}
	// Suppress intermediate broadcasts during restore (P2): setAuth is a
	// no-op for broadcasts while m.restoring is true. Emit a single
	// auth-state per unscanned session plus one session-list at the end.
	for _, s := range unscannedRestored {
		m.broker.emitAuthState(s.id, sessionAuth(s))
	}
	m.broker.emitSessionList(m.infos())
	m.log.Info("sessions restored", "count", len(m.infos()))
	return nil
}

func (m *SessionManager) Create(name string) (string, error) {
	id := newSessionID()
	if err := m.store.insert(m.appCtx, id, name); err != nil {
		return "", err
	}
	device := m.container.NewDevice()
	client := whatsmeow.NewClient(device, m.waLogger)
	s := newSession(m, id, name, client)
	s.connMu.Lock()
	m.register(s)
	m.broker.emitSessionList(m.infos())
	if err := s.startPairing(m.appCtx); err != nil {
		s.connMu.Unlock()
		m.log.Error("start pairing failed", "session", id, "err", err)
		return "", fmt.Errorf("start pairing: %w", err)
	}
	s.connMu.Unlock()
	m.log.Info("session created", "session", id, "name", name)
	return id, nil
}

func (m *SessionManager) Delete(ctx context.Context, id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no session %s", id)
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	client := s.client
	handlerID := s.handlerID
	s.mu.Unlock()
	if client != nil {
		client.EnableAutoReconnect = false
		client.RemoveEventHandler(handlerID)
	}
	if client != nil && client.Store != nil && client.Store.ID != nil {
		if err := client.Logout(ctx); err != nil {
			m.log.Warn("logout failed; deleting locally", "session", id, "err", err)
		}
		_ = m.container.DeleteDevice(ctx, client.Store)
	} else if client != nil {
		client.Disconnect()
	}
	s.teardownAllCalls()
	m.unregister(id)
	_ = m.store.delete(ctx, id)
	m.broker.emitSessionList(m.infos())
	m.log.Info("session deleted", "session", id)
	return nil
}

func (m *SessionManager) Logout(ctx context.Context, id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no session %s", id)
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil && client.Store != nil && client.Store.ID != nil {
		if err := client.Logout(ctx); err != nil {
			m.log.Warn("logout failed", "session", id, "err", err)
		}
	}
	s.replaceClient(whatsmeow.NewClient(m.container.NewDevice(), m.waLogger))
	_ = m.store.setJID(ctx, id, "")
	s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
	m.log.Info("session disconnected", "session", id)
	return nil
}

func (m *SessionManager) Pair(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("no session %s", id)
	}
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil && client.Store != nil && client.Store.ID != nil {
		return fmt.Errorf("session already paired")
	}
	s.replaceClient(whatsmeow.NewClient(m.container.NewDevice(), m.waLogger))
	if err := s.startPairing(m.appCtx); err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	m.broker.emitSessionList(m.infos())
	m.log.Info("session re-pairing", "session", id)
	return nil
}

func (m *SessionManager) disconnectAll() {
	m.mu.RLock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.RUnlock()
	for _, s := range all {
		s.shutdown()
	}
}
