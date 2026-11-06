package main

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestGraceKeeperFiresAfterWindow(t *testing.T) {
	done := make(chan string, 1)
	g := newGraceKeeper(10*time.Millisecond, func(callID string) {
		done <- callID
	})
	g.arm("c1")

	select {
	case id := <-done:
		if id != "c1" {
			t.Fatalf("want expiry for c1, got %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("grace timer never fired")
	}
}

func TestGraceKeeperCancelPreventsExpiry(t *testing.T) {
	var fired atomic.Int32
	g := newGraceKeeper(20*time.Millisecond, func(string) { fired.Add(1) })
	g.arm("c1")
	g.cancel("c1")
	time.Sleep(60 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("cancel must prevent expiry, fired %d times", fired.Load())
	}
}

func TestGraceKeeperReArmReplaces(t *testing.T) {
	var fired atomic.Int32
	g := newGraceKeeper(30*time.Millisecond, func(string) { fired.Add(1) })
	g.arm("c1")
	time.Sleep(10 * time.Millisecond)
	g.arm("c1") // re-arm restarts the window
	time.Sleep(60 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("re-arm must yield exactly one expiry, got %d", fired.Load())
	}
}

func TestHoldCallOnBridgeDetachArmsForCurrentBridge(t *testing.T) {
	s := &Session{log: slog.Default(), reg: newCallRegistry()}
	var expired atomic.Int32
	s.grace = newGraceKeeper(5*time.Millisecond, func(string) { expired.Add(1) })

	b := &Bridge{}
	s.reg.add("c1", &activeCall{bridge: b})
	s.holdCallOnBridgeDetach("c1", b)

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 1 {
		t.Fatalf("detaching the current bridge must arm the grace timer, expired %d", expired.Load())
	}
}

func TestHoldCallOnBridgeDetachIgnoresSupersededBridge(t *testing.T) {
	s := &Session{log: slog.Default(), reg: newCallRegistry()}
	var expired atomic.Int32
	s.grace = newGraceKeeper(5*time.Millisecond, func(string) { expired.Add(1) })

	old := &Bridge{}
	current := &Bridge{}
	s.reg.add("c1", &activeCall{bridge: current}) // a re-attach already swapped in a new bridge
	s.holdCallOnBridgeDetach("c1", old)

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("a superseded bridge must not arm the grace timer, expired %d", expired.Load())
	}
}

func TestHoldCallOnBridgeDetachIgnoresRemovedCall(t *testing.T) {
	s := &Session{log: slog.Default(), reg: newCallRegistry()}
	var expired atomic.Int32
	s.grace = newGraceKeeper(5*time.Millisecond, func(string) { expired.Add(1) })

	b := &Bridge{}
	// The call is already gone (removed by an end/reject racing the ICE
	// callback): the late detach must not resurrect a grace timer.
	s.holdCallOnBridgeDetach("c1", b)

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("a removed call must not arm the grace timer, expired %d", expired.Load())
	}
}

func TestRemoveCallCancelsGrace(t *testing.T) {
	s := &Session{log: slog.Default(), reg: newCallRegistry()}
	var expired atomic.Int32
	s.grace = newGraceKeeper(10*time.Millisecond, func(string) { expired.Add(1) })

	s.reg.add("c1", &activeCall{}) // admitted=false: no broker needed
	s.grace.arm("c1")
	s.removeCall("c1")

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("removeCall must cancel the grace timer, expired %d", expired.Load())
	}
	if _, ok := s.reg.get("c1"); ok {
		t.Fatal("removeCall must drop the call from the registry")
	}
}

func TestSetBridgeCancelsGrace(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "grace-reattach")
	var expired atomic.Int32
	s.grace = newGraceKeeper(10*time.Millisecond, func(string) { expired.Add(1) })

	s.reg.add("c1", &activeCall{})
	s.grace.arm("c1")
	if !s.setBridge("c1", &Bridge{}) {
		t.Fatal("setBridge must report the registered call as found")
	}

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("re-attach must cancel the grace timer, expired %d", expired.Load())
	}
}

// TestSetBridgeCancelsGraceWhenReplacingBridge covers the real page-refresh
// path: the dead leg's bridge is still registered when the new leg attaches,
// so setBridge takes the oldB != nil branch — the countdown must still be
// cancelled there (regression: the cancel used to sit after that branch).
func TestSetBridgeCancelsGraceWhenReplacingBridge(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "grace-replace")
	var expired atomic.Int32
	s.grace = newGraceKeeper(10*time.Millisecond, func(string) { expired.Add(1) })

	s.reg.add("c1", &activeCall{bridge: &Bridge{}}) // dead leg still registered
	s.grace.arm("c1")
	if !s.setBridge("c1", &Bridge{}) { // re-attach replaces it
		t.Fatal("setBridge must report the registered call as found")
	}

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("re-attach over an existing bridge must cancel the grace timer, expired %d", expired.Load())
	}
}

