package main

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"wacalls/internal/voip/call"
	"wacalls/internal/voip/core"
	"wacalls/internal/voip/logbuf"
	"wacalls/internal/voip/signaling"
	"wacalls/internal/voip/wanode"
	"wacalls/internal/wa"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type Session struct {
	id   string
	name string
	mgr  *SessionManager
	log  *slog.Logger

	client *whatsmeow.Client
	reg    *callRegistry

	newCallManager func(callID string) *call.CallManager

	mu   sync.Mutex
	auth AuthSnapshot
}

func newSession(mgr *SessionManager, id, name string, client *whatsmeow.Client) *Session {
	s := &Session{
		id:     id,
		name:   name,
		mgr:    mgr,
		log:    mgr.log.With("session", id),
		client: client,
		auth:   AuthSnapshot{State: "connecting"},
		reg:    newCallRegistry(),
	}
	s.newCallManager = s.newCall
	client.AddEventHandler(s.handleEvent)
	return s
}

func (s *Session) newCall(callID string) *call.CallManager {
	cm := call.NewCallManager(wa.NewSocket(s.client), s.log)
	s.wireCall(cm, callID)
	return cm
}

func (s *Session) createCall(callID string) *call.CallManager {
	cm := s.newCallManager(callID)
	s.reg.add(callID, &activeCall{cm: cm})
	return cm
}

func (s *Session) wireCall(cm *call.CallManager, callID string) {
	cm.OnIncoming = func(c *call.CallInfo) {
		s.mgr.broker.upsertCall(CallRecord{
			SessionID: s.id, CallID: c.CallID, Direction: "inbound", Peer: c.PeerJid,
			StartedAt: time.Now().UnixMilli(), Status: StatusRinging,
		})
		s.mgr.broker.emitIncoming(s.id, c.CallID, c.PeerJid)
	}
	cm.OnStateChange = func(c *call.CallInfo) {
		if c.IsEnded() {
			s.removeCall(c.CallID)
			s.mgr.broker.endCall(c.CallID, string(c.StateData.EndReason))
			return
		}
		dir := "outbound"
		if c.Direction == core.CallDirectionIncoming {
			dir = "inbound"
		}
		existing, _ := s.mgr.broker.getCall(c.CallID)
		rec := CallRecord{
			SessionID: s.id, CallID: c.CallID, Direction: dir, Peer: c.PeerJid,
			StartedAt: time.Now().UnixMilli(), Status: mapStatus(c.StateData.State),
		}
		if existing != nil {
			rec.Owner = existing.Owner
			rec.StartedAt = existing.StartedAt
		}
		s.mgr.broker.upsertCall(rec)
	}
	cm.OnEnded = func(c *call.CallInfo) {
		s.removeCall(c.CallID)
		s.mgr.broker.endCall(c.CallID, string(c.StateData.EndReason))
	}
	cm.OnPeerAudio = func(pcm16 []float32) {
		owner, br, ok := s.reg.getWithBridge(callID)
		if !ok || owner != cm || br == nil {
			return
		}
		// Rate-limited 5/s: two callbacks registered for this call double
		// every frame — the dup-detector catches the paired WritePCM calls.
		if logbuf.Enabled() && logbuf.Allow("session-audio:"+callID, 5) {
			logbuf.LogEvent("session-audio", logbuf.LevelDebug, "peer audio forwarded to bridge", map[string]any{
				"call_id": callID,
				"samples": len(pcm16),
			})
		}
		_ = br.WritePCM(pcm16)
	}
}

func (s *Session) startOutgoing(ctx context.Context, peer types.JID, isVideo bool) (string, error) {
	callID := signaling.GenerateCallID()
	cm := s.createCall(callID)
	if err := cm.StartCall(ctx, callID, peer, isVideo); err != nil {
		s.removeCall(callID)
		return "", err
	}
	return callID, nil
}

func (s *Session) callForEvent(from types.JID, data *waBinary.Node) (*activeCall, bool) {
	callID := callIDFromNode(wrapCall(from, data))
	if callID == "" {
		return nil, false
	}
	return s.reg.get(callID)
}

