package main

import (
	"context"
	"encoding/json"
	"log/slog"
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

	// ownerKey is the principal-scoped conflict bucket: "user:<userID>" for an
	// authenticated principal, else "client:<clientID>" for an anonymous
	// caller, falling back to "client:<clientIP>" when no client id is
	// supplied. It is never serialized: clients only ever see Owner, which
	// the SPA compares against its own client id. Owner alone cannot be the
	// conflict bucket because a client-supplied id is spoofable across users.
	ownerKey string
}

// conflictKey returns the bucket used to detect that an operator is already on
// a call: the principal-scoped ownerKey when known, else the UI-facing owner
// (records created without an authenticated principal).
func (c *CallRecord) conflictKey() string {
	if c.ownerKey != "" {
		return c.ownerKey
	}
	if c.Owner != nil {
		return *c.Owner
	}
	return ""
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

// authVerdict is the tri-state result of a keepalive revalidation. It lets a
// transient lookup failure (timeout, busy database, scan error) keep the
// stream open for up to maxTransientKeepalives (3) consecutive ticks instead
// of tearing down every stream during a brief outage, while a definitive
// invalid verdict still fails closed.
type authVerdict int

const (
	authValid authVerdict = iota
	authInvalid
	authTransient
)

// maxTransientKeepalives bounds how many consecutive authTransient verdicts a
// single stream may absorb. At the 20s keepalive interval that is ~60s of
// tolerated database stall before the stream is closed, so a wedged auth store
// cannot keep a possibly-dead principal connected indefinitely.
const maxTransientKeepalives = 3

type authChecker func(sessionID, keyID string) authVerdict

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

	// sendMu serializes sends on ch against closeCh. broadcast snapshots the
	// subscriber set under b.mu and releases it before sending, so unsubscribe
	// may close ch in that window; chClosed (guarded by sendMu) makes such a
	// late send a no-op instead of a panic.
	sendMu   sync.RWMutex
	chClosed bool
}

// closeCh closes the data channel exactly once. Safe to call concurrently
// and from multiple sites (unsubscribe and revokeMatching).
func (s *subscriber) closeCh() {
	s.chCloseOnce.Do(func() {
		s.sendMu.Lock()
		s.chClosed = true
		close(s.ch)
		s.sendMu.Unlock()
	})
}

