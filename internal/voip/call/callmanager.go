package call

import (
	"context"
	"log/slog"
	"sync"
	"time"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/media"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/transport"
	"wacalls/internal/voip/wanode"

	"go.mau.fi/whatsmeow/types"
)

type CallManager struct {
	sock core.VoipSocket
	log  *slog.Logger

	mu          sync.Mutex
	currentCall *CallInfo

	rtpSession  *media.RtpSession
	srtpSession *media.SrtpSession
	codec       media.Codec
	relay       RelayTransport

	selfSsrc      uint32
	peerSsrcs     []uint32
	actualPeerSet bool

	firstPacketSent       bool
	initialTransportSent  bool
	outgoingPreacceptSent bool
	acceptedByJid         string
	acceptingCallID       string
	calleeDevices         []types.JID
	debeEnabled           bool

	encodeBuf    []float32
	encodeBufPos int

	lastCaptureAt time.Time
	keepaliveStop chan struct{}

	// now is injectable for tests (defaults to time.Now).
	now func() time.Time

	// seenRTP drops packets delivered by more than one relay connection
	// (the relay fan-out duplicates the peer stream). Keyed by ssrc<<16|seq.
	seenRTP      map[uint64]time.Time
	seenRTPPrune time.Time

	OnStateChange func(*CallInfo)
	OnIncoming    func(*CallInfo)
	OnEnded       func(*CallInfo)
	OnPeerAudio   func([]float32)
}

func NewCallManager(sock core.VoipSocket, log *slog.Logger) *CallManager {
	if log == nil {
		log = slog.Default()
	}
	m := &CallManager{
		sock:        sock,
		log:         log,
		debeEnabled: true,
	}
	relay := transport.NewSctpRelayManager(log)
	relay.SetOnConnected(func(ip string, port int) { m.onRelayConnected() })
	relay.SetOnReceive(func(data []byte) { m.onRelayData(data) })
	m.relay = relay
	return m
}

// Deps lets tests (and advanced callers) inject the relay transport, codec and
// clock instead of the production defaults.
type Deps struct {
	Sock  core.VoipSocket
	Log   *slog.Logger
	Relay RelayTransport
	Codec media.Codec
	Now   func() time.Time
}

// NewCallManagerWithDeps builds a CallManager from injected dependencies. A nil
// Relay/Codec/Log/Now falls back to the production default, so callers can
// override only what they need.
func NewCallManagerWithDeps(d Deps) *CallManager {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	m := &CallManager{
		sock:        d.Sock,
		log:         log,
		debeEnabled: true,
		now:         d.Now,
	}
	if d.Relay != nil {
		m.relay = d.Relay
		d.Relay.SetOnConnected(func(ip string, port int) { m.onRelayConnected() })
		d.Relay.SetOnReceive(func(data []byte) { m.onRelayData(data) })
	} else {
		relay := transport.NewSctpRelayManager(log)
		relay.SetOnConnected(func(ip string, port int) { m.onRelayConnected() })
		relay.SetOnReceive(func(data []byte) { m.onRelayData(data) })
		m.relay = relay
	}
	if d.Codec != nil {
		m.codec = d.Codec
	}
	return m
}

func (m *CallManager) nowTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func (m *CallManager) CurrentCall() *CallInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentCall
}

func (m *CallManager) emitState() {
	if m.OnStateChange != nil && m.currentCall != nil {
		m.OnStateChange(m.currentCall)
	}
}

