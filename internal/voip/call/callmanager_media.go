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
	if !m.actualPeerSet {
		m.actualPeerSet = true
		if !containsSsrc(m.peerSsrcs, ssrc) {
			m.peerSsrcs = []uint32{ssrc}
			m.relay.SetSubscriptionSsrc(ssrc)
			go m.relay.ResendSubscriptions()
		}
	}
	srtp := m.srtpSession
	codec := m.codec
	callID := m.callIDLocked()
	m.mu.Unlock()

	pkt, err := srtp.Unprotect(data)
	if err != nil {
		m.log.Debug("srtp unprotect error", "err", err)
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
	pcm, err := codec.Decode(pkt.Payload)
	if err != nil {
		if logbuf.Enabled() && logbuf.Allow("decoder:"+callID, 5) {
			logbuf.LogEvent("decoder", logbuf.LevelError, "opus decode failed (frame dropped)", map[string]any{
				"call_id":     callID,
				"err":         err.Error(),
				"payload_len": len(pkt.Payload),
			})
		}
		return
	}
	pcm = media.NormalizeFrame(pcm, codec.FrameSize())
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
	if m.OnPeerAudio != nil {
		m.OnPeerAudio(pcm)
	}
}