// trySend performs a non-blocking send of data. It reports false when the
// subscriber's buffer is full or the channel was already closed.
func (s *subscriber) trySend(data []byte) bool {
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.chClosed {
		return false
	}
	select {
	case s.ch <- data:
		return true
	default:
		return false
	}
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

	// pending holds in-flight owner reservations (ownerKey→callID, with "" for
	// a call that has not been created yet). It closes the TOCTOU window
	// between an ownership check and the claim: ReserveOwner/ReleaseOwner
	// manipulate it under mu, and setOwner clears the bucket on commit.
	pending map[string]string

	SnapshotFn func() []any

	// keepaliveInterval bounds how long a stream may go without revalidating
	// its principal. Zero falls back to 20s.
	keepaliveInterval time.Duration

	// authCheck revalidates a stream's principal read-only. A verdict of
	// authInvalid closes the stream on the next keepalive; authTransient keeps
	// it open until maxTransientKeepalives consecutive transient verdicts are
	// reached. Nil disables revalidation.
	authCheck authChecker

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
		pending:           map[string]string{},
		snapshotDirty:     true,
		snapshotGen:       0,
		keepaliveInterval: 20 * time.Second,
		revokeCh:          make(chan ssePrincipal, 1024),
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
					slog.Error("revocation worker panic", "panic", r)
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
	// Snapshot the subscriber set under RLock and send outside it, so a slow
	// or contended receiver never blocks Subscribe/unsubscribe/Revoke.
	b.mu.RLock()
	subs := make([]*subscriber, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.RUnlock()
	for _, s := range subs {
		if !s.trySend(data) {
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
	// A state-change rebuild (session.wireCall) copies only the UI-facing
	// Owner, so carry the principal-scoped bucket over instead of dropping it.
	if prev, ok := b.calls[r.CallID]; ok && cp.ownerKey == "" && cp.Owner != nil {
		cp.ownerKey = prev.ownerKey
	}
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

// ReserveOwner atomically reserves the conflict bucket ownerKey for callID.
// It fails when the bucket is already held by an in-flight reservation for a
// different call, or by an active (non-ended) call of a different callID.
// callID may be empty for a call that does not exist yet; an empty-callID
// reservation is exclusive so two concurrent starts for one operator cannot
// both pass. On success the reservation is recorded in pending until setOwner
// commits the claim or ReleaseOwner cancels it.
func (b *Broker) ReserveOwner(ownerKey, callID string) bool {
	if ownerKey == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if held, ok := b.pending[ownerKey]; ok && (callID == "" || held != callID) {
		return false
	}
	for id, c := range b.calls {
		if id != callID && c.Status != StatusEnded && c.conflictKey() == ownerKey {
			return false
		}
	}
	b.pending[ownerKey] = callID
	return true
}

// ReleaseOwner cancels an in-flight reservation for ownerKey, but only when it
// belongs to callID (empty callID matches an empty reservation). It is a no-op
// after setOwner has already cleared the bucket.
func (b *Broker) ReleaseOwner(ownerKey, callID string) {
	if ownerKey == "" {
		return
	}
	b.mu.Lock()
	if held, ok := b.pending[ownerKey]; ok && held == callID {
		delete(b.pending, ownerKey)
	}
	b.mu.Unlock()
}

// setOwner claims a call for owner (the UI-facing client id) and records the
// principal-scoped bucket used by ownerActiveCall. It refuses to overwrite a
// call already claimed in a different bucket; the raw owner is updated only
// when the bucket matches (or was empty). A successful claim also clears the
// pending reservation for that bucket.
func (b *Broker) setOwner(id, owner, ownerKey string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.calls[id]
	if !ok {
		return false
	}
	if c.ownerKey != "" && c.ownerKey != ownerKey {
		return false
	}
	c.Owner = &owner
	c.ownerKey = ownerKey
	if held, ok := b.pending[ownerKey]; ok && (held == "" || held == id) {
		delete(b.pending, ownerKey)
	}
	return true
}

// ownerActiveCall reports the live call claimed by the given conflict bucket
// (see ownerKey on the server), or "" when the operator is idle.
func (b *Broker) ownerActiveCall(owner string) string {
	if owner == "" {
		return ""
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for id, c := range b.calls {
		if c.Owner != nil && c.Status != StatusEnded && c.conflictKey() == owner {
			return id
		}
	}
	return ""
}

// callOwnerKey reports the conflict bucket currently recorded for a call.
func (b *Broker) callOwnerKey(id string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.calls[id]
	if !ok || c == nil {
		return "", false
	}
	return c.ownerKey, true
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
	// RESIDUAL (owner exposure): call-list, call-status and the session events
	// carry the raw Owner (UI client id) to every authenticated subscriber,
	// not just the owning principal. That exposes an opaque identifier used
	// for own-call detection, not a credential, so it cannot be replayed to
	// claim a call (claims are authorized by ownerKey). Scoping these
	// broadcasts per principal is deferred to a later change.
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

// serveSSEWithPrincipal streams events for the authenticated principal p.
//
// The stream ends when the request is canceled, when the subscriber is revoked
// (sub.done), or when a keepalive revalidation reports the principal invalid.
// Revalidation is read-only: a transient lookup failure is tolerated for at
// most maxTransientKeepalives consecutive ticks, then the stream is closed.
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

	// transient counts consecutive inconclusive revalidations for this
	// stream. Any definitive verdict resets or ends it.
	transient := 0

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.done:
			// Revoked: the principal lost its session or key.
			return
		case data, ok := <-sub.ch:
			if !ok {
				return
			}
			if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if b.authCheck != nil && (p.sessionID != "" || p.keyID != "") {
				switch b.authCheck(p.sessionID, p.keyID) {
				case authValid:
					transient = 0
				case authInvalid:
					slog.Default().Debug("keepalive revalidation failed: principal invalid",
						"sessionID", p.sessionID, "keyID", p.keyID)
					return
				case authTransient:
					transient++
					if transient >= maxTransientKeepalives {
						slog.Default().Warn("keepalive revalidation kept failing; closing stream",
							"sessionID", p.sessionID, "keyID", p.keyID, "consecutive", transient)
						return
					}
					// Inconclusive lookup: only the DB verdict is deferred.
					// Still emit this tick's ping so the client sees liveness,
					// then retry the revalidation on the next tick.
					slog.Default().Debug("keepalive revalidation inconclusive; deferring verdict",
						"sessionID", p.sessionID, "keyID", p.keyID, "consecutive", transient)
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
