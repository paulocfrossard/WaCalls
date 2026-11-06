package main

import (
	"log/slog"
	"sync"
	"time"
)

// browserGraceWindow is how long a call is held after its browser leg drops
// (e.g. a page refresh) before it is terminated: a refreshed page re-attaches
// its WebRTC bridge within this window, otherwise the call ends.
const browserGraceWindow = 30 * time.Second

// graceKeeper holds a per-call countdown that terminates a call whose browser
// leg dropped (e.g. a page refresh) if no new leg re-attaches within the
// window. Arming is idempotent per call: a re-arm restarts the window, and
// cancel stops it.
type graceKeeper struct {
	window   time.Duration
	onExpire func(callID string)

	mu     sync.Mutex
	timers map[string]*time.Timer
}

func newGraceKeeper(window time.Duration, onExpire func(callID string)) *graceKeeper {
	return &graceKeeper{
		window:   window,
		onExpire: onExpire,
		timers:   map[string]*time.Timer{},
	}
}

// arm/cancel/stopAll are nil-safe: tests build bare &Session{} literals
// without a keeper, and a missing grace window must not panic a request.
func (g *graceKeeper) arm(callID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.timers[callID]; t != nil {
		t.Stop()
	}
	// The callback re-checks that this exact timer is still the registered
	// one before expiring: a cancel/re-arm that landed after the timer fired
	// but before the callback took the lock wins, so a re-attached call is
	// never terminated by a stale expiry.
	var t *time.Timer
	t = time.AfterFunc(g.window, func() {
		g.mu.Lock()
		if g.timers[callID] != t {
			g.mu.Unlock()
			return
		}
		delete(g.timers, callID)
		g.mu.Unlock()
		g.safeExpire(callID)
	})
	g.timers[callID] = t
}

// safeExpire runs the expiry callback with panic recovery: time.AfterFunc
// fires on its own goroutine, and a panic in EndCall must not kill the process.
func (g *graceKeeper) safeExpire(callID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("grace expiry panicked; call left as-is", "call_id", callID, "panic", r)
		}
	}()
	g.onExpire(callID)
}

func (g *graceKeeper) cancel(callID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.timers[callID]; t != nil {
		t.Stop()
		delete(g.timers, callID)
	}
}

func (g *graceKeeper) stopAll() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, t := range g.timers {
		t.Stop()
		delete(g.timers, id)
	}
}
