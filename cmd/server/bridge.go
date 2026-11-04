package main

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"wacalls/internal/voip/logbuf"
	"wacalls/internal/voip/media"

	"github.com/pion/webrtc/v4"
)

// pcmChannelLabel is the data channel the browser opens to carry raw 16 kHz mono
// Int16 LE PCM in both directions. The browser side must create it with this label.
const pcmChannelLabel = "pcm"

// Bridge is the browser-leg adapter: it carries raw PCM between the browser and
// the CallManager over a WebRTC data channel. The call core only ever sees
// []float32 PCM, so it stays unaware of the transport (no Opus here anymore).
type Bridge struct {
	pc  *webrtc.PeerConnection
	dc  atomic.Pointer[webrtc.DataChannel]
	log *slog.Logger

	// callID correlates diagnostics (set once via SetCallID right after
	// construction, before the bridge is published to the call registry).
	callID string

	// OnBrowserPCM is invoked with decoded 16 kHz mono PCM captured from the browser mic.
	// OnTerminalICE fires when the peer connection fails or closes.
	// Guarded by cbMu: setBridge detaches them while pion/data-channel
	// goroutines may still be reading, so both must be swapped under a lock.
	cbMu          sync.RWMutex
	onBrowserPCM  func(pcm []float32)
	onTerminalICE func()
}

// SetOnBrowserPCM installs the browser-PCM callback.
func (b *Bridge) SetOnBrowserPCM(fn func(pcm []float32)) {
	b.cbMu.Lock()
	b.onBrowserPCM = fn
	b.cbMu.Unlock()
}

// SetOnTerminalICE installs the terminal-ICE callback.
func (b *Bridge) SetOnTerminalICE(fn func()) {
	b.cbMu.Lock()
	b.onTerminalICE = fn
	b.cbMu.Unlock()
}

// Detach drops both callbacks. Must run before closing a replaced bridge so
// its ICE/PCM events cannot re-enter the call that now owns the callID.
func (b *Bridge) Detach() {
	b.cbMu.Lock()
	b.onBrowserPCM = nil
	b.onTerminalICE = nil
	b.cbMu.Unlock()
}

func (b *Bridge) browserPCM() func([]float32) {
	b.cbMu.RLock()
	defer b.cbMu.RUnlock()
	return b.onBrowserPCM
}

func (b *Bridge) terminalICE() func() {
	b.cbMu.RLock()
	defer b.cbMu.RUnlock()
	return b.onTerminalICE
}

// SetCallID records the call this bridge belongs to for debug-log events.
// Must be called before the bridge is shared (before Session.setBridge).
func (b *Bridge) SetCallID(id string) { b.callID = id }

func NewBridge(offerSDP string, log *slog.Logger) (*Bridge, string, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", err
	}
	br := &Bridge{pc: pc, log: log}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != pcmChannelLabel {
			return
		}
		br.dc.Store(dc)
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			defer func() {
				if r := recover(); r != nil {
					logbuf.LogEvent("bridge", logbuf.LevelError, "panic in browser pcm callback", map[string]any{
						"call_id": br.callID, "panic": r,
					})
				}
			}()
			cb := br.browserPCM()
			if cb != nil && len(msg.Data) > 0 {
				// Rate-limited 5/s: the mic pushes ~125 messages/s.
				if logbuf.Enabled() && logbuf.Allow("bridge-uplink:"+br.callID, 5) {
					logbuf.LogEvent("bridge-uplink", logbuf.LevelDebug, "uplink datachannel message from browser", map[string]any{
						"call_id": br.callID,
						"len":     len(msg.Data),
					})
				}
				cb(media.PCMInt16LEToFloat32(msg.Data))
			}
		})
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		defer func() {
			if r := recover(); r != nil {
				logbuf.LogEvent("bridge", logbuf.LevelError, "panic in terminal ice callback", map[string]any{
					"call_id": br.callID, "panic": r,
				})
			}
		}()
		log.Debug("browser ice state", "state", s.String())
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if cb := br.terminalICE(); cb != nil {
				cb()
			}
		}
	})

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offerSDP}); err != nil {
		pc.Close()
		return nil, "", err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, "", err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		pc.Close()
		return nil, "", err
	}
	<-gatherComplete

	return br, pc.LocalDescription().SDP, nil
}

// WritePCM sends 16 kHz mono float32 PCM to the browser as Int16 LE over the data
// channel. It is a no-op until the channel is open.
func (b *Bridge) WritePCM(pcm []float32) error {
	dc := b.dc.Load()
	if dc == nil || len(pcm) == 0 {
		return nil
	}
	if logbuf.Enabled() {
		// Feeds the 1s dup-detector: two WritePCM <50ms apart for the same
		// callID (healthy cadence is ~60ms) = duplicated audio path.
		logbuf.RecordWritePCM(b.callID)
		// Rate-limited 5/s with samples + peak for correlation.
		if logbuf.Allow("bridge-downlink:"+b.callID, 5) {
			_, peak := logbuf.PCMStats(pcm)
			logbuf.LogEvent("bridge-downlink", logbuf.LevelDebug, "write pcm downlink to browser", map[string]any{
				"call_id": b.callID,
				"samples": len(pcm),
				"peak":    logbuf.Round4(peak),
			})
		}
	}
	return dc.Send(media.PCMFloat32ToInt16LE(pcm))
}

func (b *Bridge) Close() {
	if b.pc != nil {
		_ = b.pc.Close()
	}
}