func (s *Session) onIncomingOffer(ctx context.Context, evt *events.CallOffer) {
	node := wrapCall(evt.From, evt.Data)
	callID := callIDFromNode(node)
	if callID == "" {
		return
	}
	if _, ok := s.reg.get(callID); ok {
		s.logDuplicateOffer(callID, evt.From)
		return
	}
	if info := signaling.ExtractNodeInfo(node); info != nil {
		if _, err := signaling.DecryptCallKeyInNode(ctx, wa.NewSocket(s.client), info.InnerNode, evt.From); err != nil {
			s.log.Error("offer call key undecryptable; rejecting call", "call_id", callID, "peer", evt.From.String(), "err", err)
			s.rejectOffer(ctx, node, evt.From)
			return
		}
	}
	if max := s.mgr.maxCalls; max > 0 && s.reg.count() >= max {
		s.rejectOffer(ctx, node, evt.From)
		return
	}
	if !s.mgr.broker.tryAdmit() {
		s.rejectOffer(ctx, node, evt.From)
		return
	}
	cm := s.newCallManager(callID)
	// Insert atomically: if another delivery won the race between the check
	// above and here, keep the existing call instead of overwriting it.
	if !s.reg.addIfAbsent(callID, &activeCall{cm: cm}) {
		s.logDuplicateOffer(callID, evt.From)
		return
	}
	cm.HandleCallOffer(ctx, node, evt.From)
}

// logDuplicateOffer records a re-entrant offer that must not replace a live
// call. createCall would otherwise orphan the live bridge and leak a second
// CallManager whose OnPeerAudio still writes PCM to the registry bridge.
func (s *Session) logDuplicateOffer(callID string, from types.JID) {
	logbuf.LogEvent("session", logbuf.LevelWarn, "duplicate call ignored, existing call kept", map[string]any{
		"call_id": callID, "session_id": s.id, "peer": maskPeer(from.String()),
	})
}

func (s *Session) rejectOffer(ctx context.Context, node *waBinary.Node, from types.JID) {
	info := signaling.ExtractNodeInfo(node)
	if info == nil {
		return
	}
	creator := wanode.AttrString(info.InnerNode.Attrs, "call-creator")
	if creator == "" {
		creator = from.String()
	}
	reject := signaling.BuildRejectStanza(from, info.CallID, wanode.MustJID(creator))
	_ = wa.NewSocket(s.client).SendNode(ctx, reject)
	s.log.Info("inbound call rejected: session at capacity", "call_id", info.CallID)
}

func (s *Session) handleEvent(rawEvt any) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("panic in event handler", "session", s.id, "panic", r)
		}
	}()
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Connected:
		if id := s.client.Store.ID; id != nil {
			_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
		}
		s.setAuth(AuthSnapshot{State: "open", Paired: true})
	case *events.LoggedOut:
		s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
	case *events.CallOffer:
		s.onIncomingOffer(ctx, evt)
	case *events.CallAccept:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallAccept(ctx, wrapCall(evt.From, evt.Data), evt.From)
		}
	case *events.CallTransport:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTransport(ctx, wrapCall(evt.From, evt.Data), evt.From)
		}
	case *events.CallTerminate:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTerminate(wrapCall(evt.From, evt.Data))
		}
	case *events.CallReject:
		if ac, ok := s.callForEvent(evt.From, evt.Data); ok {
			ac.cm.HandleCallTerminate(wrapCall(evt.From, evt.Data))
		}
	}
}

func (s *Session) connect(ctx context.Context) error {
	if s.client.Store.ID != nil {
		return s.client.Connect()
	}
	return s.startPairing(ctx)
}

func (s *Session) startPairing(ctx context.Context) error {
	qrChan, err := s.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := s.client.Connect(); err != nil {
		return err
	}
	go func() {
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				s.log.Info("scan the QR code to pair this session")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				s.setAuth(AuthSnapshot{State: "qr", QR: evt.Code})
				s.mgr.broker.emitSessionQR(s.id, evt.Code)
			case "success":
				if id := s.client.Store.ID; id != nil {
					_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
				}
				s.setAuth(AuthSnapshot{State: "open", Paired: true})
			case "timeout":
				s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
			default:
				s.log.Warn("unhandled qr event", "event", evt.Event, "err", evt.Error)
			}
		}
		s.log.Info("qr channel closed")
		s.mu.Lock()
		st := s.auth.State
		s.mu.Unlock()
		if st == "connecting" || st == "qr" {
			s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
		}
	}()
	return nil
}

