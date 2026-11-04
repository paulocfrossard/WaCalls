package call

import (
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"

	"go.mau.fi/whatsmeow/types"
)

// deviceForm returns the device-suffixed form of a JID ("user:0@server").
func deviceForm(j types.JID) string {
	s := j.String()
	at := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return s
	}
	if i := indexByte(s[:at], ':'); i >= 0 {
		return s
	}
	return s[:at] + ":0" + s[at:]
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// mediaCall builds a CallManager with a real SRTP session and a peer SSRC so
// tests can inject real SRTP-protected peer audio.
func mediaCall(t *testing.T, callID string, callKey []byte, peer types.JID) (*CallManager, *fakeRelayTransport, *int) {
	t.Helper()
	sock := newFakeSocket()
	relay := newFakeRelay()
	relay.connected = true
	m := NewCallManagerWithDeps(Deps{Sock: sock, Relay: relay, Codec: &passthroughCodec{}})

	ourDevice := deviceForm(sock.OwnLID())
	peerDevice := deviceForm(peer)

	c := NewIncomingCall(callID, peer.String(), peer.String(), "", core.CallMediaTypeAudio)
	c.EncryptionKey = callKey
	c.RelayData = &core.RelayData{ParticipantJids: []string{ourDevice, peerDevice}}
	m.currentCall = c
	m.selfSsrc = media.GenerateSecureSsrc(callID, ourDevice, 0)
	m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, peerDevice, 0)}
	m.rtpSession = media.NewWhatsAppOpusSession(m.selfSsrc)
	m.initSrtpKeysLocked()
	if m.srtpSession == nil {
		t.Fatal("srtp session not derived")
	}
	got := 0
	m.OnPeerAudio = func(pcm []float32) { got++ }
	return m, relay, &got
}

// peerPacket builds a real SRTP packet as the peer would send it to m.
func peerPacket(t *testing.T, m *CallManager, callKey []byte, peer types.JID, payload []byte) []byte {
	t.Helper()
	peerDevice := deviceForm(peer)
	ourDevice := deviceForm(m.sock.OwnLID())
	peerSend, err := media.DerivePerJidSrtpKey(callKey, peerDevice)
	if err != nil {
		t.Fatal(err)
	}
	ourRecv, err := media.DerivePerJidSrtpKey(callKey, ourDevice)
	if err != nil {
		t.Fatal(err)
	}
	// Peer's send == manager's recv; the other direction is irrelevant here.
	sess, err := media.NewSrtpSession(peerSend, ourRecv, core.SRTPSendAuthTagLen, core.SRTPRecvAuthTagLen)
	if err != nil {
		t.Fatal(err)
	}
	rtp := media.NewWhatsAppOpusSession(m.peerSsrcs[0])
	pkt := rtp.CreatePacket(payload, false)
	out, err := sess.Protect(pkt)
	if err != nil {
		t.Fatalf("peer protect: %v", err)
	}
	return out
}

// TestMediaDownlinkIsolationTwoCalls proves a peer packet for call A reaches
// only call A's OnPeerAudio, never call B's (real SRTP keys).
func TestMediaDownlinkIsolationTwoCalls(t *testing.T) {
	media.SetAuthVerify(true)
	keyA := media.GenerateCallKey()
	keyB := media.GenerateCallKey()
	peerA := types.NewJID("5511888888888", types.DefaultUserServer)
	peerB := types.NewJID("5511777777777", types.DefaultUserServer)

	mA, _, gotA := mediaCall(t, "call-A", keyA, peerA)
	mB, _, gotB := mediaCall(t, "call-B", keyB, peerB)

	payload := make([]byte, 128)
	for i := range payload {
		payload[i] = byte(i)
	}
	pktA := peerPacket(t, mA, keyA, peerA, payload)

	mA.onRelayData(pktA)
	if *gotA != 1 {
		t.Fatalf("call A downlink: OnPeerAudio=%d want 1", *gotA)
	}

	// The same bytes delivered to B must fail SRTP auth (different keys) and
	// never reach B's OnPeerAudio.
	mB.onRelayData(pktA)
	if *gotB != 0 {
		t.Fatalf("call B must not receive call A audio, got %d", *gotB)
	}
}

// TestMediaUplinkIsolationTwoCalls proves PCM captured for call A is broadcast
// only on A's relay, never B's.
func TestMediaUplinkIsolationTwoCalls(t *testing.T) {
	media.SetAuthVerify(true)
	keyA := media.GenerateCallKey()
	keyB := media.GenerateCallKey()
	peerA := types.NewJID("5511888888888", types.DefaultUserServer)
	peerB := types.NewJID("5511777777777", types.DefaultUserServer)

	mA, relayA, _ := mediaCall(t, "call-A", keyA, peerA)
	_, relayB, _ := mediaCall(t, "call-B", keyB, peerB)

	pcm := make([]float32, 960)
	for i := range pcm {
		pcm[i] = float32(i%16) / 16.0
	}
	mA.FeedCapturedPCM(pcm)

	if len(relayA.broadcasts) == 0 {
		t.Fatal("call A should broadcast uplink audio")
	}
	if len(relayB.broadcasts) != 0 {
		t.Fatalf("call B must not broadcast call A uplink, got %d", len(relayB.broadcasts))
	}
}

// TestGenerateSecureSsrcDistinctPerCall guards the SSRC separation invariant.
func TestGenerateSecureSsrcDistinctPerCall(t *testing.T) {
	callA := media.GenerateSecureSsrc("call-A", "5511888888888:0@s.whatsapp.net", 0)
	callB := media.GenerateSecureSsrc("call-B", "5511888888888:0@s.whatsapp.net", 0)
	if callA == callB {
		t.Fatal("same JID on different calls must yield different SSRCs")
	}
}
