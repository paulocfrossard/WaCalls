package main

import (
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
	g.timers[callID] = time.AfterFunc(g.window, func() {
		g.mu.Lock()
		delete(g.timers, callID)
		g.mu.Unlock()
		g.onExpire(callID)
	})
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
