package main

import (
	"sync"

	"wacalls/internal/voip/call"
)

type activeCall struct {
	cm       *call.CallManager
	bridge   browserBridge
	admitted bool
}

type callRegistry struct {
	mu    sync.Mutex
	calls map[string]*activeCall
}

func newCallRegistry() *callRegistry {
	return &callRegistry{calls: map[string]*activeCall{}}
}

func (r *callRegistry) add(callID string, ac *activeCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls[callID] = ac
}

// addIfAbsent inserts ac only when callID is free, in one critical section.
// It returns false if the slot was already taken, so callers never overwrite
// (and orphan) a live call's CallManager.
func (r *callRegistry) addIfAbsent(callID string, ac *activeCall) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.calls[callID]; ok {
		return false
	}
	r.calls[callID] = ac
	return true
}

func (r *callRegistry) get(callID string) (*activeCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	return ac, ok
}

// getWithBridge snapshots the manager and bridge together under the lock, so
// callers do not race with setBridge mutating the bridge field.
func (r *callRegistry) getWithBridge(callID string) (*call.CallManager, browserBridge, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	if !ok {
		return nil, nil, false
	}
	return ac.cm, ac.bridge, true
}

// bridgeIs reports whether callID is still owned by b.
func (r *callRegistry) bridgeIs(callID string, b *Bridge) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	return ok && ac.bridge == b
}

func (r *callRegistry) remove(callID string) (*activeCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	delete(r.calls, callID)
	return ac, true
}

// statsSnapshot returns the per-call media counters for every live call.
func (r *callRegistry) statsSnapshot() []call.CallStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]call.CallStats, 0, len(r.calls))
	for id, ac := range r.calls {
		if ac.cm != nil {
			out = append(out, ac.cm.StatsFor(id))
		}
	}
	return out
}

func (r *callRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *callRegistry) setBridge(callID string, b browserBridge) (browserBridge, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ac, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	oldB := ac.bridge
	ac.bridge = b
	return oldB, true
}

func (r *callRegistry) drain() []*activeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*activeCall, 0, len(r.calls))
	for _, ac := range r.calls {
		out = append(out, ac)
	}
	r.calls = map[string]*activeCall{}
	return out
}
