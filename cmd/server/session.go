package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
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
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type Session struct {
	id   string
	name string
	mgr  *SessionManager
	log  *slog.Logger

	client    *whatsmeow.Client
	handlerID uint32
	reg       *callRegistry

	mu   sync.Mutex
	auth AuthSnapshot

	// connMu serializes connect() with the logout teardown (F1), so a
	// LoggedOut event can never race an in-flight connect()/pairing start.
	// It is held across blocking whatsmeow calls, hence kept separate from
	// mu, which only guards short state reads/writes.
	connMu sync.Mutex

	// qrActive/qrGen enforce "never two concurrent QR channels per session".
	// qrGen is bumped whenever a pairing context is invalidated so that a
	// superseded pairing's release cannot free a newer claim.
	qrActive bool
	qrGen    int
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
	// Register a handler that captures the client at registration time, so a
	// stale event from a discarded client (post-replaceClient) is dropped
	// instead of mutating the replacement client (H3 identity guard).
	s.handlerID = client.AddEventHandler(func(evt any) {
		s.mu.Lock()
		if s.client != client {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.handleEvent(evt)
	})
	return s
}

func (s *Session) createCall(callID string) *call.CallManager {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	cm := call.NewCallManager(wa.NewSocket(client), s.log)
	s.wireCall(cm, callID)
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
		ac, ok := s.reg.get(callID)
		if !ok || ac.bridge == nil {
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
		_ = ac.bridge.WritePCM(pcm16)
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
	if max := s.mgr.maxCalls; max > 0 && s.reg.count() >= max {
		s.rejectOffer(ctx, node, evt.From)
		return
	}
	cm := s.createCall(callID)
	cm.HandleCallOffer(ctx, node, evt.From)
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
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	_ = wa.NewSocket(client).SendNode(ctx, reject)
	s.log.Info("inbound call rejected: session at capacity", "call_id", info.CallID)
}

func (s *Session) handleEvent(rawEvt any) {
	// Identity guard is enforced by the closure registered in newSession/
	// replaceClient, which captures the client at registration time.
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Connected:
		s.mu.Lock()
		c := s.client
		s.mu.Unlock()
		if c != nil && c.Store != nil {
			if id := c.Store.ID; id != nil {
				_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
			}
		}
		s.setAuth(AuthSnapshot{State: "open", Paired: true})
	case *events.LoggedOut:
		// F1: an external logout must stop whatsmeow's reconnect loop.
		// Tear the client down first (so no retry can be armed while the
		// state still looks live), then publish the terminal state.
		s.stopReconnect()
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

// pairingDecision is the branch connect() must take for a session.
type pairingDecision int

const (
	decisionConnect pairingDecision = iota
	decisionPair
)

// decideConnect maps the auth state (the single source of truth), the stored
// credentials and the QR-channel guard to the branch connect() takes:
//
//   - no stored credentials            => pair  (inv 1: fresh/unpaired session)
//   - state "logged_out"               => pair  (inv 3: terminal state, F1)
//   - state "qr" without a live channel => pair (inv 3: stuck pairing)
//   - anything else with credentials   => connect (inv 2: boot of a paired
//     session must auto-Connect and never force a re-pair)
//
// A restored unscanned session (T2) arrives with an intact auth store but no
// user ID, so it hits the first clause and becomes pairable while inv 2 keeps
// credentialed sessions on the Connect path.
func decideConnect(state string, hasCreds, qrActive bool) pairingDecision {
	switch {
	case !hasCreds, state == "logged_out", state == "qr" && !qrActive:
		return decisionPair
	default:
		return decisionConnect
	}
}

// connect resumes a session with stored credentials or opens a fresh QR
// pairing channel. connMu keeps this from racing the F1 logout teardown.
func (s *Session) connect(ctx context.Context) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	s.mu.Lock()
	state, qrActive := s.auth.State, s.qrActive
	client := s.client
	s.mu.Unlock()

	if decideConnect(state, client.Store.ID != nil, qrActive) == decisionPair {
		if client.Store.ID != nil {
			// Pairing was decided, but whatsmeow refuses GetQRChannel while
			// a user ID is stored: those credentials can never reach "open"
			// again, so drop them and pair on a fresh device store.
			s.replaceClient(whatsmeow.NewClient(s.mgr.container.NewDevice(), s.mgr.waLogger))
		}
		return s.startPairing(ctx)
	}
	if err := client.Connect(); err != nil {
		return s.connectFailed(ctx, err)
	}
	return nil
}

// connectFailed applies invariant 3 when an authenticated Connect() fails:
// the session must not stay stuck without a QR. Transport-level failures keep
// the stored credentials (a network blip at boot must not force a healthy
// session to re-pair — QR pairing needs the same network anyway); every other
// failure means the credentials cannot reach "open", so they are replaced and
// a fresh QR is opened through the normal pairing path.
func (s *Session) connectFailed(ctx context.Context, err error) error {
	if errors.Is(err, whatsmeow.ErrAlreadyConnected) {
		return nil // already connected: nothing to recover
	}
	if isTransientConnectError(err) {
		s.log.Warn("connect failed transiently; stored credentials kept", "err", err)
		// H5: schedule a background retry with backoff so the session does
		// not stay stuck in "connecting" forever after a transient blip.
		// NOTE: retry goroutines are not tracked here (retryDone deferred);
		// Delete in sessionmanager.go should wait for in-flight retries with
		// a timeout before tearing the session down.
		go func() {
			s.mu.Lock()
			client := s.client
			s.mu.Unlock()
			retryCtx := context.Background()
			for attempt := 1; attempt <= 3; attempt++ {
				time.Sleep(time.Duration(attempt) * 5 * time.Second)
				// Abort if session was deleted.
				if _, ok := s.mgr.Get(s.id); !ok {
					return
				}
				s.mu.Lock()
				if s.client != client {
					s.mu.Unlock()
					return
				}
				if s.auth.State == "open" {
					s.mu.Unlock()
					return
				}
				s.mu.Unlock()
				if rerr := s.connect(retryCtx); rerr == nil {
					s.log.Info("connect retry succeeded", "attempt", attempt)
					return
				} else {
					s.log.Warn("connect retry failed", "attempt", attempt, "err", rerr)
				}
			}
		}()
		return err
	}
	s.log.Warn("connect failed; dropping stored credentials and opening QR", "err", err)
	s.replaceClient(whatsmeow.NewClient(s.mgr.container.NewDevice(), s.mgr.waLogger))
	if pairErr := s.startPairing(ctx); pairErr != nil {
		s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
		return fmt.Errorf("connect: %w (qr fallback: %v)", err, pairErr)
	}
	return nil
}

// isTransientConnectError reports whether err is a transport-level failure
// that says nothing about the stored credentials. Auth rejections never come
// back from Connect(): whatsmeow reports them asynchronously as
// events.LoggedOut, which routes the session to "logged_out" and then through
// pairing (see decideConnect).
func isTransientConnectError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, socket.ErrDialFailed) {
		return true
	}
	var statusErr socket.ErrWithStatusCode
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case 400, 401, 403, 404, 410, 425:
			return false // rejected/dead credentials or non-retryable client error
		default:
			return true // 408/5xx and friends: server/network trouble
		}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false // unknown failure (e.g. broken handshake): surface a QR (inv 3)
}