func (m *CallManager) StartCall(ctx context.Context, callID string, peerJid types.JID, isVideo bool) error {
	m.mu.Lock()
	if m.currentCall != nil && !m.currentCall.IsEnded() {
		m.mu.Unlock()
		return &CallError{"a call is already in progress"}
	}

	mediaType := core.CallMediaTypeAudio
	if isVideo {
		mediaType = core.CallMediaTypeVideo
	}
	creator := m.sock.OwnLID()
	if creator.IsEmpty() {
		creator = m.sock.OwnPN()
	}
	resolved := m.sock.ResolveLIDForPN(ctx, peerJid)

	call := NewOutgoingCall(callID, resolved.String(), creator.String(), mediaType)
	callKey := media.GenerateCallKey()
	call.EncryptionKey = callKey
	m.currentCall = call
	m.acceptedByJid = ""
	m.initialTransportSent = false
	m.outgoingPreacceptSent = false

	selfJid := creator.String()
	m.selfSsrc = media.GenerateSecureSsrc(callID, selfJid, 0)
	m.rtpSession = media.NewWhatsAppOpusSession(m.selfSsrc)
	m.peerSsrcs = []uint32{media.GenerateSecureSsrc(callID, resolved.String(), 0)}
	m.initCodec()
	m.mu.Unlock()

	offer, calleeDevices, err := signaling.BuildOfferStanza(ctx, m.sock, callID, callKey, resolved, isVideo)
	if err != nil {
		m.abortStart()
		return err
	}
	ackNode, err := m.sock.Query(ctx, offer)
	if err != nil {
		m.abortStart()
		return err
	}

	m.mu.Lock()
	if m.currentCall != call || call.IsEnded() {
		m.mu.Unlock()
		return &CallError{"call ended or replaced before offer was sent"}
	}
	m.calleeDevices = calleeDevices
	_ = m.currentCall.ApplyTransition(Transition{Type: TransitionOfferSent})
	m.emitState()
	m.mu.Unlock()

	if ackNode != nil {
		go m.HandleCallAck(context.Background(), ackNode)
	}

	m.log.Info("call offer sent", "call_id", callID, "peer", resolved.String())
	return nil
}

// AlreadyAccepted reports whether the given id is an inbound call that has
// moved past ringing but is not ended (a retried accept is a no-op, not a
// failure that should tear the call down).
func (m *CallManager) AlreadyAccepted(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.currentCall
	return c != nil && c.CallID == callID &&
		c.Direction == core.CallDirectionIncoming &&
		!c.CanAccept() && !c.IsEnded()
}

func (m *CallManager) AcceptCall(ctx context.Context, callID string) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.CallID != callID {
		m.mu.Unlock()
		return &CallError{"no incoming call with id " + callID}
	}
	if !call.CanAccept() {
		m.mu.Unlock()
		return &CallError{"call cannot be accepted in state " + string(call.StateData.State)}
	}
	if m.acceptingCallID == callID {
		m.mu.Unlock()
		return &CallError{"accept already in progress"}
	}
	m.acceptingCallID = callID
	key := call.EncryptionKey
	peerJid := call.PeerJid
	creatorJid := call.CallCreator
	isVideo := call.MediaType == core.CallMediaTypeVideo
	relayData := call.RelayData
	m.mu.Unlock()

	peer := wanode.MustJID(peerJid)
	creator := wanode.MustJID(creatorJid)

	// Always release the in-flight marker, even if a send panics.
	defer func() {
		m.mu.Lock()
		if m.acceptingCallID == callID {
			m.acceptingCallID = ""
		}
		m.mu.Unlock()
	}()

	fail := func(err error) error {
		m.mu.Lock()
		if m.acceptingCallID == callID {
			m.acceptingCallID = ""
		}
		m.mu.Unlock()
		return err
	}

	// Build and send the accept BEFORE transitioning state. If the stanza
	// cannot be sent, the call stays ringing so the caller can roll it back
	// instead of looking like a retried (already-accepted) call.
	if key == nil {
		return fail(&CallError{"accept failed: missing call key"})
	}
	acceptNode, err := signaling.BuildAcceptStanza(ctx, m.sock, callID, key, peer, creator, isVideo)
	if err != nil {
		return fail(&CallError{"build accept failed: " + err.Error()})
	}
	if err := m.sock.SendNode(ctx, acceptNode); err != nil {
		return fail(&CallError{"send accept failed: " + err.Error()})
	}

	m.mu.Lock()
	if m.acceptingCallID == callID {
		m.acceptingCallID = ""
	}
	// Re-check under the lock: the call may have ended/changed during send.
	if m.currentCall != call || call.IsEnded() || !call.CanAccept() {
		m.mu.Unlock()
		return nil
	}
	_ = call.ApplyTransition(Transition{Type: TransitionLocalAccepted})
	m.emitState()
	m.mu.Unlock()

	if relayData != nil {
		m.setupIncomingMedia(call, relayData)
		m.connectRelays(relayData.Endpoints)
	} else {
		m.log.Warn("call accepted but no relay endpoints yet; media path waits for a transport message", "call_id", callID)
	}
	m.log.Info("call accepted", "call_id", callID)
	return nil
}

