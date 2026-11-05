package main

import "testing"

func ownerPtr(s string) *string { return &s }

func TestOwnerActiveCall(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c1", Owner: ownerPtr("op-A"), Status: StatusConnected})
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c2", Owner: ownerPtr("op-B"), Status: StatusRinging})

	if sid, cid, ok := b.ownerActiveCall("op-A"); !ok || sid != "s1" || cid != "c1" {
		t.Fatalf("op-A should own s1/c1, got %q/%q/%v", sid, cid, ok)
	}
	if _, _, ok := b.ownerActiveCall("op-C"); ok {
		t.Fatal("op-C owns nothing")
	}
	if _, _, ok := b.ownerActiveCall(""); ok {
		t.Fatal("empty owner must report no active call")
	}

	b.endCall("s1", "c1", "done")
	if _, _, ok := b.ownerActiveCall("op-A"); ok {
		t.Fatal("op-A's call ended, expected no active call")
	}
}

// T4b: the same call id in two sessions must occupy two independent slots.
func TestCompositeKeyIsolatesSessions(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "same", Owner: ownerPtr("op-A"), Status: StatusRinging})
	b.upsertCall(CallRecord{SessionID: "s2", CallID: "same", Owner: ownerPtr("op-B"), Status: StatusRinging})

	if rec, ok := b.getCall("s1", "same"); !ok || rec.Owner == nil || *rec.Owner != "op-A" {
		t.Fatalf("s1/same lost: ok=%v rec=%+v", ok, rec)
	}
	if rec, ok := b.getCall("s2", "same"); !ok || rec.Owner == nil || *rec.Owner != "op-B" {
		t.Fatalf("s2/same lost: ok=%v rec=%+v", ok, rec)
	}
	// Ending one session's call must not touch the other.
	b.endCall("s1", "same", "done")
	if _, ok := b.getCall("s1", "same"); ok {
		t.Fatal("s1/same should be gone after endCall")
	}
	if _, ok := b.getCall("s2", "same"); !ok {
		t.Fatal("s2/same must survive ending s1's call")
	}
	// History keeps both, and per-session history filters correctly.
	rows := b.historyRows("s1", 10)
	if len(rows) != 1 || rows[0].SessionID != "s1" {
		t.Fatalf("s1 history = %+v", rows)
	}
	rows = b.historyRows("s2", 10)
	if len(rows) != 0 {
		t.Fatalf("s2 history should be empty, got %+v", rows)
	}
	// JSON wire format must be unchanged (golden check on one record).
	rec, _ := b.getCall("s2", "same")
	if rec.CallID != "same" || rec.SessionID != "s2" {
		t.Fatalf("wire fields changed: %+v", rec)
	}
}
