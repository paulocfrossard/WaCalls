package call

import (
	"time"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/logbuf"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"
)

// callIDLocked returns the current call id (empty when no call is set up).
// Callers must hold m.mu.
func (m *CallManager) callIDLocked() string {
	if m.currentCall != nil {
		return m.currentCall.CallID
	}
	return ""
}

func (m *CallManager) initCodec() {
	if m.codec != nil {
		return
	}
	codec, err := media.NewMLowCodec(media.DefaultCodecOptions)
	if err != nil {
		m.log.Warn("MLow codec unavailable — call will run signaling-only (no audio)", "err", err)
		return
	}
	m.codec = codec
}

func (m *CallManager) FeedCapturedPCM(data []float32) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.codec == nil || m.rtpSession == nil || m.srtpSession == nil || !m.relay.HasConnection() {
		return
	}
	m.lastCaptureAt = time.Now()
	frameSize := m.codec.FrameSize()
	if m.encodeBuf == nil {
		m.encodeBuf = make([]float32, frameSize)
		m.encodeBufPos = 0
	}
	callID := m.callIDLocked()

	offset := 0
	for offset < len(data) {
		toCopy := min(len(data)-offset, frameSize-m.encodeBufPos)
		copy(m.encodeBuf[m.encodeBufPos:], data[offset:offset+toCopy])
		m.encodeBufPos += toCopy
		offset += toCopy
		if m.encodeBufPos < frameSize {
			break
		}
		frame := make([]float32, frameSize)
		copy(frame, m.encodeBuf)
		m.encodeBufPos = 0

		frameStart := time.Now()
		opus, err := m.codec.Encode(frame)
		if err != nil {
			m.log.Debug("encode error", "err", err)
			if logbuf.Enabled() && logbuf.Allow("encoder:"+callID, 5) {
				logbuf.LogEvent("encoder", logbuf.LevelError, "opus encode failed", map[string]any{
					"call_id": callID,
					"err":     err.Error(),
					"samples": frameSize,
				})
			}
			continue
		}
		m.sendOpusFrameLocked(opus)
		// Rate-limited ~5/s: 125 msg/s would refill the whole ring in 1s.
		if logbuf.Enabled() && logbuf.Allow("uplink-frame:"+callID, 5) {
			fields := map[string]any{
				"call_id":       callID,
				"len":           len(data),
				"frame_samples": frameSize,
			}
			if d := time.Since(frameStart); d > 10*time.Millisecond {
				fields["broadcast_ms"] = logbuf.Round4(float64(d.Microseconds()) / 1000.0)
			}
			logbuf.LogEvent("uplink-frame", logbuf.LevelDebug, "full frame encoded+protected+broadcast", fields)
		}
	}
}

func (m *CallManager) sendOpusFrameLocked(opus []byte) {
	if m.rtpSession == nil || m.srtpSession == nil {
		return
	}
	marker := !m.firstPacketSent
	pkt := m.rtpSession.CreatePacketWithDuration(opus, m.codec.FrameSize(), marker)
	if m.debeEnabled {
		pkt.Header.Extension = true
		pkt.Header.ExtensionProfile = 0xbede
		pkt.Header.ExtensionData = nil
	}
	m.firstPacketSent = true

	srtp, err := m.srtpSession.Protect(pkt)
	if err != nil {
		m.log.Debug("srtp protect error", "err", err)
		// Errors only in the ring (Protect is the only failing step;
		// Broadcast has no error return).
		logbuf.LogEvent("srtp", logbuf.LevelError, "srtp protect failed (frame dropped)", map[string]any{
			"call_id": m.callIDLocked(),
			"ssrc":    m.selfSsrc,
			"err":     err.Error(),
			"len":     len(pkt.Payload),
		})
		return
	}
	m.relay.Broadcast(srtp)
}

