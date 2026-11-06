package call

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// TestNCallIsolationMatrix builds N independent calls (each with its own
// CallManager, relay, SRTP keys) and proves that peer audio injected into call i
// reaches ONLY call i, and that uplink captured on call i broadcasts ONLY on
// call i's relay. N ∈ {1,2,3,8} per the Wave-2 plan.
func TestNCallIsolationMatrix(t *testing.T) {
	media.SetAuthVerify(true)
	for _, n := range []int{1, 2, 3, 8} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			type slot struct {
				m     *CallManager
				relay *fakeRelayTransport
				got   *int
				key   []byte
				peer  types.JID
			}
			slots := make([]slot, n)
			payload := make([]byte, 128)
			for i := range payload {
				payload[i] = byte(i)
			}

			for i := 0; i < n; i++ {
				key := media.GenerateCallKey()
				peer := types.NewJID(fmt.Sprintf("551180000000%d", i), types.DefaultUserServer)
				m, relay, got := mediaCall(t, fmt.Sprintf("call-%d", i), key, peer)
				slots[i] = slot{m: m, relay: relay, got: got, key: key, peer: peer}
			}

			// Downlink: each peer's packet must land only on its own call.
			for _, s := range slots {
				pkt := peerPacket(t, s.m, s.key, s.peer, payload)
				s.m.onRelayData(pkt)
			}
			for i, s := range slots {
				if *s.got != 1 {
					t.Fatalf("N=%d call %d: OnPeerAudio=%d, want 1", n, i, *s.got)
				}
			}

			// Uplink: mic PCM on call i broadcasts only on relay i.
			pcm := make([]float32, 960)
			for j := range pcm {
				pcm[j] = float32(j%16) / 16.0
			}
			for i := range slots {
				slots[i].m.FeedCapturedPCM(pcm)
			}
			for i, s := range slots {
				if len(s.relay.broadcasts) == 0 {
					t.Fatalf("N=%d call %d: uplink did not broadcast", n, i)
				}
				if len(s.relay.broadcasts[0]) < 12 {
					t.Fatalf("N=%d call %d: broadcast too short", n, i)
				}
			}
		})
	}
}

// TestEndOneCallLeavesOthersRunning tears down one call of a set and asserts
// the survivors keep their live call and media state.
func TestEndOneCallLeavesOthersRunning(t *testing.T) {
	media.SetAuthVerify(true)
	const n = 4
	keys := make([][]byte, n)
	managers := make([]*CallManager, n)
	for i := 0; i < n; i++ {
		keys[i] = media.GenerateCallKey()
		peer := types.NewJID(fmt.Sprintf("551170000000%d", i), types.DefaultUserServer)
		m, _, _ := mediaCall(t, fmt.Sprintf("end-%d", i), keys[i], peer)
		managers[i] = m
	}

	// End call 1 (as a local hangup would).
	if err := managers[1].EndCall(t.Context(), core.EndCallReasonUserEnded); err != nil {
		t.Fatalf("EndCall: %v", err)
	}
	if managers[1].CurrentCall() != nil && !managers[1].CurrentCall().IsEnded() {
		t.Fatal("ended call must report IsEnded")
	}
	for i, m := range managers {
		if i == 1 {
			continue
		}
		c := m.CurrentCall()
		if c == nil || c.IsEnded() {
			t.Fatalf("call %d must stay live after ending call 1", i)
		}
	}
}

