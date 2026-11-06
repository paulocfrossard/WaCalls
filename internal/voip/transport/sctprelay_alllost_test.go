package transport

import "testing"

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
