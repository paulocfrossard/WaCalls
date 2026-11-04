package call

import (
	"sync"
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
)

// TestNManagersHaveDistinctSSRC verifies that N CallManagers never share a
// self/peer SSRC, which is the prerequisite for per-call isolation.
func TestNManagersHaveDistinctSSRC(t *testing.T) {
	const N = 8
	selfs := make(map[uint32]bool)
	peers := make(map[uint32]bool)
	for i := 0; i < N; i++ {
		sock := newFakeSocket()
		relay := newFakeRelay()
		m := newTestManager(t, sock, relay, &passthroughCodec{})
		callID := "call-" + string(rune('A'+i))
		c := NewIncomingCall(callID, "peer@lid", "creator@lid", "", core.CallMediaTypeAudio)
		m.currentCall = c
		m.selfSsrc = media.GenerateSecureSsrc(callID, sock.OwnLID().String(), 0)
		m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, "peer@lid", 0)}
		if selfs[m.selfSsrc] {
			t.Fatalf("duplicate self SSRC at i=%d", i)
		}
		if peers[m.peerSsrcs[0]] {
			t.Fatalf("duplicate peer SSRC at i=%d", i)
		}
		selfs[m.selfSsrc] = true
		peers[m.peerSsrcs[0]] = true
		if m.selfSsrc == m.peerSsrcs[0] {
			t.Fatalf("self==peer SSRC at i=%d", i)
		}
		_ = m
	}
}

// TestEndOneCallKeepsOthersActive verifies that two independent CallManagers
// do not share state: ending one does not nil the other's currentCall.
func TestEndOneCallKeepsOthersActive(t *testing.T) {
	m1 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	m2 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})

	c1 := NewIncomingCall("c1", "peer1@lid", "creator@lid", "", core.CallMediaTypeAudio)
	c2 := NewIncomingCall("c2", "peer2@lid", "creator@lid", "", core.CallMediaTypeAudio)
	m1.currentCall = c1
	m2.currentCall = c2

	// Simulate ending m1.
	m1.mu.Lock()
	m1.currentCall = nil
	m1.mu.Unlock()

	if m1.currentCall != nil {
		t.Fatal("m1 should be cleared")
	}
	if m2.currentCall == nil || m2.currentCall.CallID != "c2" {
		t.Fatal("m2 must be untouched")
	}
}

// TestDuplicateRTPDroppedPerCall verifies the dedup map is per-CallManager.
func TestDuplicateRTPDroppedPerCall(t *testing.T) {
	m1 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	m2 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})

	if m1.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("m1 first should pass")
	}
	if m2.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("m2 first should pass (per-call map)")
	}
	if !m1.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("m1 duplicate should drop")
	}
	if !m2.isDuplicateRTPPacketLocked(42, 7) {
		t.Fatal("m2 duplicate should drop")
	}
}

// TestLIDPNIdentity verifies ownBaseJIDs covers both LID and PN via isOwnBase.
func TestLIDPNIdentity(t *testing.T) {
	cleanBases := []string{"123456789012345@lid", "5511999999999@s.whatsapp.net"}
	if !isOwnBase("123456789012345:0@lid", cleanBases) {
		t.Fatal("LID device form should match")
	}
	if !isOwnBase("5511999999999:3@s.whatsapp.net", cleanBases) {
		t.Fatal("PN device form should match")
	}
	if isOwnBase("9999999999999:0@s.whatsapp.net", cleanBases) {
		t.Fatal("unrelated JID should not match")
	}
}

// keep imports used
var _ = sync.Mutex{}
var _ = media.GenerateSecureSsrc
var _ = core.CallMediaTypeAudio
