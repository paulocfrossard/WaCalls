package main

import (
	"sync"
	"testing"
)

func TestConcurrentOffersSingleWinnerServer(t *testing.T) {
	reg := newCallRegistry()
	const N = 50
	var wg sync.WaitGroup
	wins := make(chan bool, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wins <- reg.addIfAbsent("c1", &activeCall{cm: nil})
		}()
	}
	wg.Wait()
	close(wins)
	n := 0
	for ok := range wins {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", n)
	}
}

func TestRegistryRemoveDoesNotAffectOthers(t *testing.T) {
	reg := newCallRegistry()
	reg.add("a", &activeCall{})
	reg.add("b", &activeCall{})
	reg.add("c", &activeCall{})
	if _, ok := reg.remove("b"); !ok {
		t.Fatal("remove b should succeed")
	}
	if reg.count() != 2 {
		t.Fatalf("count=%d", reg.count())
	}
	if _, ok := reg.get("b"); ok {
		t.Fatal("b should be gone")
	}
	for _, id := range []string{"a", "c"} {
		if _, ok := reg.get(id); !ok {
			t.Fatalf("%s should remain", id)
		}
	}
}

func TestBridgeIsRejectsStale(t *testing.T) {
	reg := newCallRegistry()
	b1 := &Bridge{}
	b2 := &Bridge{}
	reg.add("c1", &activeCall{cm: nil, bridge: b1})
	old, ok := reg.setBridge("c1", b2)
	if !ok || old != b1 {
		t.Fatal("setBridge replace failed")
	}
	if reg.bridgeIs("c1", b1) {
		t.Fatal("stale bridge must not be current")
	}
	if !reg.bridgeIs("c1", b2) {
		t.Fatal("new bridge must be current")
	}
}

func TestAdmissionCapAndRelease(t *testing.T) {
	b := NewBroker()
	b.SetMaxTotalCalls(2)
	if !b.tryAdmit() || !b.tryAdmit() {
		t.Fatal("first two admissions must succeed")
	}
	if b.tryAdmit() {
		t.Fatal("third admission must be rejected at cap 2")
	}
	if b.ActiveCallCount() != 2 {
		t.Fatalf("active=%d want 2", b.ActiveCallCount())
	}
	b.releaseAdmission()
	if b.ActiveCallCount() != 1 {
		t.Fatalf("active=%d want 1", b.ActiveCallCount())
	}
	if !b.tryAdmit() {
		t.Fatal("admission must succeed after release")
	}
	// Releasing more than admitted must not go negative.
	b.releaseAdmission()
	b.releaseAdmission()
	b.releaseAdmission()
	if b.ActiveCallCount() != 0 {
		t.Fatalf("active=%d want 0 (must not go negative)", b.ActiveCallCount())
	}
}