func (s *Session) setAuth(a AuthSnapshot) {
	s.mu.Lock()
	s.auth = a
	s.mu.Unlock()
	s.mgr.broker.emitAuthState(s.id, a)
	s.mgr.broker.emitSessionList(s.mgr.infos())
}

func (s *Session) info() SessionInfo {
	s.mu.Lock()
	a := s.auth
	s.mu.Unlock()
	jid := ""
	if id := s.client.Store.ID; id != nil {
		jid = id.String()
	}
	return SessionInfo{ID: s.id, Name: s.name, JID: jid, State: a.State, Paired: a.Paired || jid != ""}
}

func (s *Session) setBridge(callID string, b *Bridge) {
	oldB, found := s.reg.setBridge(callID, b)
	if !found {
		logbuf.LogEvent("bridge", logbuf.LevelWarn, "setBridge: call not found, new bridge closed", map[string]any{
			"call_id": callID, "session_id": s.id,
		})
		b.Close()
		return
	}
	if oldB != nil {
		// A replaced bridge with an active WebRTC peer is a duplication
		// suspect: log the swap explicitly.
		logbuf.LogEvent("bridge", logbuf.LevelWarn, "setBridge: bridge replaced, old bridge closed", map[string]any{
			"call_id": callID, "session_id": s.id, "replaced": true,
		})
		// Detach first: closing the old peer can fire ICE Failed/Closed,
		// and its OnTerminalICE would otherwise terminate the LIVE call,
		// while OnBrowserPCM would keep feeding the live CallManager.
		oldB.Detach()
		oldB.Close()
		return
	}
	peer := "unknown"
	if rec, ok := s.mgr.broker.getCall(callID); ok {
		peer = maskPeer(rec.Peer)
	}
	logbuf.LogEvent("bridge", logbuf.LevelInfo, "setBridge: bridge attached", map[string]any{
		"call_id": callID, "session_id": s.id, "peer": peer,
	})
}

func (s *Session) removeCall(callID string) {
	ac, ok := s.reg.remove(callID)
	if !ok {
		return
	}
	if ac.bridge != nil {
		ac.bridge.Detach()
		ac.bridge.Close()
	}
	s.mgr.broker.releaseAdmission()
}

func (s *Session) terminateCall(callID string, reason core.EndCallReason) {
	ac, ok := s.reg.get(callID)
	if !ok {
		return
	}
	_ = ac.cm.EndCall(context.Background(), reason)
}

// terminateCallIfBridge ends callID only when b still owns it, so an in-flight
// ICE callback from a replaced/closed bridge cannot kill the live call.
func (s *Session) terminateCallIfBridge(callID string, b *Bridge) {
	if !s.reg.bridgeIs(callID, b) {
		return
	}
	ac, ok := s.reg.get(callID)
	if !ok {
		return
	}
	_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
}

func (s *Session) teardownAllCalls() {
	for _, ac := range s.reg.drain() {
		_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
		if ac.bridge != nil {
			ac.bridge.Detach()
			ac.bridge.Close()
		}
	}
}

func (s *Session) replaceClient(client *whatsmeow.Client) {
	s.teardownAllCalls()
	s.client.Disconnect()
	s.client = client
	client.AddEventHandler(s.handleEvent)
}

func (s *Session) shutdown() {
	s.teardownAllCalls()
	s.client.Disconnect()
}

func mapStatus(state core.CallState) CallStatus {
	switch state {
	case core.CallStateActive:
		return StatusConnected
	case core.CallStateEnded:
		return StatusEnded
	case core.CallStateInitiating:
		return StatusStarting
	default:
		return StatusRinging
	}
}

// maskPeer keeps only the last 4 characters of the user part of a JID/phone,
// so debug logs stay diagnostic without leaking full numbers (LAN-trust endpoint).
func maskPeer(p string) string {
	at := strings.IndexByte(p, '@')
	user := p
	domain := ""
	if at >= 0 {
		user = p[:at]
		domain = p[at:]
	}
	if c := strings.IndexByte(user, ':'); c >= 0 {
		user = user[:c]
	}
	runes := []rune(user)
	if len(runes) <= 4 {
		return "****" + domain
	}
	return strings.Repeat("*", len(runes)-4) + string(runes[len(runes)-4:]) + domain
}