func (m *CallManager) startSilenceKeepaliveLocked() {
	if m.keepaliveStop != nil || m.codec == nil {
		return
	}
	stop := make(chan struct{})
	m.keepaliveStop = stop
	frameSize := m.codec.FrameSize()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("panic in silence keepalive", "panic", r)
			}
		}()
		ticker := time.NewTicker(60 * time.Millisecond)
		defer ticker.Stop()
		silence := make([]float32, frameSize)
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				m.mu.Lock()
				ready := m.codec != nil && m.rtpSession != nil && m.srtpSession != nil && m.relay.HasConnection()
				idle := time.Since(m.lastCaptureAt) > 120*time.Millisecond
				if ready && idle {
					if opus, err := m.codec.Encode(silence); err == nil {
						m.sendOpusFrameLocked(opus)
					}
				}
				m.mu.Unlock()
			}
		}
	}()
}

func (m *CallManager) onRelayData(data []byte) {
	if transport.IsStunPacket(data) {
		return
	}
	if !transport.IsRtpPacket(data) {
		return
	}
	if len(data) < 12 {
		return
	}
	pt := data[1] & 0x7f
	if pt != core.PayloadTypeWhatsAppOpus {
		return
	}

	m.mu.Lock()
	if m.srtpSession == nil || m.codec == nil {
		m.mu.Unlock()
		return
	}
	ssrc := uint32(data[8])<<24 | uint32(data[9])<<16 | uint32(data[10])<<8 | uint32(data[11])
	seq := uint16(data[2])<<8 | uint16(data[3])
	if ssrc == m.selfSsrc {
		callID := m.callIDLocked()
		selfSsrc := m.selfSsrc
		m.mu.Unlock()
		// Logged for EVERY discard: seeing our own SSRC come back from the
		// relay is the smoking gun for an echo/loop ("cliente se ouvindo").
		logbuf.LogEvent("relay-filter", logbuf.LevelInfo, "dropped relay packet with own SSRC (echo/loop candidate)", map[string]any{
			"call_id":   callID,
			"ssrc":      ssrc,
			"self_ssrc": selfSsrc,
			"len":       len(data),
		})
		return
	}
	srtp := m.srtpSession
	codec := m.codec
	callID := m.callIDLocked()
	m.mu.Unlock()

	pkt, err := srtp.Unprotect(data)
	if err != nil {
		m.log.Debug("srtp unprotect error", "err", err)
		m.srtpErrors.Add(1)
		if logbuf.Enabled() && logbuf.Allow("srtp-unprotect:"+callID, 5) {
			logbuf.LogEvent("srtp", logbuf.LevelError, "srtp unprotect failed (packet dropped)", map[string]any{
				"call_id": callID,
				"err":     err.Error(),
				"len":     len(data),
			})
		}
		return
	}
	if len(pkt.Payload) == 0 {
		return
	}
	// Dedup and subscription bootstrap only after the packet authenticated:
	// RTP-shaped bytes with a spoofed SSRC must never redirect the subscription
	// or shadow a genuine packet in the dedup window.
	m.mu.Lock()
	if m.isDuplicateRTPPacketLocked(ssrc, seq) {
		m.mu.Unlock()
		m.dupDropped.Add(1)
		// The relay fan-out delivers the same packet on every connected relay;
		// forwarding it twice doubles the downlink and trips the dup-detector.
		if logbuf.Enabled() && logbuf.Allow("relay-dup:"+callID, 5) {
			logbuf.LogEvent("relay-filter", logbuf.LevelDebug, "duplicate relay packet dropped", map[string]any{
				"call_id": callID,
				"ssrc":    ssrc,
				"seq":     seq,
			})
		}
		return
	}
	if !m.actualPeerSet {
		m.actualPeerSet = true
		if !containsSsrc(m.peerSsrcs, ssrc) {
			m.peerSsrcs = []uint32{ssrc}
			m.relay.SetSubscriptionSsrc(ssrc)
			go m.relay.ResendSubscriptions()
		}
	}
	// Reorder by RTP sequence so the stateful codec decodes frames in
	// production order; frames missing for jitterDepth newer packets are
	// concealed instead of leaving a hole in the playout.
	//
	// The push AND the decode loop deliberately stay inside m.mu: releasing
	// the lock between them would let a second relay goroutine decode its
	// released frames before this one (breaking both the sequence order the
	// jitter buffer just restored and the non-thread-safe codec), and the
	// lock is only held for sub-millisecond decode work per frame.
	if m.jitter == nil {
		m.jitter = newJitterBuffer(jitterDepth)
	}
	var out [][]float32
	for _, fr := range m.jitter.push(seq, pkt.Payload) {
		if !fr.present {
			if c := m.concealLocked(); c != nil {
				out = append(out, c)
			}
			continue
		}
		pcm, err := codec.Decode(fr.payload)
		if err != nil || len(pcm) == 0 {
			if err != nil && logbuf.Enabled() && logbuf.Allow("decoder:"+callID, 5) {
				logbuf.LogEvent("decoder", logbuf.LevelError, "opus decode failed (frame dropped)", map[string]any{
					"call_id":     callID,
					"err":         err.Error(),
					"payload_len": len(fr.payload),
				})
			}
			// A present-but-undecodable frame still leaves a hole in the
			// playout: conceal it exactly like a network loss.
			if c := m.concealLocked(); c != nil {
				out = append(out, c)
			}
			continue
		}
		pcm = media.NormalizeFrame(pcm, codec.FrameSize())
		m.lastFrame = pcm
		m.concealed = 0
		m.framesDecoded.Add(1)
		m.lastDownlink.Store(time.Now().UnixMilli())
		// Rate-limited 5/s: len/peak show whether dead air or hot audio follows.
		if logbuf.Enabled() && logbuf.Allow("downlink-decode:"+callID, 5) {
			rms, peak := logbuf.PCMStats(pcm)
			logbuf.LogEvent("downlink-decode", logbuf.LevelDebug, "peer frame decoded", map[string]any{
				"call_id": callID,
				"samples": len(pcm),
				"rms":     logbuf.Round4(rms),
				"peak":    logbuf.Round4(peak),
			})
		}
		out = append(out, pcm)
	}
	m.emitPeerAudioLocked(out)
}

