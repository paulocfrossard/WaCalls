package main

import (
	"context"
	"encoding/json"
	"log/slog"
	log "log/slog"
	"net/http"
	"sync"
	"time"
)

type CallStatus string

const (
	StatusStarting  CallStatus = "starting"
	StatusRinging   CallStatus = "ringing"
	StatusConnected CallStatus = "connected"
	StatusEnded     CallStatus = "ended"
)

type CallRecord struct {
	SessionID string     `json:"sessionId"`
	CallID    string     `json:"callId"`
	Owner     *string    `json:"owner"`
	Direction string     `json:"direction"`
	Peer      string     `json:"peer"`
	StartedAt int64      `json:"startedAt"`
	Status    CallStatus `json:"status"`
	EndedAt   *int64     `json:"endedAt,omitempty"`
	EndReason string     `json:"endReason,omitempty"`
}

type AuthSnapshot struct {
	State  string `json:"state"`
	Paired bool   `json:"paired"`
	QR     string `json:"qr,omitempty"`
}

type SessionInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	JID    string `json:"jid"`
	State  string `json:"state"`
	Paired bool   `json:"paired"`
}

// ssePrincipal identifies the authenticated principal behind an SSE stream.
// Empty fields are unset; every stream opened by the SSE handler carries at
// least one of sessionID or keyID (plus userID), so revocation can target it.
type ssePrincipal struct {
	sessionID string
	keyID     string
	userID    string
}

type subscriber struct {
	clientID    string
	sessionID   string
	keyID       string
	userID      string
	ch          chan []byte
	done        chan struct{}
	closeOnce   sync.Once
	chCloseOnce sync.Once
}

// closeCh closes the data channel exactly once. Safe to call concurrently
// and from multiple sites (unsubscribe and revokeMatching).
func (s *subscriber) closeCh() {
	s.chCloseOnce.Do(func() { close(s.ch) })
}

// close signals the serving goroutine that this subscriber was revoked. It is
// idempotent and safe to call concurrently. It never closes ch; only
// unsubscribe may do that.
func (s *subscriber) close() {
	s.closeOnce.Do(func() { close(s.done) })
}

// matches reports whether this subscriber is selected by the revocation
// predicate p. Empty p fields are wildcards; p is never entirely empty.
func (s *subscriber) matches(p ssePrincipal) bool {
	if p.sessionID != "" && s.sessionID != p.sessionID {
		return false
	}
	if p.keyID != "" && s.keyID != p.keyID {
		return false
	}
	if p.userID != "" && s.userID != p.userID {
		return false
	}
	return true
}

type Broker struct {
	mu      sync.RWMutex
	subs    map[*subscriber]struct{}
	calls   map[string]*CallRecord
	history []CallRecord

	SnapshotFn func() []any

	// keepaliveInterval bounds how long a stream may go without revalidating
	// its principal. Zero falls back to 20s.
	keepaliveInterval time.Duration

	// authCheck revalidates a stream's principal read-only. It must be
	// fail-closed: any lookup error reports false and the stream is closed on
	// the next keepalive. Nil disables keepalive revalidation.
	authCheck func(sessionID, keyID string) bool

	// revokeCh carries revocation predicates from Revoke to the single
	// consumer started by StartRevocations. It is bounded so a slow/absent
	// consumer can never block a caller.
	revokeCh chan ssePrincipal

	startOnce sync.Once

	// Cached SSE replay snapshot (H0-11). Recomputed only when dirty.
	snapshotMu    sync.RWMutex
	snapshot      []any
	snapshotDirty bool
	snapshotGen   uint64

	// computeMu serializes SnapshotFn computation to avoid double-compute
	// under contention.
	computeMu sync.Mutex

	// Dispatcher, when non-nil, receives every broadcast event for outbound
	// webhook delivery (Phase B). Dispatch must be non-blocking.
	Dispatcher EventDispatcher
}

func NewBroker() *Broker {
	return &Broker{
		subs:              map[*subscriber]struct{}{},
		calls:             map[string]*CallRecord{},
		snapshotDirty:     true,
		snapshotGen:       0,
		keepaliveInterval: 20 * time.Second,
		revokeCh:          make(chan ssePrincipal, 64),
	}
}

