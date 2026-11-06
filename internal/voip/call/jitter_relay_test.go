package call

import (
	"testing"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"

	"go.mau.fi/whatsmeow/types"
)

// jitterPeer wraps peer SRTP protection so tests can emit real packets with
// controlled sequence numbers.
type jitterPeer struct {
	t    *testing.T
	m    *CallManager
	key  []byte
	peer types.JID
	sess *media.SrtpSession
	rtp  *media.RtpSession
}

func newJitterPeer(t *testing.T, callID string, key []byte, peer types.JID) *jitterPeer {
	t.Helper()
	m, _, _ := mediaCall(t, callID, key, peer)
	peerDevice := ensureDeviceJid(peer.String())
	ourDevice := ensureDeviceJid(m.sock.OwnLID().String())
	peerSend, err := media.DerivePerJidSrtpKey(key, peerDevice)
	if err != nil {
		t.Fatal(err)
	}
	ourRecv, err := media.DerivePerJidSrtpKey(key, ourDevice)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := media.NewSrtpSession(peerSend, ourRecv, core.SRTPRecvAuthTagLen, core.SRTPSendAuthTagLen)
	if err != nil {
		t.Fatal(err)
	}
	return &jitterPeer{
		t: t, m: m, key: key, peer: peer,
		sess: sess,
		rtp:  media.NewWhatsAppOpusSession(m.peerSsrcs[0]),
	}
}

// packet builds the next RTP packet (sequence advances per call) with the given
// payload and returns the SRTP-protected bytes.
func (j *jitterPeer) packet(payload []byte) []byte {
	j.t.Helper()
	pkt := j.rtp.CreatePacket(payload, false)
	out, err := j.sess.Protect(pkt)
	if err != nil {
		j.t.Fatalf("protect: %v", err)
	}
	return out
}

// firstSample returns the first decoded sample of a 2-byte payload so tests can
// tell frames apart.
func firstSample(payload []byte) float32 {
	v := int16(payload[0]) | int16(payload[1])<<8
	return float32(v) / 32767.0
}

// TestJitterReordersInOnRelayData delivers real SRTP packets out of order
// (N, N+2, N+1) and asserts OnPeerAudio fires in sequence order: the jitter
// buffer must hold N+2 until N+1 arrives, then release both in order.
func TestJitterReordersInOnRelayData(t *testing.T) {
	media.SetAuthVerify(true)
	key := media.GenerateCallKey()
	peer := types.NewJID("5511888800001", types.DefaultUserServer)
	j := newJitterPeer(t, "call-jit-order", key, peer)

	payA := []byte{0x01, 0x00}
	payB := []byte{0x02, 0x00}
	payC := []byte{0x03, 0x00}
	pktA := j.packet(payA) // seq N
	pktB := j.packet(payB) // seq N+1
	pktC := j.packet(payC) // seq N+2

	var firsts []float32
	j.m.OnPeerAudio = func(pcm []float32) {
		if len(pcm) > 0 {
			firsts = append(firsts, pcm[0])
		}
	}

	// Deliver out of order: A, C, B.
	j.m.onRelayData(pktA)
	if len(firsts) != 1 {
		t.Fatalf("after A: OnPeerAudio=%d want 1 (A released immediately)", len(firsts))
	}
	j.m.onRelayData(pktC)
	if len(firsts) != 1 {
		t.Fatalf("after C: OnPeerAudio=%d want 1 (C held for missing N+1)", len(firsts))
	}
	j.m.onRelayData(pktB)
	if len(firsts) != 3 {
		t.Fatalf("after B: OnPeerAudio=%d want 3 (B then C released in order)", len(firsts))
	}
	want := []float32{firstSample(payA), firstSample(payB), firstSample(payC)}
	for i, w := range want {
		if firsts[i] != w {
			t.Fatalf("frame %d first sample = %v want %v (order must be sequence order)", i, firsts[i], w)
		}
	}
}

// TestJitterConcealsLossInOnRelayData drops one sequence number and proves the
// hole is concealed after jitterDepth newer packets instead of stalling the
// playout: every subsequent frame still reaches OnPeerAudio in order.
func TestJitterConcealsLossInOnRelayData(t *testing.T) {
	media.SetAuthVerify(true)
	key := media.GenerateCallKey()
	peer := types.NewJID("5511888800002", types.DefaultUserServer)
	j := newJitterPeer(t, "call-jit-loss", key, peer)

	// 4-byte payloads decode to two distinct samples so the concealment fade
	// (second sample decays) is distinguishable from a plain repeat.
	payA := []byte{0x01, 0x00, 0x10, 0x00}
	payC := []byte{0x03, 0x00, 0x30, 0x00}
	payD := []byte{0x04, 0x00, 0x40, 0x00}
	payE := []byte{0x05, 0x00, 0x50, 0x00}
	pktA := j.packet(payA)           // seq N
	_ = j.packet([]byte{0x00, 0x00}) // seq N+1 never delivered (lost)
	pktC := j.packet(payC)           // seq N+2
	pktD := j.packet(payD)           // seq N+3
	pktE := j.packet(payE)           // seq N+4

	var frames [][]float32
	j.m.OnPeerAudio = func(pcm []float32) {
		frames = append(frames, pcm)
	}

	j.m.onRelayData(pktA)
	j.m.onRelayData(pktC)
	j.m.onRelayData(pktD)
	j.m.onRelayData(pktE)

	// A, concealment for N+1, C, D, E.
	if len(frames) != 5 {
		t.Fatalf("OnPeerAudio=%d want 5 (A, concealed, C, D, E)", len(frames))
	}
	if frames[0][0] != firstSample(payA) {
		t.Fatalf("frame 0 = %v want A", frames[0][0])
	}
	// The concealed frame starts at A's amplitude and fades the second sample
	// down — a plain repeat of A would keep it unchanged.
	if frames[1][0] != frames[0][0] {
		t.Fatalf("concealment must start at the last frame amplitude, got %v want %v", frames[1][0], frames[0][0])
	}
	if !(frames[1][1] < frames[0][1]) {
		t.Fatalf("concealment must fade the last frame, second sample %v not below %v", frames[1][1], frames[0][1])
	}
	want := []float32{firstSample(payC), firstSample(payD), firstSample(payE)}
	for i, w := range want {
		if frames[i+2][0] != w {
			t.Fatalf("frame %d = %v want %v", i+2, frames[i+2][0], w)
		}
	}
}
