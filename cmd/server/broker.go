package main

import (
	"encoding/json"
	"net/http"
	"strings"
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
	sessions map[string]bool // nil = all sessions
}

func (s *subscriber) wantsSession(sid string) bool {
	if s.sessions == nil {
		return true
	}
	return s.sessions[sid]
}

func (s *subscriber) wantsEvent(ev any) bool {
	m, ok := ev.(map[string]any)
	if !ok {
		return true
	}
	// call-list is filtered per subscriber in broadcast.
	if t, ok := m["type"].(string); ok && t == "call-list" {
		return true
	}
	if sid, ok := m["sessionId"].(string); ok && sid != "" && !s.wantsSession(sid) {
		return false
	}
	// Owner-bearing events are only delivered to the owning client (empty
	// clientID = anonymous LAN client, which sees everything by design).
	if s.clientID != "" {
		if own, ok := m["owner"].(string); ok && own != "" && own != s.clientID {
			return false
		}
		if own, ok := m["owner"].(*string); ok && own != nil && *own != "" && *own != s.clientID {
			return false
		}
	}
	return true
}

type Broker struct {
	mu            sync.RWMutex
	subs          map[*subscriber]struct{}
	calls         map[string]*CallRecord
	history       []CallRecord
	maxTotalCalls int
	totalActive   int
	draining      bool

	SnapshotFn func() []any

	// Dispatcher, when non-nil, receives every broadcast event for outbound
	// webhook delivery (Phase B). Dispatch must be non-blocking.
	Dispatcher EventDispatcher
}

func NewBroker() *Broker {
	return &Broker{
		subs:  map[*subscriber]struct{}{},
		calls: map[string]*CallRecord{},
	}
}

func (b *Broker) SetMaxTotalCalls(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.maxTotalCalls = n
}

func (b *Broker) tryAdmit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.draining {
		return false
	}
	if b.maxTotalCalls > 0 && b.totalActive >= b.maxTotalCalls {
		return false
	}
	b.totalActive++
	return true
}

// SetDraining rejects new calls (used on shutdown so the drain window cannot
// admit a call that will never be terminated).
func (b *Broker) SetDraining(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.draining = v
}

func (b *Broker) releaseAdmission() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.totalActive > 0 {
		b.totalActive--
	}
}

func (b *Broker) ActiveCallCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.totalActive
}

func (b *Broker) MaxTotalCalls() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.maxTotalCalls
}

func (b *Broker) subscribe(clientID string, sessions []string) *subscriber {
	s := &subscriber{clientID: clientID, ch: make(chan []byte, 32)}
	if len(sessions) > 0 {
		s.sessions = make(map[string]bool, len(sessions))
		for _, sid := range sessions {
			s.sessions[sid] = true
		}
	}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *Broker) unsubscribe(s *subscriber) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
	// Do NOT close(s.ch): broadcast snapshots subscribers and sends outside
	// the lock, so closing here would race a send and panic ("send on closed
	// channel"). The reader exits on r.Context().Done(); the channel is GC'd
	// with the subscriber.
}

func (b *Broker) broadcast(ev any) {
	if b.Dispatcher != nil {
		if m, ok := ev.(map[string]any); ok {
			if t, ok := m["type"].(string); ok {
				b.Dispatcher.Dispatch(t, ev)
			}
		}
	}

	// call-list snapshots are filtered per subscriber (owner + session).
	if m, ok := ev.(map[string]any); ok {
		if t, _ := m["type"].(string); t == "call-list" {
			b.broadcastCallList()
			return
		}
	}

	data, err := json.Marshal(ev)
	if err != nil {
		return
	}

	b.mu.RLock()
	subs := make([]*subscriber, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.RUnlock()

	for _, s := range subs {
		if !s.wantsEvent(ev) {
			continue
		}
		select {
		case s.ch <- data:
		default:
		}
	}
}

func (b *Broker) emitAuthState(sessionID string, a AuthSnapshot) {
	b.broadcast(map[string]any{
		"type": "auth-state", "sessionId": sessionID,
		"paired": a.Paired, "state": a.State, "qr": a.QR,
	})
}

func (b *Broker) emitSessionList(sessions []SessionInfo) {
	b.broadcast(map[string]any{"type": "session-list", "sessions": sessions})
}

func (b *Broker) emitSessionQR(sessionID, qr string) {
	b.broadcast(map[string]any{"type": "session-qr", "sessionId": sessionID, "qr": qr})
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

// isOwnerOrUnclaimed reports whether owner may act on id: either the call has
// no owner yet, or it is already owned by owner. Unlike setOwner it does not
// claim an unowned call, so only an existing owner (or a never-claimed call)
// can reject/end it.
func (b *Broker) isOwnerOrUnclaimed(id, owner string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	c, ok := b.calls[id]
	if !ok {
		return true
	}
	if c.Owner == nil {
		return true
	}
	return *c.Owner == owner
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
	calls := make([]CallRecord, 0, len(b.calls))
	for _, c := range b.calls {
		calls = append(calls, *c)
	}
	subs := make([]*subscriber, 0, len(b.subs))
	for s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.RUnlock()

	for _, s := range subs {
		list := make([]CallRecord, 0, len(calls))
		for _, c := range calls {
			if c.Owner != nil && s.clientID != "" && *c.Owner != s.clientID {
				continue // hide other operators' calls
			}
			if !s.wantsSession(c.SessionID) {
				continue
			}
			list = append(list, c)
		}
		payload, err := json.Marshal(map[string]any{"type": "call-list", "calls": list})
		if err != nil {
			continue
		}
		select {
		case s.ch <- payload:
		default:
		}
	}
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

	var sessions []string
	if v := r.URL.Query().Get("sessions"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				sessions = append(sessions, s)
			}
		}
	}

	sub := b.subscribe(clientID, sessions)
	defer b.unsubscribe(sub)

	if b.SnapshotFn != nil {
		for _, ev := range b.SnapshotFn() {
			writeSSE(w, flusher, ev)
		}
	}
	b.broadcastCallList()

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