// Subscribe registers an SSE subscriber bound to the authenticated principal.
// The caller owns the returned subscriber and must call unsubscribe exactly
// once when the stream ends.
func (b *Broker) Subscribe(clientID string, p ssePrincipal) *subscriber {
	s := &subscriber{
		clientID:  clientID,
		sessionID: p.sessionID,
		keyID:     p.keyID,
		userID:    p.userID,
		ch:        make(chan []byte, 128),
		done:      make(chan struct{}),
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

// subscribe is the internal alias kept for callers without a principal.
func (b *Broker) subscribe(clientID string) *subscriber {
	return b.Subscribe(clientID, ssePrincipal{})
}

// unsubscribe removes s and closes its data channel. It is idempotent: if s is
// already gone (e.g. removed by Revoke) it does nothing, so a revoked stream's
// channel is never closed twice. close(s.ch) happens under b.mu so no
// broadcast can race a send on it.
func (b *Broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	if _, ok := b.subs[s]; ok {
		delete(b.subs, s)
		s.closeCh()
	}
	b.mu.Unlock()
}

// Revoke enqueues a non-blocking revocation. Empty fields are wildcards; at
// least one field must be set. When the queue is full the request is dropped
// and logged: revocation is also enforced by the keepalive revalidation, so a
// dropped hint cannot keep a dead stream alive indefinitely.
//
// Limitation: streams opened in auth-disabled (dev) mode carry no sessionID or
// keyID, so they are not revalidated by keepalive and a dropped revocation
// hint cannot be recovered for them. Dev mode is insecure by design.
func (b *Broker) Revoke(p ssePrincipal) {
	if p.sessionID == "" && p.keyID == "" && p.userID == "" {
		return
	}
	select {
	case b.revokeCh <- p:
	default:
		slog.Default().Warn("revocation queue full, dropping",
			"sessionId", p.sessionID, "keyId", p.keyID, "userId", p.userID)
	}
}

// StartRevocations starts the single revocation consumer. It closes matching
// streams and removes them from the subscription set; it never closes sub.ch
// (unsubscribe owns that). Safe to call more than once.
func (b *Broker) StartRevocations(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.startOnce.Do(func() {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error("revocation worker panic", "panic", r)
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case p := <-b.revokeCh:
					b.revokeMatching(p)
				}
			}
		}()
	})
}

func (b *Broker) revokeMatching(p ssePrincipal) {
	var matched []*subscriber
	b.mu.Lock()
	for s := range b.subs {
		if s.matches(p) {
			delete(b.subs, s)
			s.closeCh()
			matched = append(matched, s)
		}
	}
	b.mu.Unlock()
	// Close outside the lock: closing done only unblocks the serving goroutine.
	for _, s := range matched {
		s.close()
	}
}

func (b *Broker) broadcast(ev any) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if b.Dispatcher != nil {
		if m, ok := ev.(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				b.Dispatcher.Dispatch(t, ev)
			}
		}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.subs {
		select {
		case s.ch <- data:
		default:
			slog.Default().Warn("dropped broadcast frame", "subscriber", s.clientID)
		}
	}
}

func (b *Broker) emitAuthState(sessionID string, a AuthSnapshot) {
	b.broadcast(map[string]any{
		"type": "auth-state", "sessionId": sessionID,
		"paired": a.Paired, "state": a.State, "qr": a.QR,
	})
	b.invalidateSnapshot()
}

func (b *Broker) emitSessionList(sessions []SessionInfo) {
	b.broadcast(map[string]any{"type": "session-list", "sessions": sessions})
	b.invalidateSnapshot()
}

func (b *Broker) emitSessionQR(sessionID, qr string) {
	b.broadcast(map[string]any{"type": "session-qr", "sessionId": sessionID, "qr": qr})
	b.invalidateSnapshot()
}

func (b *Broker) upsertCall(r CallRecord) {
	b.mu.Lock()
	cp := r
	b.calls[r.CallID] = &cp
	b.mu.Unlock()
	b.broadcastCallList()
	b.broadcast(map[string]any{
		"type": "call-status", "sessionId": r.SessionID, "id": r.CallID, "owner": r.Owner,
		"status": r.Status, "peer": r.Peer, "direction": r.Direction, "startedAt": r.StartedAt,
	})
}