// stopReconnect tears the client down after an external logout (F1).
//
// whatsmeow's autoReconnect loop (client.go) retries forever while
// EnableAutoReconnect is true and the store holds an ID; the keepAliveLoop can
// also force a reconnect after 3 minutes while EnableAutoReconnect is true.
// Disconnect() marks the disconnect as expected — cancelling an in-flight
// autoReconnect sleep and keeping onDisconnect from spawning a new one — and
// disabling EnableAutoReconnect closes the remaining retry paths. connMu keeps
// this from racing an in-flight connect().
func (s *Session) stopReconnect() {
	// H4: connMu may already be held by this goroutine when stopReconnect is
	// invoked from within a connMu-held context (e.g. synchronously inside
	// client.Connect()). Go's sync.Mutex is not re-entrant, so use TryLock and
	// defer the teardown to a goroutine in that case, clearing the slot now.
	if !s.connMu.TryLock() {
		// connMu already held (re-entrant). Capture client now; defer teardown
		// only if the client hasn't changed by then.
		s.mu.Lock()
		client := s.client
		s.mu.Unlock()
		go func() {
			s.connMu.Lock()
			defer s.connMu.Unlock()
			s.mu.Lock()
			if s.client != client {
				s.mu.Unlock()
				return // client changed, abort teardown
			}
			// Clear the slot only after the identity check, in the deferred
			// path (never in the main path).
			s.qrActive = false
			s.qrGen++
			s.mu.Unlock()
			if client != nil {
				client.EnableAutoReconnect = false
				client.Disconnect()
			}
		}()
		return
	}
	defer s.connMu.Unlock()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client != nil {
		client.EnableAutoReconnect = false
		client.Disconnect()
	}
	// Clear any in-flight pairing claim so a pairing active at logout does
	// not leave the slot claimed and block future QR (MEDIUM-2). Lock order
	// connMu -> s.mu, consistent with the rest of the code.
	s.mu.Lock()
	s.qrActive = false
	s.qrGen++
	s.mu.Unlock()
}

// beginPairing claims the session's single QR-channel slot (inv 4). The
// returned generation lets a superseded pairing release the slot without
// clearing a newer claim.
func (s *Session) beginPairing() (gen int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.qrActive {
		return 0, false
	}
	s.qrActive = true
	s.qrGen++
	return s.qrGen, true
}

