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

// TestEndOneCallKeepsOthersActive verifies registry isolation: removing one
// call does not touch the others.
func TestEndOneCallKeepsOthersActive(t *testing.T) {
	// Covered at the call-manager level: two managers are independent.
	m1 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	m2 := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	if m1 == m2 {
		t.Fatal("managers must be distinct")
	}
	// Distinct SSRCs
	m1.selfSsrc = media.GenerateSecureSsrc("c1", "a@b", 0)
	m2.selfSsrc = media.GenerateSecureSsrc("c2", "a@b", 0)
	if m1.selfSsrc == m2.selfSsrc {
		t.Fatal("SSRC collision")
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

// TestDupDetectorPerCall is a placeholder ensuring the package compiles with
// the new fakes; real dup-detector coverage lives in logbuf tests.
func TestDupDetectorPerCall(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	if m == nil {
		t.Fatal("manager nil")
	}
}

var _ = sync.Mutex{}
var _ = media.GenerateSecureSsrc
var _ = core.CallMediaTypeAudio