// AbortAccept rolls back a failed accept. It aborts (ends) the call only when
// it is still ringing and no accept for it is in flight. It reports whether it
// aborted, and whether the call is already accepted (a concurrent accept won).
func (m *CallManager) AbortAccept(ctx context.Context, callID string) (aborted, accepted, gone bool) {
	m.mu.Lock()
	call := m.currentCall
	if m.acceptingCallID == callID {
		m.mu.Unlock()
		return false, false, false
	}
	if call == nil || call.CallID != callID {
		m.mu.Unlock()
		return false, false, true
	}
	if call.IsEnded() {
		m.mu.Unlock()
		return false, false, true
	}
	if !call.CanAccept() {
		// Past ringing but not ended => another accept already won.
		m.mu.Unlock()
		return false, true, false
	}
	_ = call.ApplyTransition(Transition{Type: TransitionTerminated, Reason: core.EndCallReasonUserEnded})
	termDest := call.PeerJid
	if m.acceptedByJid != "" {
		termDest = m.acceptedByJid
	}
	node := signaling.BuildTerminateStanza(wanode.MustJID(termDest), call.CallID, wanode.MustJID(call.CallCreator))
	ended := call
	m.emitState()
	m.mu.Unlock()

	go func() {
		sendCtx, cancel := detachedSendContext(ctx)
		defer cancel()
		_, _ = m.sock.Query(sendCtx, node)
	}()
	if m.OnEnded != nil {
		m.OnEnded(ended)
	}
	m.cleanupMedia()
	return true, false, false
}

// AbortAccept rolls back a failed accept, but only if the call is STILL
// acceptable (ringing). If another accept already won the race, it is a no-op
// and reports false so the caller does not tear down the now-live call.

// CanAcceptID reports whether callID is an inbound call currently in a state
// that can be accepted (used to reject stray accepts without tearing down an
// outbound/live call).
func (m *CallManager) CanAcceptID(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.currentCall
	return c != nil && c.CallID == callID &&
		c.Direction == core.CallDirectionIncoming && c.CanAccept()
}

func (m *CallManager) setupIncomingMedia(call *CallInfo, relayData *core.RelayData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(relayData.ParticipantJids) > 0 {
		ourBases := m.ownBaseJIDs()
		ourDeviceJid := ensureDeviceJid(findOurDevice(relayData.ParticipantJids, ourBases, m.ownCredJid()))
		if newSelf := media.GenerateSecureSsrc(call.CallID, ourDeviceJid, 0); newSelf != m.selfSsrc {
			m.selfSsrc = newSelf
			m.rtpSession = media.NewWhatsAppOpusSession(newSelf)
		}
		if peer := firstPeerDevice(relayData.ParticipantJids, ourBases); peer != "" {
			// Best-effort subscription guess; the SSRC is confirmed (and
			// corrected if wrong) from the first authenticated RTP packet.
			m.peerSsrcs = []uint32{media.GenerateSecureSsrc(call.CallID, ensureDeviceJid(peer), 0)}
			m.actualPeerSet = false
		} else {
			m.actualPeerSet = false
			m.log.Warn("participant list has no peer device; waiting for first RTP packet to learn SSRC", "call_id", call.CallID)
		}
	}
	m.relay.SetSubscriptionSsrc(firstSsrc(m.peerSsrcs))
	m.initSrtpKeysLocked()
}

func (m *CallManager) RejectCall(ctx context.Context, callID string, reason core.EndCallReason) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.CallID != callID {
		m.mu.Unlock()
		return &CallError{"no call with id " + callID}
	}
	_ = call.ApplyTransition(Transition{Type: TransitionLocalRejected, Reason: reason})
	node := signaling.BuildRejectStanza(wanode.MustJID(call.PeerJid), call.CallID, wanode.MustJID(call.CallCreator))
	m.emitState()
	m.mu.Unlock()

	go func() {
		sendCtx, cancel := detachedSendContext(ctx)
		defer cancel()
		_, _ = m.sock.Query(sendCtx, node)
	}()
	m.cleanupMedia()
	return nil
}

func (m *CallManager) EndCall(ctx context.Context, reason core.EndCallReason) error {
	return m.endCall(ctx, "", reason)
}