func (s *Session) endPairing(gen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.qrActive && s.qrGen == gen {
		s.qrActive = false
	}
}

func (s *Session) startPairing(ctx context.Context) error {
	gen, ok := s.beginPairing()
	if !ok {
		// inv 4: another QR channel is already live for this session.
		s.log.Debug("pairing already active; not opening a second QR channel")
		return nil
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		s.endPairing(gen)
		return err
	}
	// F1 disables auto-reconnect on logout; pairing starts a new login
	// lifecycle on this client, so arm it again before connecting.
	client.EnableAutoReconnect = true
	if err := client.Connect(); err != nil {
		s.endPairing(gen)
		return err
	}
	go func() {
		defer s.endPairing(gen)
		terminal := false
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				s.log.Info("scan the QR code to pair this session")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				s.setAuth(AuthSnapshot{State: "qr", QR: evt.Code})
				s.mgr.broker.emitSessionQR(s.id, evt.Code)
			case "success":
				if client.Store != nil {
					if id := client.Store.ID; id != nil {
						_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, id.String())
					}
				}
				s.setAuth(AuthSnapshot{State: "open", Paired: true})
				terminal = true
			case "timeout":
				// Disable auto-reconnect + disconnect before publishing the
				// terminal state, so the slot is not left armed/connected
				// (LOW-1). stopReconnect acquires connMu; this goroutine is
				// not holding it, so it is safe to call here.
				s.stopReconnect()
				s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
				terminal = true
			}
		}
		if !terminal {
			s.setAuth(AuthSnapshot{State: "logged_out", Paired: false})
		}
	}()
	return nil
}

func (s *Session) setAuth(a AuthSnapshot) {
	s.mu.Lock()
	s.auth = a
	s.mu.Unlock()
	if s.mgr.isRestoring() {
		return
	}
	s.mgr.broker.emitAuthState(s.id, a)
	s.mgr.broker.emitSessionList(s.mgr.infos())
}

func (s *Session) info() SessionInfo {
	s.mu.Lock()
	client := s.client
	a := s.auth
	s.mu.Unlock()
	jid := ""
	if client != nil && client.Store != nil {
		if id := client.Store.ID; id != nil {
			jid = id.String()
		}
	}
	return SessionInfo{ID: s.id, Name: s.name, JID: jid, State: a.State, Paired: a.Paired || (a.State != "logged_out" && jid != "")}
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
		oldB.Close()
		return
	}
	logbuf.LogEvent("bridge", logbuf.LevelInfo, "setBridge: bridge attached", map[string]any{
		"call_id": callID, "session_id": s.id,
	})
}

func (s *Session) removeCall(callID string) {
	ac, ok := s.reg.remove(callID)
	if !ok {
		return
	}
	if ac.bridge != nil {
		ac.bridge.Close()
	}
}

func (s *Session) terminateCall(callID string, reason core.EndCallReason) {
	ac, ok := s.reg.get(callID)
	if !ok {
		return
	}
	_ = ac.cm.EndCall(context.Background(), reason)
}

func (s *Session) teardownAllCalls() {
	for _, ac := range s.reg.drain() {
		_ = ac.cm.EndCall(context.Background(), core.EndCallReasonUserEnded)
		if ac.bridge != nil {
			ac.bridge.Close()
		}
	}
}

func (s *Session) replaceClient(client *whatsmeow.Client) {
	s.mu.Lock()
	oldClient := s.client
	oldHandlerID := s.handlerID
	s.mu.Unlock()

	s.teardownAllCalls()
	if oldClient != nil {
		// H0-3: disarm auto-reconnect on old client
		oldClient.EnableAutoReconnect = false
		// H0-2: remove event handler from old client
		oldClient.RemoveEventHandler(oldHandlerID)
		// H0-1: clear persisted JID of discarded client
		if oldClient.Store != nil && oldClient.Store.ID != nil {
			_ = s.mgr.store.setJID(s.mgr.appCtx, s.id, "")
			_ = s.mgr.container.DeleteDevice(s.mgr.appCtx, oldClient.Store)
		}
		oldClient.Disconnect()
	}

	// Register a handler capturing the new client, so stale events from the
	// discarded client are dropped (H3 identity guard).
	s.mu.Lock()
	s.client = client
	s.handlerID = client.AddEventHandler(func(evt any) {
		s.mu.Lock()
		if s.client != client {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.handleEvent(evt)
	})
	s.qrActive = false
	s.qrGen++
	s.mu.Unlock()
}

func (s *Session) shutdown() {
	s.teardownAllCalls()
	s.mu.Lock()
	client := s.client
	handlerID := s.handlerID
	s.mu.Unlock()
	if client == nil {
		return
	}
	client.EnableAutoReconnect = false
	client.RemoveEventHandler(handlerID)
	client.Disconnect()
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
