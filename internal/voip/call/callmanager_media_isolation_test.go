package call

import (
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"

	"go.mau.fi/whatsmeow/types"
)

// mediaCall builds a CallManager with a real SRTP session and a peer SSRC so
// tests can inject real SRTP-protected peer audio.
func mediaCall(t *testing.T, callID string, callKey []byte, peer types.JID) (*CallManager, *fakeRelayTransport, *int) {
	t.Helper()
	sock := newFakeSocket()
	relay := newFakeRelay()
	relay.connected = true
	m := NewCallManagerWithDeps(Deps{Sock: sock, Relay: relay, Codec: &passthroughCodec{}})

	ourDevice := ensureDeviceJid(sock.OwnLID().String())
	peerDevice := ensureDeviceJid(peer.String())

	c := NewIncomingCall(callID, peer.String(), peer.String(), "", core.CallMediaTypeAudio)
	c.EncryptionKey = callKey
	c.RelayData = &core.RelayData{ParticipantJids: []string{ourDevice, peerDevice}}
	m.currentCall = c
	m.selfSsrc = media.GenerateSecureSsrc(callID, ourDevice, 0)
	m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, peerDevice, 0)}
	m.rtpSession = media.NewWhatsAppOpusSession(m.selfSsrc)
	m.mu.Lock()
	m.initSrtpKeysLocked()
	m.mu.Unlock()
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
	peerDevice := ensureDeviceJid(peer.String())
	ourDevice := ensureDeviceJid(m.sock.OwnLID().String())
	peerSend, err := media.DerivePerJidSrtpKey(callKey, peerDevice)
	if err != nil {
		t.Fatal(err)
	}
	ourRecv, err := media.DerivePerJidSrtpKey(callKey, ourDevice)
	if err != nil {
		t.Fatal(err)
	}
	// Peer's send == manager's recv; the other direction is irrelevant here.
	sess, err := media.NewSrtpSession(peerSend, ourRecv, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
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
	if len(relayA.broadcasts[0]) < 12 {
		t.Fatalf("uplink broadcast too short to be RTP/SRTP: %d bytes", len(relayA.broadcasts[0]))
	}
	// The broadcast must carry call A's own SSRC in the RTP header.
	gotSsrc := uint32(relayA.broadcasts[0][8])<<24 | uint32(relayA.broadcasts[0][9])<<16 | uint32(relayA.broadcasts[0][10])<<8 | uint32(relayA.broadcasts[0][11])
	if gotSsrc != mA.selfSsrc {
		t.Fatalf("uplink SSRC=%x want %x", gotSsrc, mA.selfSsrc)
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
