package main

import (
	"testing"

	"wacalls/internal/voip/call"
)

func TestCallRegistryAddGetRemove(t *testing.T) {
	r := newCallRegistry()
	if r.count() != 0 {
		t.Fatalf("new registry must be empty, got %d", r.count())
	}

	a := &activeCall{}
	r.add("c1", a)
	r.add("c2", &activeCall{})
	if r.count() != 2 {
		t.Fatalf("expected 2, got %d", r.count())
	}

	got, ok := r.get("c1")
	if !ok || got != a {
		t.Fatalf("get c1 mismatch: ok=%v", ok)
	}

	removed, ok := r.remove("c1")
	if !ok || removed != a {
		t.Fatalf("remove c1 should return it: ok=%v", ok)
	}
	if _, ok := r.get("c1"); ok {
		t.Fatal("c1 must be gone after remove")
	}
	if _, ok := r.remove("c1"); ok {
		t.Fatal("removing twice must report not-found (idempotent)")
	}
	if r.count() != 1 {
		t.Fatalf("expected 1 left, got %d", r.count())
	}
}

func TestCallRegistryDrain(t *testing.T) {
	r := newCallRegistry()
	r.add("c1", &activeCall{})
	r.add("c2", &activeCall{})
	all := r.drain()
	if len(all) != 2 {
		t.Fatalf("drain should return 2, got %d", len(all))
	}
	if r.count() != 0 {
		t.Fatalf("registry must be empty after drain, got %d", r.count())
	}
}

func TestCallRegistrySetBridgeMissing(t *testing.T) {
	r := newCallRegistry()
	_, found := r.setBridge("nope", nil)
	if found {
		t.Fatal("setBridge on missing call must report not-found")
	}
}

func TestCallRegistryAddIfAbsentKeepsExisting(t *testing.T) {
	r := newCallRegistry()
	first := &activeCall{}
	if !r.addIfAbsent("c1", first) {
		t.Fatal("first insert must succeed")
	}
	if r.addIfAbsent("c1", &activeCall{}) {
		t.Fatal("second insert for the same callID must be rejected")
	}
	got, ok := r.get("c1")
	if !ok || got != first {
		t.Fatal("rejected insert must not overwrite the live call")
	}
}

func TestCallRegistryGetWithBridgeAndBridgeIs(t *testing.T) {
	r := newCallRegistry()
	cm := &call.CallManager{}
	b := &Bridge{}
	r.add("c1", &activeCall{cm: cm, bridge: b})

	gotCm, gotB, ok := r.getWithBridge("c1")
	if !ok || gotCm != cm || gotB != b {
		t.Fatalf("getWithBridge mismatch: ok=%v", ok)
	}
	if !r.bridgeIs("c1", b) {
		t.Fatal("bridgeIs must accept the current bridge")
	}
	if r.bridgeIs("c1", &Bridge{}) {
		t.Fatal("bridgeIs must reject a different bridge")
	}
	if r.bridgeIs("missing", b) {
		t.Fatal("bridgeIs must reject a missing call")
	}
}