func (m *CallManager) EndCallForID(ctx context.Context, callID string, reason core.EndCallReason) error {
	return m.endCall(ctx, callID, reason)
}

// endCall terminates the current call. When expectID is non-empty the call is
// ended only if it still matches, and the check happens under the same lock as
// the transition (no TOCTOU with a concurrently-replaced call).
func (m *CallManager) endCall(ctx context.Context, expectID string, reason core.EndCallReason) error {
	m.mu.Lock()
	call := m.currentCall
	if call == nil || call.IsEnded() || (expectID != "" && call.CallID != expectID) {
		m.mu.Unlock()
		return nil
	}
	_ = call.ApplyTransition(Transition{Type: TransitionTerminated, Reason: reason})
	// Route the terminate to the device that actually answered, like the rest of the in-call
	// signaling. Sending it to the base JID lets the server deliver it to the peer's primary
	// device, so a call answered on a companion (e.g. WhatsApp Web) never sees the terminate and
	// hangs in "reconnecting" until it times out. acceptedByJid is empty for inbound calls,
	// where call.PeerJid already carries the caller's device.
	termDest := call.PeerJid
	if m.acceptedByJid != "" {
		termDest = m.acceptedByJid
	}
	node := signaling.BuildTerminateStanza(wanode.MustJID(termDest), call.CallID, wanode.MustJID(call.CallCreator))
	ended := call
	m.emitState()
	m.mu.Unlock()

	go func() {
		sendCtx, cancel := detachedSendContext(ctx)
		defer cancel()
		_, _ = m.sock.Query(sendCtx, node)
	}()
	if m.OnEnded != nil {
		m.OnEnded(ended)
	}
	m.cleanupMedia()
	return nil
}

// abortStart clears a partially-started outgoing call (offer send failed) and
// releases its media, so the manager/codec are not left half-initialized.
func (m *CallManager) abortStart() {
	m.mu.Lock()
	m.currentCall = nil
	m.mu.Unlock()
	m.cleanupMedia()
}

func (m *CallManager) ownCredJid() string {
	lid := m.sock.OwnLID()
	if !lid.IsEmpty() {
		return lid.String()
	}
	return m.sock.OwnPN().String()
}

// ownBaseJIDs lists every identity this device may appear as in a
// participant list (WhatsApp now mixes LID and PN). Matching only one of
// them makes findOurDevice miss self and firstPeerDevice pick our own
// device, which subscribes the relay to our own SSRC (one-way audio/echo).
func (m *CallManager) ownBaseJIDs() []string {
	var out []string
	if lid := m.sock.OwnLID(); !lid.IsEmpty() {
		out = append(out, wanode.CleanJID(lid.String()))
	}
	if pn := m.sock.OwnPN(); !pn.IsEmpty() {
		clean := wanode.CleanJID(pn.String())
		dup := false
		for _, b := range out {
			if b == clean {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, clean)
		}
	}
	if len(out) == 0 {
		out = append(out, wanode.CleanJID(m.ownCredJid()))
	}
	return out
}

// isDuplicateRTPPacketLocked reports whether (ssrc, seq) was already handled
// within the last 2s. Callers must hold m.mu.
func (m *CallManager) isDuplicateRTPPacketLocked(ssrc uint32, seq uint16) bool {
	now := m.nowTime()
	if m.seenRTP == nil {
		m.seenRTP = make(map[uint64]time.Time, 256)
	}
	key := uint64(ssrc)<<16 | uint64(seq)
	if t, ok := m.seenRTP[key]; ok && now.Sub(t) < 2*time.Second {
		return true
	}
	m.seenRTP[key] = now
	if now.Sub(m.seenRTPPrune) > 2*time.Second {
		for k, t := range m.seenRTP {
			if now.Sub(t) > 2*time.Second {
				delete(m.seenRTP, k)
			}
		}
		m.seenRTPPrune = now
	}
	return false
}

type CallError struct{ Msg string }

func (e *CallError) Error() string { return e.Msg }

// detachedSendContext keeps a detached send alive after the caller (usually
// an HTTP handler) returns and cancels its request context. Without this the
// terminate/reject stanza can be aborted before reaching WhatsApp, leaving
// the peer's call up.
func detachedSendContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
