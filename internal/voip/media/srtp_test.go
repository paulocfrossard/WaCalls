package media

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"wacalls/internal/voip/core"
)

func TestMain(m *testing.M) {
	// Force the auth kill-switch ON so tests never depend on global state
	// left behind by TestSrtpAuthDisabledAllowsTampered.
	SetAuthVerify(true)
	os.Exit(m.Run())
}

func newTestSrtpContext(t *testing.T) *SrtpContext {
	t.Helper()
	keying := core.SrtpKeyingMaterial{
		MasterKey:  bytes.Repeat([]byte{0x11}, 16),
		MasterSalt: bytes.Repeat([]byte{0x22}, 14),
	}
	ctx, err := NewSrtpContext(keying, 0)
	if err != nil {
		t.Fatalf("NewSrtpContext: %v", err)
	}
	return ctx
}

func protectTestPacket(t *testing.T, ctx *SrtpContext, seq uint16, payload []byte) []byte {
	t.Helper()
	pkt := &RtpPacket{
		Header:  NewRtpHeader(core.PayloadTypeWhatsAppOpus, seq, 1000, 0x0A0B0C0D),
		Payload: payload,
	}
	out, err := ctx.Protect(pkt)
	if err != nil {
		t.Fatalf("Protect: %v", err)
	}
	return out
}

func TestSrtpRoundtripAuthVerified(t *testing.T) {
	ctx := newTestSrtpContext(t)
	payload := []byte("hello srtp frame")
	sealed := protectTestPacket(t, ctx, 1, payload)

	got, err := ctx.Unprotect(sealed)
	if err != nil {
		t.Fatalf("Unprotect: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got.Payload, payload)
	}
	if got.Header.Ssrc != 0x0A0B0C0D {
		t.Fatalf("ssrc mismatch: %#x", got.Header.Ssrc)
	}
}

func TestSrtpUnprotectRejectsCorruptedTag(t *testing.T) {
	ctx := newTestSrtpContext(t)
	sealed := protectTestPacket(t, ctx, 2, []byte("payload"))
	sealed[len(sealed)-1] ^= 0x01

	if _, err := ctx.Unprotect(sealed); err == nil {
		t.Fatal("expected auth error for corrupted tag, got nil")
	} else if !errors.Is(err, ErrAuthenticate) {
		t.Fatalf("expected ErrAuthenticate, got %v", err)
	}
}

func TestSrtpUnprotectRejectsTamperedPayload(t *testing.T) {
	ctx := newTestSrtpContext(t)
	sealed := protectTestPacket(t, ctx, 3, []byte("payload"))
	sealed[12] ^= 0x01 // flip a bit in the ciphertext (after 12B header)

	if _, err := ctx.Unprotect(sealed); err == nil {
		t.Fatal("expected auth error for tampered payload, got nil")
	} else if !errors.Is(err, ErrAuthenticate) {
		t.Fatalf("expected ErrAuthenticate, got %v", err)
	}
}

func TestSrtpUnprotectRejectsTruncatedTag(t *testing.T) {
	ctx := newTestSrtpContext(t)
	sealed := protectTestPacket(t, ctx, 4, []byte("payload"))
	truncated := sealed[:len(sealed)-2]

	if _, err := ctx.Unprotect(truncated); err == nil {
		t.Fatal("expected error for truncated tag, got nil")
	}
}

func TestSrtpUnprotectRejectsShortPackets(t *testing.T) {
	ctx := newTestSrtpContext(t)
	for _, n := range []int{0, 1, 11, 12} {
		buf := make([]byte, n)
		if _, err := ctx.Unprotect(buf); err == nil {
			t.Fatalf("packet of %d bytes: expected error, got nil", n)
		}
	}
}

// Documents the escape hatch: with the kill-switch OFF (-srtp-auth=false)
// Unprotect skips the HMAC comparison and decrypts anyway, so a corrupted
// tag is tolerated (legacy pre-auth behavior).
func TestSrtpAuthDisabledAllowsTampered(t *testing.T) {
	SetAuthVerify(false)
	defer SetAuthVerify(true)

	ctx := newTestSrtpContext(t)
	payload := []byte("legacy payload")
	sealed := protectTestPacket(t, ctx, 5, payload)
	sealed[len(sealed)-1] ^= 0x01 // corrupt the auth tag

	got, err := ctx.Unprotect(sealed)
	if err != nil {
		t.Fatalf("expected legacy no-verify behavior, got err: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload mismatch: got %q want %q", got.Payload, payload)
	}
}
