package main

import (
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

type subscriber struct {
	clientID string
	ch       chan []byte
}

type Broker struct {
	mu      sync.RWMutex
	subs    map[*subscriber]struct{}
	calls   map[string]*CallRecord
	history []CallRecord

	SnapshotFn func() []any

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
		subs:          map[*subscriber]struct{}{},
		calls:         map[string]*CallRecord{},
		snapshotDirty: true,
		snapshotGen:   0,
	}
}

func (b *Broker) subscribe(clientID string) *subscriber {
	s := &subscriber{clientID: clientID, ch: make(chan []byte, 128)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
	close(s.ch)
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

func (b *Broker) serveSSE(w http.ResponseWriter, r *http.Request, clientID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	sub := b.subscribe(clientID)
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

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-sub.ch:
			if _, err := w.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev any) {
	data, _ := json.Marshal(ev)
	w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	f.Flush()
}
