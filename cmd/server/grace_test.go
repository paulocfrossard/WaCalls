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
	s.setBridge("c1", &Bridge{})

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
	s.setBridge("c1", &Bridge{}) // re-attach replaces it

	time.Sleep(40 * time.Millisecond)
	if expired.Load() != 0 {
		t.Fatalf("re-attach over an existing bridge must cancel the grace timer, expired %d", expired.Load())
	}
}
