package transport

import (
	"sync/atomic"
	"testing"
	"time"
)

// noteConnectionLost must fire onAllLost exactly once when the last dialed
// connection disappears, and must NOT fire from a manager that never dialed.
func TestNoteConnectionLostFiresOnce(t *testing.T) {
	m := NewSctpRelayManager(nil)
	fired := 0
	m.SetOnAllLost(func() { fired++ })

	// Never dialed: nothing to lose.
	m.noteConnectionLost()
	if fired != 0 {
		t.Fatalf("fired %d times before any dial", fired)
	}

	// Dial two connections, lose them one by one.
	for _, id := range []string{"1.1.1.1:3478", "2.2.2.2:3478"} {
		c := &relayConnection{id: id, stopCh: make(chan struct{})}
		c.setState(relayStateOpen)
		m.mu.Lock()
		m.connections[id] = c
		m.everDialed = true
		m.mu.Unlock()
	}
	m.mu.Lock()
	delete(m.connections, "1.1.1.1:3478")
	m.mu.Unlock()
	m.noteConnectionLost()
	if fired != 0 {
		t.Fatalf("fired while a connection remains: %d", fired)
	}
	m.mu.Lock()
	delete(m.connections, "2.2.2.2:3478")
	m.mu.Unlock()
	m.noteConnectionLost()
	if fired != 1 {
		t.Fatalf("fired %d times after last connection lost, want 1", fired)
	}
	// Re-armed: a subsequent dial cycle can fire again.
	m.mu.Lock()
	m.connections["3.3.3.3:3478"] = &relayConnection{id: "3.3.3.3:3478", stopCh: make(chan struct{})}
	m.everDialed = true
	m.mu.Unlock()
	m.mu.Lock()
	delete(m.connections, "3.3.3.3:3478")
	m.mu.Unlock()
	m.noteConnectionLost()
	if fired != 2 {
		t.Fatalf("fired %d times on second dial cycle, want 2", fired)
	}
}

// TestAllLostWaitsForPendingDials proves a fast-failing dial cannot fire
// all-lost while a sibling dial has not published its connection yet (the
// dial-race that used to end live calls during ConfigureRelays).
func TestAllLostWaitsForPendingDials(t *testing.T) {
	m := NewSctpRelayManager(nil)
	fired := 0
	m.SetOnAllLost(func() { fired++ })

	// ConfigureRelays reserves both dials before spawning.
	m.mu.Lock()
	m.dialing = map[string]bool{"a": true, "b": true}
	m.pendingDials = 2
	m.everDialed = true
	m.mu.Unlock()

	// Dial "a" fails immediately, publishing nothing.
	m.dialFinished("a")
	if fired != 0 {
		t.Fatalf("all-lost fired while a sibling dial is pending: %d", fired)
	}

	// Dial "b" succeeds.
	c := &relayConnection{id: "b", stopCh: make(chan struct{})}
	m.mu.Lock()
	m.connections["b"] = c
	m.mu.Unlock()
	m.dialFinished("b")
	if fired != 0 {
		t.Fatalf("all-lost fired while a connection exists: %d", fired)
	}

	// Later the surviving connection dies: now the cycle may fire.
	m.mu.Lock()
	delete(m.connections, "b")
	m.mu.Unlock()
	m.noteConnectionLost()
	if fired != 1 {
		t.Fatalf("fired %d times after the last connection died, want 1", fired)
	}
}

// TestAllLostFiresWhenEveryDialFailsEarly: every dial in the cycle fails
// before publishing — the last dialFinished must fire exactly once.
func TestAllLostFiresWhenEveryDialFailsEarly(t *testing.T) {
	m := NewSctpRelayManager(nil)
	fired := 0
	m.SetOnAllLost(func() { fired++ })

	m.mu.Lock()
	m.dialing = map[string]bool{"a": true, "b": true}
	m.pendingDials = 2
	m.everDialed = true
	m.mu.Unlock()

	m.dialFinished("a")
	if fired != 0 {
		t.Fatalf("fired %d after first early failure", fired)
	}
	m.dialFinished("b")
	if fired != 1 {
		t.Fatalf("fired %d after last early failure, want 1", fired)
	}
	// A late noteConnectionLost must not fire the consumed cycle again.
	m.noteConnectionLost()
	if fired != 1 {
		t.Fatalf("fired %d total, want exactly 1 per cycle", fired)
	}
}

// TestIceGraceRearmAndCancel: a sustained Disconnected gives up after the
// window; a re-arm restarts it and a recovery (cancel) prevents the expiry.
func TestIceGraceRearmAndCancel(t *testing.T) {
	c := &relayConnection{stopCh: make(chan struct{})}
	var fired atomic.Int32

	c.armIceGrace(15*time.Millisecond, func() { fired.Add(1) })
	time.Sleep(8 * time.Millisecond)
	c.armIceGrace(15*time.Millisecond, func() { fired.Add(1) }) // restart window
	time.Sleep(10 * time.Millisecond)
	if fired.Load() != 0 {
		t.Fatalf("re-arm must restart the window, fired %d", fired.Load())
	}
	time.Sleep(20 * time.Millisecond)
	if fired.Load() != 1 {
		t.Fatalf("sustained Disconnected must fire exactly once, got %d", fired.Load())
	}

	// Recovery path: cancel prevents expiry.
	c2 := &relayConnection{stopCh: make(chan struct{})}
	var fired2 atomic.Int32
	c2.armIceGrace(10*time.Millisecond, func() { fired2.Add(1) })
	c2.cancelIceGrace()
	time.Sleep(30 * time.Millisecond)
	if fired2.Load() != 0 {
		t.Fatalf("cancel must prevent the ice-grace expiry, fired %d", fired2.Load())
	}
}
