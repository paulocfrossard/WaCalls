package main

import (
	"log/slog"
	"sync/atomic"

	"wacalls/internal/voip/logbuf"
	"wacalls/internal/voip/media"

	"github.com/pion/webrtc/v4"
)

// pcmChannelLabel is the data channel the browser opens to carry raw 16 kHz mono
// Int16 LE PCM in both directions. The browser side must create it with this label.
const pcmChannelLabel = "pcm"

// debugSampleRate: only 1 in N frames runs the debug logging/stats path.
const debugSampleRate = 16

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

	// debug sampling counters: debug logging/stats run only on 1 in
	// debugSampleRate frames to keep per-frame overhead out of the media path.
	uplinkCounter   atomic.Uint64
	downlinkCounter atomic.Uint64

	// OnBrowserPCM is invoked with decoded 16 kHz mono PCM captured from the browser mic.
	OnBrowserPCM func(pcm []float32)
	// OnTerminalICE fires when the peer connection fails or closes.
	OnTerminalICE func()
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
			if cb := br.OnBrowserPCM; cb != nil && len(msg.Data) > 0 {
				// Debug logging is sampled (1 in debugSampleRate frames) to
				// keep the mutex/rate-limiter off the per-frame media path.
				if logbuf.Enabled() {
					if n := br.uplinkCounter.Add(1); n%debugSampleRate == 0 {
						if logbuf.Allow("bridge-uplink:"+br.callID, 5) {
							logbuf.LogEvent("bridge-uplink", logbuf.LevelDebug, "uplink datachannel message from browser", map[string]any{
								"call_id": br.callID,
								"len":     len(msg.Data),
							})
						}
					}
				}
				cb(media.PCMInt16LEToFloat32(msg.Data))
			}
		})
	})

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Debug("browser ice state", "state", s.String())
		if s == webrtc.ICEConnectionStateFailed || s == webrtc.ICEConnectionStateClosed {
			if br.OnTerminalICE != nil {
				br.OnTerminalICE()
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
		// Sampled (1 in debugSampleRate frames) to keep RecordWritePCM,
		// the rate-limiter mutex and the O(n) PCMStats off the media path.
		if n := b.downlinkCounter.Add(1); n%debugSampleRate == 0 {
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
	}
	return dc.Send(media.PCMFloat32ToInt16LE(pcm))
}

func (b *Bridge) Close() {
	if b.pc != nil {
		_ = b.pc.Close()
	}
}
