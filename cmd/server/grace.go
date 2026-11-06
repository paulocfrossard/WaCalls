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

	mu sync.Mutex
	// timers tracks the live countdown per call; gen tracks a per-call
	// arm/cancel generation so an expiry can tell "nothing happened since I
	// was armed" apart from "a cancel landed in the gap" — cancel removes the
	// timer entry, which alone looks identical to never having been armed.
	timers map[string]*time.Timer
	gen    map[string]uint64
}

func newGraceKeeper(window time.Duration, onExpire func(callID string)) *graceKeeper {
	return &graceKeeper{
		window:   window,
		onExpire: onExpire,
		timers:   map[string]*time.Timer{},
		gen:      map[string]uint64{},
	}
}

// arm/cancel/stopAll/shouldExpire are nil-safe: tests build bare &Session{}
// literals without a keeper, and a missing grace window must not panic a request.
func (g *graceKeeper) arm(callID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if t := g.timers[callID]; t != nil {
		t.Stop()
	}
	if g.gen == nil {
		g.gen = map[string]uint64{}
	}
	// Every arm bumps the generation; the callback captures the value it was
	// armed with and re-validates it (shouldExpire) right before expiring.
	g.gen[callID]++
	armedGen := g.gen[callID]
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
		// Re-validate before ending the call: with the entry gone, a cancel
		// that lands from here on finds nothing to stop and would silently
		// no-op while this stale expiry proceeds. shouldExpire sees it
		// through the generation bump instead.
		if !g.shouldExpire(callID, armedGen) {
			return
		}
		g.safeExpire(callID)
	})
	g.timers[callID] = t
}

// shouldExpire re-validates under the lock that the arm which fired is still
// current: gen must match the value captured at arm time (a cancel or a
// re-arm bumps it), and no fresh timer may have been installed for callID.
// It runs after the firing callback released the lock, closing the gap in
// which a cancel from setBridge/removeCall would find no timer entry and
// no-op while the stale expiry killed a just re-attached call.
func (g *graceKeeper) shouldExpire(callID string, armedGen uint64) bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gen[callID] != armedGen {
		return false // a cancel or a re-arm landed after the timer fired
	}
	if _, reArmed := g.timers[callID]; reArmed {
		return false // a fresh timer owns this call now
	}
	return true
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
	// Bump the generation even when no timer entry is found: a cancel that
	// lands after the firing callback already deleted its entry must still
	// abort that pending expiry (shouldExpire compares against the gen the
	// arm captured), not silently no-op.
	if g.gen == nil {
		g.gen = map[string]uint64{}
	}
	g.gen[callID]++
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
