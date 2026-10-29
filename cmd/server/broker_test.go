package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func ownerPtr(s string) *string { return &s }

func TestOwnerActiveCall(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c1", Owner: ownerPtr("op-A"), Status: StatusConnected})
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "c2", Owner: ownerPtr("op-B"), Status: StatusRinging})

	if got := b.ownerActiveCall("op-A"); got != "c1" {
		t.Fatalf("op-A should own c1, got %q", got)
	}
	if got := b.ownerActiveCall("op-C"); got != "" {
		t.Fatalf("op-C owns nothing, got %q", got)
	}
	if got := b.ownerActiveCall(""); got != "" {
		t.Fatalf("empty owner must return empty, got %q", got)
	}

	b.endCall("c1", "done")
	if got := b.ownerActiveCall("op-A"); got != "" {
		t.Fatalf("op-A's call ended, expected empty, got %q", got)
	}
}

// ownerKeyRequest builds a request carrying the authenticated principal the
// way authMiddleware attaches one after a successful credential check. An
// empty userID models an anonymous (auth-disabled) caller.
func ownerKeyRequest(t *testing.T, userID, clientID string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/s1/calls", nil)
	req.Header.Set("X-Client-Id", clientID)
	if userID == "" {
		return req
	}
	ctx := context.WithValue(req.Context(), authContextKey{}, authInfo{userID: userID, method: "session"})
	return req.WithContext(ctx)
}

// TestOwnerKeyPerUserBucket verifies the conflict bucket is scoped to the
// authenticated user, not to the client-supplied X-Client-Id: two tabs of the
// same user share one bucket, so a second concurrent start is rejected.
func TestOwnerKeyPerUserBucket(t *testing.T) {
	s := &server{}
	k1 := s.ownerKey(ownerKeyRequest(t, "user-A", "client-tab-1"))
	k2 := s.ownerKey(ownerKeyRequest(t, "user-A", "client-tab-2"))

	if k1 != k2 {
		t.Fatalf("same user with different X-Client-Id must share one bucket: %q != %q", k1, k2)
	}
	if want := "user:user-A"; k1 != want {
		t.Fatalf("bucket = %q, want %q", k1, want)
	}

	b := NewBroker()
	// The first start reserves the bucket before its call exists (empty callID).
	if !b.ReserveOwner(k1, "") {
		t.Fatal("first start should reserve the user bucket")
	}
	// A second concurrent start from another tab presents a different
	// X-Client-Id but must still be rejected.
	if b.ReserveOwner(k2, "") {
		t.Fatal("second concurrent start from the same user must be rejected")
	}
	b.ReleaseOwner(k1, "")
	if !b.ReserveOwner(k2, "") {
		t.Fatal("bucket should be free after ReleaseOwner")
	}
}

// TestOwnerKeySpoofDoesNotHijack verifies that replaying the victim's
// X-Client-Id does not move user B into user A's bucket, and that setOwner
// refuses a cross-principal claim on A's call.
func TestOwnerKeySpoofDoesNotHijack(t *testing.T) {
	s := &server{}
	reqA := ownerKeyRequest(t, "user-A", "client-shared")
	reqB := ownerKeyRequest(t, "user-B", "client-shared") // B spoofs A's client id

	keyA, keyB := s.ownerKey(reqA), s.ownerKey(reqB)
	if keyA == keyB {
		t.Fatalf("different users must never share a bucket: both %q", keyA)
	}
	if want := "user:user-B"; keyB != want {
		t.Fatalf("B's bucket = %q, want %q", keyB, want)
	}

	b := NewBroker()
	rawA := clientID(reqA)
	b.upsertCall(CallRecord{
		SessionID: "s1", CallID: "call-A",
		Owner: &rawA, ownerKey: keyA, Status: StatusConnected,
	})

	// B must not observe A's call as its own active call.
	if other := b.ownerActiveCall(keyB); other != "" {
		t.Fatalf("spoofed client id exposed A's call to B: %q", other)
	}
	// Claiming A's call with B's bucket must be refused, even though the raw
	// client ids are identical (the spoof).
	if b.setOwner("call-A", clientID(reqB), keyB) {
		t.Fatal("setOwner accepted a cross-principal claim on A's call")
	}
	rec, ok := b.getCall("call-A")
	if !ok {
		t.Fatal("call-A disappeared after a refused claim")
	}
	if rec.conflictKey() != keyA {
		t.Fatalf("A's bucket changed to %q, want %q", rec.conflictKey(), keyA)
	}
	if rec.Owner == nil || *rec.Owner != rawA {
		t.Fatalf("A's raw owner changed to %v, want %q", rec.Owner, rawA)
	}
}

// TestReserveOwnerAtomic verifies concurrent reservations of one bucket with
// different call ids admit exactly one winner, that ReleaseOwner frees the
// bucket, and that another bucket is unaffected.
func TestReserveOwnerAtomic(t *testing.T) {
	b := NewBroker()
	const contenders = 16
	const bucket = "user:user-A"

	start := make(chan struct{})
	wins := make(chan string, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			callID := fmt.Sprintf("call-%d", i)
			if b.ReserveOwner(bucket, callID) {
				wins <- callID
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(wins)

	var winners []string
	for id := range wins {
		winners = append(winners, id)
	}
	if len(winners) != 1 {
		t.Fatalf("exactly one reservation must win, got %d: %v", len(winners), winners)
	}

	// A different bucket is independent of the contended one.
	if !b.ReserveOwner("user:user-B", "call-other") {
		t.Fatal("a different bucket must not be blocked by the contended bucket")
	}
	// The contended bucket stays reserved until its winner releases it.
	if b.ReserveOwner(bucket, "call-loser") {
		t.Fatal("bucket must remain reserved until ReleaseOwner")
	}

	b.ReleaseOwner(bucket, winners[0])
	if !b.ReserveOwner(bucket, "call-after-release") {
		t.Fatal("ReleaseOwner must free the bucket")
	}
}

// TestSetOwnerKeepsRawOwner verifies the two identities stay separate: Owner
// keeps the raw client id the SPA compares against, while the conflict bucket
// remains the principal-scoped ownerKey.
func TestSetOwnerKeepsRawOwner(t *testing.T) {
	b := NewBroker()
	b.upsertCall(CallRecord{SessionID: "s1", CallID: "call-1", Status: StatusRinging})

	const rawClientID = "client-tab-7"
	const bucket = "user:user-A"
	if !b.setOwner("call-1", rawClientID, bucket) {
		t.Fatal("setOwner refused an unclaimed call")
	}

	rec, ok := b.getCall("call-1")
	if !ok {
		t.Fatal("call-1 not found after setOwner")
	}
	if rec.Owner == nil || *rec.Owner != rawClientID {
		t.Fatalf("Owner = %v, want raw client id %q", rec.Owner, rawClientID)
	}
	if got := rec.conflictKey(); got != bucket {
		t.Fatalf("conflict bucket = %q, want %q", got, bucket)
	}
	if got := b.ownerActiveCall(bucket); got != "call-1" {
		t.Fatalf("ownerActiveCall(%q) = %q, want call-1", bucket, got)
	}
	// The raw client id is UI-facing only; it must not act as the bucket once
	// the principal-scoped ownerKey is known.
	if got := b.ownerActiveCall(rawClientID); got != "" {
		t.Fatalf("raw client id must not resolve as a bucket, got %q", got)
	}
}
