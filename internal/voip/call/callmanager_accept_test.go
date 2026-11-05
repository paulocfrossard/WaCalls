package call

import (
	"context"
	"testing"

	"wacalls/internal/voip/core"
)

func TestAlreadyAcceptedScopedToIDAndDirection(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})

	in := NewIncomingCall("A", "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
	if err := in.ApplyTransition(Transition{Type: TransitionLocalAccepted}); err != nil {
		t.Fatal(err)
	}
	m.currentCall = in
	if m.AlreadyAccepted("B") {
		t.Fatal("wrong call id must not be reported as accepted")
	}
	if !m.AlreadyAccepted("A") {
		t.Fatal("accepted inbound call must be reported as accepted")
	}

	out := NewOutgoingCall("C", "peer@lid", "me@lid", core.CallMediaTypeAudio)
	m.currentCall = out
	if m.AlreadyAccepted("C") {
		t.Fatal("outbound call must never be reported as an already-accepted inbound call")
	}
}

func TestEndCallForIDIgnoresMismatch(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	c := NewIncomingCall("A", "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
	m.currentCall = c

	if err := m.EndCallForID(context.Background(), "B", core.EndCallReasonUserEnded); err != nil {
		t.Fatalf("mismatch should be a no-op: %v", err)
	}
	if c.IsEnded() {
		t.Fatal("a mismatched id must not end the live call")
	}
}

func TestAbortAcceptOnlyAbortsRinging(t *testing.T) {
	// Ringing: abort succeeds and ends the call.
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	r := NewIncomingCall("A", "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
	m.currentCall = r
	if aborted, _, _ := m.AbortAccept(context.Background(), "A"); !aborted {
		t.Fatal("abort on a ringing call must succeed")
	}
	if !r.IsEnded() {
		t.Fatal("aborted call must be ended")
	}

	// Already accepted: abort is a no-op and must not end the live call.
	m2 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	a := NewIncomingCall("B", "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
	if err := a.ApplyTransition(Transition{Type: TransitionLocalAccepted}); err != nil {
		t.Fatal(err)
	}
	m2.currentCall = a
	if aborted, accepted, _ := m2.AbortAccept(context.Background(), "B"); aborted || !accepted {
		t.Fatalf("abort on an already-accepted call must report accepted only: aborted=%v accepted=%v", aborted, accepted)
	}
	if a.IsEnded() {
		t.Fatal("abort must not end an accepted call")
	}
}

func TestAbortAcceptReportsGone(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})

	// nil currentCall -> gone
	if _, _, gone := m.AbortAccept(context.Background(), "X"); !gone {
		t.Fatal("nil current call must report gone")
	}

	// mismatched live call -> gone, and it must not be ended
	live := NewIncomingCall("A", "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
	m.currentCall = live
	if _, _, gone := m.AbortAccept(context.Background(), "B"); !gone {
		t.Fatal("mismatched id must report gone")
	}
	if live.IsEnded() {
		t.Fatal("mismatched abort must not end the live call")
	}

	// ended call -> gone
	_ = live.ApplyTransition(Transition{Type: TransitionTerminated, Reason: core.EndCallReasonUserEnded})
	if _, _, gone := m.AbortAccept(context.Background(), "A"); !gone {
		t.Fatal("ended call must report gone")
	}
}