// emitPeerAudioLocked hands decoded frames to OnPeerAudio in decode order and
// returns with m.mu released (no CallManager lock held on return).
//
// Caller holds m.mu — the jitter push + decode loop above ran under it.
// emitMu is taken BEFORE m.mu is released: a second relay goroutine must not
// decode and emit its newer frames between our unlock and our emission, or
// the browser receives N+1 before N — one 60ms segment out of order is an
// audible click. Lock order is always m.mu → emitMu; never acquire them in
// the reverse order (emitMu is taken at no other site, so no cycle exists).
//
// m.mu is deliberately NOT held while the callback runs: OnPeerAudio →
// session callback → registry → bridge WritePCM (datachannel Send) can apply
// backpressure, which must never block call state. cleanupMedia/endCall take
// m.mu but not emitMu, so a concurrent emission neither blocks them nor is
// blocked by them; they simply wait for m.mu, which this function releases
// before the first frame goes out.
//
// OnPeerAudio read (L4): it is assigned exactly once, in cmd/server/session.go
// wireCall, immediately after NewCallManager and before the manager is
// published to the registry or any relay can deliver data (connectRelays →
// ConfigureRelays run later, from signaling). Nothing re-assigns it on a live
// manager, so this is a snapshot of a stable func value — no lock protects
// the field, and none is needed. The nil-check stays.
func (m *CallManager) emitPeerAudioLocked(out [][]float32) {
	if len(out) == 0 {
		m.mu.Unlock()
		return
	}
	m.emitMu.Lock()
	onPeer := m.OnPeerAudio
	m.mu.Unlock()
	// defer: a panic inside the callback (recovered by the relay layer) must
	// not leave emitMu locked forever and wedge every later emission.
	defer m.emitMu.Unlock()
	if onPeer == nil {
		return
	}
	for _, pcm := range out {
		onPeer(pcm)
	}
}

// concealLocked produces a frame to cover a lost packet: the last decoded
// frame faded out once, then silence for any consecutive losses.
// Caller holds m.mu.
func (m *CallManager) concealLocked() []float32 {
	n := 960 // default 16 kHz frame
	if m.codec != nil {
		n = m.codec.FrameSize()
	}
	if len(m.lastFrame) > 0 {
		n = len(m.lastFrame)
	}
	pcm := make([]float32, n)
	if m.concealed == 0 && len(m.lastFrame) > 1 {
		last := len(pcm) - 1
		for i := range pcm {
			pcm[i] = m.lastFrame[i] * (1 - float32(i)/float32(last))
		}
	}
	m.concealed++
	return pcm
}
