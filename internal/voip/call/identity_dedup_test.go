package call

import (
	"context"
	"testing"
)

func TestFindOurDeviceMatchesLIDAndPN(t *testing.T) {
	participants := []string{
		"5511999999999:3@s.whatsapp.net",
		"123456789012345:0@lid",
		"5511888888888:0@s.whatsapp.net",
	}
	// Our PN is 5511999999999 and our LID is 123456789012345; WhatsApp mixes
	// both spellings in the participant list.
	own := []string{"5511999999999@s.whatsapp.net", "123456789012345@lid"}

	if got := findOurDevice(participants, own, "fallback"); got != "5511999999999:3@s.whatsapp.net" {
		t.Fatalf("findOurDevice = %q", got)
	}
	if got := firstPeerDevice(participants, own); got != "5511888888888:0@s.whatsapp.net" {
		t.Fatalf("firstPeerDevice = %q", got)
	}
}

func TestFirstPeerDeviceOnlyLIDForm(t *testing.T) {
	// Regression: matching only the LID made the PN entry look like a peer,
	// subscribing the relay to our own SSRC (one-way audio / self echo).
	participants := []string{
		"5511999999999:0@s.whatsapp.net",
		"123456789012345:0@lid",
		"5511888888888:1@s.whatsapp.net",
	}
	own := []string{"123456789012345@lid", "5511999999999@s.whatsapp.net"}
	if got := firstPeerDevice(participants, own); got != "5511888888888:1@s.whatsapp.net" {
		t.Fatalf("firstPeerDevice = %q", got)
	}
}

func TestDuplicateRTPPacketDropped(t *testing.T) {
	m := &CallManager{}
	if m.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("first occurrence must not be a duplicate")
	}
	if !m.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("same ssrc+seq within the window must be dropped")
	}
	if m.isDuplicateRTPPacketLocked(42, 8) {
		t.Fatal("a new sequence number must pass")
	}
	if m.isDuplicateRTPPacketLocked(43, 7) {
		t.Fatal("a different SSRC with the same seq must pass")
	}
}

func TestDetachedSendContextSurvivesParentCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, done := detachedSendContext(parent)
	defer done()
	if err := ctx.Err(); err != nil {
		t.Fatalf("detached send context must survive parent cancellation, got %v", err)
	}
}
