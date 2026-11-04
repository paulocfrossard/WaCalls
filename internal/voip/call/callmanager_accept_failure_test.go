package call

import (
	"context"
	"errors"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// A failed accept send must leave the call ringing (not "accepted"), so the
// API layer can roll it back instead of reporting success on a retry.
func TestAcceptSendFailureKeepsRinging(t *testing.T) {
	sock := newFakeSocket()
	sock.sendErr = errors.New("send boom")
	m := newTestManager(t, sock, newFakeRelay(), &passthroughCodec{})

	c := NewIncomingCall("A", "5511888888888@s.whatsapp.net", "5511888888888@s.whatsapp.net", "", core.CallMediaTypeAudio)
	c.EncryptionKey = media.GenerateCallKey()
	m.currentCall = c

	if err := m.AcceptCall(context.Background(), "A"); err == nil {
		t.Fatal("expected accept send failure")
	}
	if m.AlreadyAccepted("A") {
		t.Fatal("a failed accept must not look already-accepted")
	}
	if !m.CanAcceptID("A") {
		t.Fatal("the call must remain acceptable after a failed send")
	}
	if c.IsEnded() {
		t.Fatal("a failed send must not end the call")
	}
}
