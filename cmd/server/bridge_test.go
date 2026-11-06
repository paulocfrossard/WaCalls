package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// makeBrowserOffer simulates the browser: it opens the "pcm" data channel and
// returns the SDP offer. It also returns the peer connection so a test can
// finish the handshake and exchange PCM.
func makeBrowserOffer(t *testing.T) (*webrtc.PeerConnection, *webrtc.DataChannel, string) {
	t.Helper()
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}

	dc, err := pc.CreateDataChannel(pcmChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gather
	return pc, dc, pc.LocalDescription().SDP
}

func TestNewBridgeNegotiatesDataChannelOffer(t *testing.T) {
	pc, _, offer := makeBrowserOffer(t)
	defer pc.Close()

	br, answer, err := NewBridge(offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge failed: %v", err)
	}
	defer br.Close()

	if answer == "" || !strings.Contains(answer, "m=application") {
		t.Fatalf("answer missing data channel (application) m-line:\n%s", answer)
	}
	if strings.Contains(answer, "m=audio") {
		t.Fatalf("answer should not negotiate an audio m-line anymore:\n%s", answer)
	}
}

// TestBridgePCMRoundtrip connects the simulated browser to the bridge and checks
// that PCM sent on the data channel surfaces as float32 via OnBrowserPCM.
func TestBridgePCMRoundtrip(t *testing.T) {
	pc, dc, offer := makeBrowserOffer(t)
	defer pc.Close()

	got := make(chan []float32, 1)
	br, answer, err := NewBridge(offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()
	br.SetOnBrowserPCM(func(pcm []float32) { got <- pcm })

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}

	dc.OnOpen(func() {
		// 0.5 and -0.5 in Int16 LE.
		_ = dc.Send([]byte{0x00, 0x40, 0x00, 0xC0})
	})

	select {
	case pcm := <-got:
		if len(pcm) != 2 {
			t.Fatalf("expected 2 samples, got %d", len(pcm))
		}
		if pcm[0] < 0.4 || pcm[0] > 0.6 || pcm[1] > -0.4 || pcm[1] < -0.6 {
			t.Fatalf("unexpected samples: %v", pcm)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for PCM over data channel")
	}
}

func TestBridgeDetachDropsCallbacks(t *testing.T) {
	br := &Bridge{}
	br.SetOnBrowserPCM(func([]float32) {})
	br.SetOnTerminalICE(func() {})
	br.Detach()
	if br.browserPCM() != nil {
		t.Fatal("Detach must drop OnBrowserPCM")
	}
	if br.terminalICE() != nil {
		t.Fatal("Detach must drop OnTerminalICE")
	}
}

// TestBridgeDatachannelCloseFiresTerminal: a clean browser teardown (tab
// close / unload) closes the pcm datachannel; the bridge must fire the
// terminal callback — the grace-window trigger — without waiting for the
// ~30s ICE disconnected/failed timeouts.
func TestBridgeDatachannelCloseFiresTerminal(t *testing.T) {
	pc, dc, offer := makeBrowserOffer(t)
	defer pc.Close()

	br, answer, err := NewBridge(offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()

	terminal := make(chan struct{}, 1)
	br.SetOnTerminalICE(func() { terminal <- struct{}{} })

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}

	opened := make(chan struct{}, 1)
	dc.OnOpen(func() { opened <- struct{}{} })
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the browser datachannel to open")
	}

	// Browser side goes away cleanly — same signal a tab close produces.
	pc.Close()

	select {
	case <-terminal:
	case <-time.After(15 * time.Second):
		t.Fatal("datachannel close must fire the terminal callback")
	}
}

// TestBridgeDetachedCloseDoesNotFireTerminal: Detach() runs before any Close
// on a superseded/removed bridge, so its own teardown must not arm grace.
func TestBridgeDetachedCloseDoesNotFireTerminal(t *testing.T) {
	pc, dc, offer := makeBrowserOffer(t)
	defer pc.Close()

	br, answer, err := NewBridge(offer, slog.Default())
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	defer br.Close()

	terminal := make(chan struct{}, 1)
	br.SetOnTerminalICE(func() { terminal <- struct{}{} })

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("browser SetRemoteDescription: %v", err)
	}
	opened := make(chan struct{}, 1)
	dc.OnOpen(func() { opened <- struct{}{} })
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the browser datachannel to open")
	}

	br.Detach() // supersede first: clears onTerminalICE
	pc.Close()

	select {
	case <-terminal:
		t.Fatal("a Detached bridge must not fire the terminal callback")
	case <-time.After(2 * time.Second):
	}
}