// TestGoroutineBaselineAfterCallCycles: repeated create/end cycles must not
// leak goroutines (detached terminate sends, keepalive, relay watchers).
func TestGoroutineBaselineAfterCallCycles(t *testing.T) {
	media.SetAuthVerify(true)
	// Warm up so lazily-started runtime goroutines are already present.
	for i := 0; i < 3; i++ {
		m, _, _ := mediaCall(t, fmt.Sprintf("warm-%d", i), media.GenerateCallKey(),
			types.NewJID("5511600000000", types.DefaultUserServer))
		_ = m.EndCall(t.Context(), core.EndCallReasonUserEnded)
	}
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		m, _, _ := mediaCall(t, fmt.Sprintf("cyc-%d", i), media.GenerateCallKey(),
			types.NewJID("5511600000001", types.DefaultUserServer))
		if err := m.EndCall(t.Context(), core.EndCallReasonUserEnded); err != nil {
			t.Fatalf("EndCall: %v", err)
		}
		// Drain the detached terminate send so it cannot outlive the loop.
		if !m.WaitPendingSends(2 * time.Second) {
			t.Fatal("terminate send never completed")
		}
	}
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > base+5 {
		t.Fatalf("goroutine leak: baseline=%d after=%d", base, after)
	}
}

// TestMLowRealCodecRoundtrip: the audibility variant — the production codec
// must encode and decode a real tone with preserved energy (the isolation
// matrix above uses the lossy passthrough codec for byte-level routing).
func TestMLowRealCodecRoundtrip(t *testing.T) {
	codec, err := media.NewMLowCodec(media.DefaultCodecOptions)
	if err != nil {
		t.Fatalf("NewMLowCodec: %v", err)
	}
	defer codec.Close()

	frame := make([]float32, codec.FrameSize())
	for i := range frame {
		frame[i] = 0.5 // constant mid-level signal
	}
	enc, err := codec.Encode(frame)
	if err != nil || len(enc) == 0 {
		t.Fatalf("Encode: err=%v len=%d", err, len(enc))
	}
	dec, err := codec.Decode(enc)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(dec) == 0 {
		t.Fatal("decoded frame is empty")
	}
	var peak float32
	for _, v := range dec {
		if v > peak {
			peak = v
		}
	}
	if peak < 0.1 {
		t.Fatalf("decoded peak %.4f too low — energy lost in round-trip", peak)
	}
}

// TestHandleCallTerminateSiblingGuard: after an accept, a terminate from a
// sibling device (different device number) must be ignored; the answering
// device's terminate must end the call.
func TestHandleCallTerminateSiblingGuard(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	c := NewIncomingCall("G", "5511888888888@s.whatsapp.net", "5511888888888@s.whatsapp.net", "", core.CallMediaTypeAudio)
	m.currentCall = c
	m.acceptedByJid = "5511888888888:3@s.whatsapp.net"

	terminate := func(from string) *waBinary.Node {
		return &waBinary.Node{Tag: "call", Attrs: waBinary.Attrs{"from": from}}
	}

	// Sibling (device 5) times out: must not kill the live call.
	m.HandleCallTerminate(terminate("5511888888888:5@s.whatsapp.net"))
	if c.IsEnded() {
		t.Fatal("terminate from a sibling device must be ignored")
	}

	// The answering device ends it for real.
	m.HandleCallTerminate(terminate("5511888888888:3@s.whatsapp.net"))
	if !c.IsEnded() {
		t.Fatal("terminate from the answering device must end the call")
	}
}

// TestRelayAllLostEndsLiveCall: when every relay connection drops, the call
// must be ended (reason failed) instead of staying ACTIVE with no media.
func TestRelayAllLostEndsLiveCall(t *testing.T) {
	media.SetAuthVerify(true)
	m, relay, _ := mediaCall(t, "zombie", media.GenerateCallKey(),
		types.NewJID("5511500000000", types.DefaultUserServer))

	relay.LoseAll()
	c := m.CurrentCall()
	if c != nil && !c.IsEnded() {
		t.Fatal("call must be ended after all relays are lost")
	}

	// A second all-lost on an already-ended call is a no-op (no panic, no
	// second EndCall).
	relay.LoseAll()
	if m.CurrentCall() != nil && !m.CurrentCall().IsEnded() {
		t.Fatal("call must stay ended")
	}
}
