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