func (b *Broker) getCall(id string) (*CallRecord, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.calls[id]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

func (b *Broker) setOwner(id, owner string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.calls[id]
	if !ok {
		return false
	}
	if c.Owner != nil && *c.Owner != owner {
		return false
	}
	c.Owner = &owner
	return true
}

func (b *Broker) ownerActiveCall(owner string) string {
	if owner == "" {
		return ""
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for id, c := range b.calls {
		if c.Owner != nil && *c.Owner == owner && c.Status != StatusEnded {
			return id
		}
	}
	return ""
}

func (b *Broker) endCall(id, reason string) {
	b.mu.Lock()
	c, ok := b.calls[id]
	if !ok {
		b.mu.Unlock()
		return
	}
	now := time.Now().UnixMilli()
	c.Status = StatusEnded
	c.EndedAt = &now
	c.EndReason = reason
	ended := *c
	delete(b.calls, id)
	b.history = append(b.history, ended)
	if len(b.history) > 1000 {
		trimmed := make([]CallRecord, 1000)
		copy(trimmed, b.history[len(b.history)-1000:])
		b.history = trimmed
	}
	owner := c.Owner
	sessionID := c.SessionID
	peer := ended.Peer
	direction := ended.Direction
	b.mu.Unlock()

	b.broadcast(map[string]any{
		"type": "call-ended", "sessionId": sessionID, "id": id, "owner": owner,
		"peer": peer, "direction": direction, "reason": reason, "endedAt": now,
	})
	b.broadcastCallList()
}

func (b *Broker) broadcastCallList() {
	b.mu.RLock()
	list := make([]CallRecord, 0, len(b.calls))
	for _, c := range b.calls {
		list = append(list, *c)
	}
	b.mu.RUnlock()
	b.broadcast(map[string]any{"type": "call-list", "calls": list})
}

func (b *Broker) emitIncoming(sessionID, id, peer string) {
	b.broadcast(map[string]any{
		"type": "incoming", "sessionId": sessionID, "id": id, "peer": peer, "offeredAt": time.Now().UnixMilli(),
	})
}

func (b *Broker) emitIncomingClaimed(sessionID, id, owner string) {
	b.broadcast(map[string]any{"type": "incoming-claimed", "sessionId": sessionID, "id": id, "owner": owner})
}

func (b *Broker) historyRows(sessionID string, limit int) []CallRecord {
	b.mu.RLock()
	defer b.mu.RUnlock()
	rows := make([]CallRecord, 0, limit)
	for i := len(b.history) - 1; i >= 0 && len(rows) < limit; i-- {
		if sessionID == "" || b.history[i].SessionID == sessionID {
			rows = append(rows, b.history[i])
		}
	}
	return rows
}

// Snapshot returns the cached SSE replay snapshot, recomputing via SnapshotFn
// when dirty (H0-11). Safe for concurrent use.
func (b *Broker) Snapshot() []any {
	b.snapshotMu.RLock()
	if !b.snapshotDirty && b.snapshot != nil {
		out := append([]any(nil), b.snapshot...)
		b.snapshotMu.RUnlock()
		return out
	}
	b.snapshotMu.RUnlock()

	// Serialize computation to avoid double SnapshotFn calls under contention.
	b.computeMu.Lock()
	defer b.computeMu.Unlock()

	// Re-check cache after acquiring computeMu.
	b.snapshotMu.RLock()
	if !b.snapshotDirty && b.snapshot != nil {
		out := append([]any(nil), b.snapshot...)
		b.snapshotMu.RUnlock()
		return out
	}
	gen := b.snapshotGen
	b.snapshotMu.RUnlock()

	// Compute outside snapshotMu so invalidations aren't blocked (M4).
	var evs []any
	if b.SnapshotFn != nil {
		evs = b.SnapshotFn()
	}

	b.snapshotMu.Lock()
	defer b.snapshotMu.Unlock()
	// Re-check: another goroutine may have recomputed while we were unlocked.
	if !b.snapshotDirty && b.snapshot != nil {
		return append([]any(nil), b.snapshot...)
	}
	// Only cache if no invalidation occurred during compute.
	if b.snapshotGen == gen {
		b.snapshot = evs
		b.snapshotDirty = false
	}
	if evs == nil {
		return nil
	}
	return append([]any(nil), evs...)
}

// invalidateSnapshot marks the cached SSE snapshot stale.
func (b *Broker) invalidateSnapshot() {
	b.snapshotMu.Lock()
	b.snapshotGen++
	b.snapshotDirty = true
	b.snapshot = nil
	b.snapshotMu.Unlock()
}

// serveSSE is the compatibility entrypoint. Callers that already have a
// principal pass it as the fourth argument; callers still on the old
// three-argument form open a stream with an empty principal, so that stream is
// not revocable. Both forms are supported so unmigrated call sites keep
// compiling while T2/T3 adopt the principal-aware path.
func (b *Broker) serveSSE(w http.ResponseWriter, r *http.Request, clientID string, p ...ssePrincipal) {
	var principal ssePrincipal
	if len(p) > 0 {
		principal = p[0]
	}
	b.serveSSEWithPrincipal(w, r, clientID, principal)
}

// serveSSEWithPrincipal streams events for the authenticated principal p.
//
// The stream ends when the request is canceled, when the subscriber is revoked
// (sub.done), or when a keepalive revalidation fails. Revalidation is
// read-only and fail-closed: a lookup error closes the stream.
func (b *Broker) serveSSEWithPrincipal(w http.ResponseWriter, r *http.Request, clientID string, p ssePrincipal) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := b.Subscribe(clientID, p)
	defer b.unsubscribe(sub)

	if snap := b.Snapshot(); snap != nil {
		for _, ev := range snap {
			writeSSE(w, flusher, ev)
		}
	}
	// Send the current call list only to this new subscriber instead of
	// broadcasting to all subscribers (avoids O(subs) per connect).
	b.mu.RLock()
	list := make([]CallRecord, 0, len(b.calls))
	for _, c := range b.calls {
		list = append(list, *c)
	}
	b.mu.RUnlock()
	writeSSE(w, flusher, map[string]any{"type": "call-list", "calls": list})

	interval := b.keepaliveInterval
	if interval <= 0 {
		interval = 20 * time.Second
	}
	keepalive := time.NewTicker(interval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			// Revoked: the principal lost its session or key.
			return
		case data := <-sub.ch:
			if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if b.authCheck != nil && (p.sessionID != "" || p.keyID != "") {
				if !b.authCheck(p.sessionID, p.keyID) {
					slog.Default().Debug("keepalive revalidation failed", "sessionID", p.sessionID, "keyID", p.keyID)
					return
				}
			}
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev any) {
	data, _ := json.Marshal(ev)
	w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	f.Flush()
}