// graceGen reads the per-call arm/cancel generation the way the AfterFunc
// callback would have captured it at arm time.
func graceGen(g *graceKeeper, callID string) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gen[callID]
}

// dropTimerEntry simulates the firing callback's own delete of the timer
// entry before it re-validates: from that point on a cancel finds nothing to
// stop, which is exactly the gap the generation check must close.
func dropTimerEntry(g *graceKeeper, callID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.timers, callID)
}

// TestGraceKeeperCancelAfterFireAbortsExpiry is the QA M4a regression: a
// cancel landing after the timer fired but before the expiry decision (it
// finds no entry, so it can only bump the generation) must abort the expiry
// instead of killing a call that just re-attached.
func TestGraceKeeperCancelAfterFireAbortsExpiry(t *testing.T) {
	var fired atomic.Int32
	// A window that never fires on its own: the callback path is driven
	// manually below, so the assertion cannot race the real timer.
	g := newGraceKeeper(time.Hour, func(string) { fired.Add(1) })
	g.arm("c1")
	armedGen := graceGen(g, "c1")

	dropTimerEntry(g, "c1") // the callback deleted its own entry
	g.cancel("c1")          // lands in the gap: entry already gone

	if g.shouldExpire("c1", armedGen) {
		t.Fatal("a cancel that landed after the timer fired must abort the expiry")
	}
	if fired.Load() != 0 {
		t.Fatalf("expiry must not run after the abort, fired %d times", fired.Load())
	}
}

// TestGraceKeeperRearmAfterFireAbortsExpiry: a re-arm in the same gap bumps
// the generation and installs a fresh timer, so the stale callback must not
// expire the call out from under the new countdown.
func TestGraceKeeperRearmAfterFireAbortsExpiry(t *testing.T) {
	var fired atomic.Int32
	g := newGraceKeeper(time.Hour, func(string) { fired.Add(1) })
	g.arm("c1")
	staleGen := graceGen(g, "c1")

	dropTimerEntry(g, "c1") // the first callback deleted its own entry
	g.arm("c1")             // a re-arm lands before the expiry decision

	if g.shouldExpire("c1", staleGen) {
		t.Fatal("a re-arm that landed after the timer fired must abort the stale expiry")
	}
	if fired.Load() != 0 {
		t.Fatalf("expiry must not run after the abort, fired %d times", fired.Load())
	}
}

// TestGraceKeeperShouldExpireWhenNothingLanded is the positive control: with
// no cancel/re-arm in the gap, the same validation must let the expiry
// through — otherwise M4a would be fixed by killing every grace window.
func TestGraceKeeperShouldExpireWhenNothingLanded(t *testing.T) {
	g := newGraceKeeper(time.Hour, func(string) {})
	g.arm("c1")
	armedGen := graceGen(g, "c1")

	dropTimerEntry(g, "c1") // only the callback's own delete happens

	if !g.shouldExpire("c1", armedGen) {
		t.Fatal("an untouched arm must still be allowed to expire")
	}
}

// TestSetBridgeReportsCallMissing: setBridge must report false for a call
// that is not in the registry (the bridge is closed, never attached) and true
// when the swap succeeds — doWebRTC turns false into a 404.
func TestSetBridgeReportsCallMissing(t *testing.T) {
	broker := NewBroker()
	mgr := &SessionManager{
		broker:   broker,
		log:      slog.Default(),
		sessions: map[string]*Session{},
	}
	s := &Session{
		id:    "s1",
		mgr:   mgr,
		log:   slog.Default(),
		reg:   newCallRegistry(),
		grace: newGraceKeeper(time.Hour, func(string) {}),
	}

	if s.setBridge("missing", &Bridge{}) {
		t.Fatal("setBridge must report false for a call that is not registered")
	}
	if _, ok := s.reg.get("missing"); ok {
		t.Fatal("a missing call must not appear in the registry after setBridge")
	}

	s.reg.add("c1", &activeCall{})
	if !s.setBridge("c1", &Bridge{}) {
		t.Fatal("setBridge must report true for a registered call")
	}
	if _, ok := s.reg.get("c1"); !ok {
		t.Fatal("the attached call must stay registered")
	}
}
