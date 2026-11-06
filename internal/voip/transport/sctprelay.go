package transport

import (
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
	"wacalls/internal/voip/core"

	"github.com/pion/webrtc/v4"
)

const (
	relayConnectionTimeout = 20 * time.Second
	relayKeepaliveInterval = 1100 * time.Millisecond
	// relayIceGrace is how long a sustained ICE Disconnected may persist
	// before the relay is declared dead (pion recovers from short blips).
	relayIceGrace = 10 * time.Second
)

type relayConnState int

const (
	relayStateConnecting relayConnState = iota
	relayStateOpen
	relayStateClosed
	relayStateFailed
)

type RelayConfig struct {
	IP           string
	Port         int
	Token        string
	AuthToken    string
	RawAuthToken []byte
	RawToken     []byte
	Key          string
	RelayID      int
	Name         string
	AuthTokenID  string
}

type relayConnection struct {
	state        atomic.Int32
	pc           *webrtc.PeerConnection
	channel      *webrtc.DataChannel
	id           string
	info         RelayConfig
	localUfrag   string
	stopCh       chan struct{}
	teardownOnce sync.Once

	// iceGrace gives a transient ICE Disconnected a chance to recover before
	// the connection is treated as dead. Guarded by iceGraceMu.
	iceGraceMu sync.Mutex
	iceGrace   *time.Timer
}

// armIceGrace (re)starts the give-up timer for a sustained ICE Disconnected;
// every new Disconnected notification restarts the window.
func (c *relayConnection) armIceGrace(window time.Duration, onExpire func()) {
	c.iceGraceMu.Lock()
	defer c.iceGraceMu.Unlock()
	if c.iceGrace != nil {
		c.iceGrace.Stop()
	}
	// The callback re-checks that this exact timer is still the registered
	// one before expiring (mirrors cmd/server/grace.go): time.Timer.Stop
	// returns false once the timer has already fired, so a cancel (ICE
	// Connected recovered) or a re-arm (new Disconnected) that landed after
	// the fire but before the callback took this lock must win — a recovered
	// connection is never failed by a stale expiry. Declared separately so
	// the closure can reference t; t is READ only while holding iceGraceMu
	// (the assignment below also happens under that lock), otherwise the
	// capture races with the assignment. The fail logic (onExpire) runs
	// OUTSIDE the lock.
	var t *time.Timer
	t = time.AfterFunc(window, func() {
		c.iceGraceMu.Lock()
		current := c.iceGraceStillCurrentLocked(t)
		c.iceGraceMu.Unlock()
		if !current {
			return
		}
		onExpire()
	})
	c.iceGrace = t
}

// iceGraceStillCurrentLocked reports whether t is still the connection's
// registered grace timer. Caller must hold iceGraceMu.
func (c *relayConnection) iceGraceStillCurrentLocked(t *time.Timer) bool {
	return c.iceGrace == t
}

// iceGraceStillCurrent reports whether t is still the connection's registered
// grace timer. It is false when the timer was cancelled (field nil) or
// re-armed (field replaced) after t fired but before t's callback validated
// the identity — that stale expiry must not fail the connection.
func (c *relayConnection) iceGraceStillCurrent(t *time.Timer) bool {
	c.iceGraceMu.Lock()
	defer c.iceGraceMu.Unlock()
	return c.iceGraceStillCurrentLocked(t)
}

func (c *relayConnection) cancelIceGrace() {
	c.iceGraceMu.Lock()
	defer c.iceGraceMu.Unlock()
	if c.iceGrace != nil {
		c.iceGrace.Stop()
		c.iceGrace = nil
	}
}

func (c *relayConnection) getState() relayConnState  { return relayConnState(c.state.Load()) }
func (c *relayConnection) setState(s relayConnState) { c.state.Store(int32(s)) }

type SctpRelayManager struct {
	mu          sync.Mutex
	connections map[string]*relayConnection
	log         *slog.Logger

	audioSsrc        atomic.Uint32
	subscriptionSsrc atomic.Uint32

	onConnected func(ip string, port int)

	onReceive func(data []byte)

	// onAllLost fires (at most once per dial cycle) when every dialed relay
	// connection is gone, so the call owner can end a media-less zombie call
	// instead of leaving it ACTIVE forever.
	onAllLost  func()
	everDialed bool
	// pendingDials counts connectToRelay calls still in flight and dialing
	// holds the ids being dialed (a relay is only published into connections
	// once fully built). all-lost must not fire while a dial is pending.
	pendingDials int
	dialing      map[string]bool
}

func NewSctpRelayManager(log *slog.Logger) *SctpRelayManager {
	if log == nil {
		log = slog.Default()
	}
	return &SctpRelayManager{
		connections: map[string]*relayConnection{},
		log:         log,
	}
}

func (m *SctpRelayManager) SetSsrc(ssrc uint32) { m.audioSsrc.Store(ssrc) }

func (m *SctpRelayManager) SetSubscriptionSsrc(ssrc uint32) { m.subscriptionSsrc.Store(ssrc) }

func (m *SctpRelayManager) SetOnConnected(fn func(ip string, port int)) { m.onConnected = fn }

func (m *SctpRelayManager) SetOnReceive(fn func(data []byte)) { m.onReceive = fn }

func (m *SctpRelayManager) SetOnAllLost(fn func()) { m.onAllLost = fn }

// maybeAllLostLocked reports whether the all-lost condition holds right now:
// no dial in flight, no connection left, and a dial cycle actually started.
// It consumes the cycle (everDialed=false) so it fires at most once per cycle.
// Caller holds m.mu; the returned func (if any) must run WITHOUT the lock.
func (m *SctpRelayManager) maybeAllLostLocked() func() {
	if m.pendingDials > 0 || len(m.connections) != 0 || !m.everDialed || m.onAllLost == nil {
		return nil
	}
	m.everDialed = false // re-arm for the next dial cycle
	return m.onAllLost
}

// noteConnectionLost reports whether the last dialed connection just went
// away, firing onAllLost exactly once for that dial cycle.
func (m *SctpRelayManager) noteConnectionLost() {
	m.mu.Lock()
	fn := m.maybeAllLostLocked()
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// dialFinished releases a dial reservation (id + pending count) when
// connectToRelay returns, firing all-lost if that was the last pending dial
// and nothing is left in the map (every dial in the cycle failed early).
func (m *SctpRelayManager) dialFinished(id string) {
	m.mu.Lock()
	delete(m.dialing, id)
	if m.pendingDials > 0 {
		m.pendingDials--
	}
	fn := m.maybeAllLostLocked()
	m.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (m *SctpRelayManager) ResendSubscriptions() {
	m.mu.Lock()
	conns := make([]*relayConnection, 0, len(m.connections))
	for _, c := range m.connections {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		if c.getState() == relayStateOpen && c.channel != nil {
			m.sendStunRegistration(c)
		}
	}
}

func connID(ip string, port int, authTokenID string) string {
	base := fmt.Sprintf("%s:%d", ip, port)
	if authTokenID != "" {
		return base + "#" + authTokenID
	}
	return base
}

func (m *SctpRelayManager) ConfigureRelays(relays []RelayConfig) {
	// Two passes: reserve EVERY dial id (and the pending counter) under one
	// lock BEFORE spawning any of them, so a fast-failing first dial can
	// never observe pendingDials==0 while a sibling is still about to start.
	var toDial []RelayConfig
	m.mu.Lock()
	if m.dialing == nil {
		m.dialing = map[string]bool{}
	}
	for _, r := range relays {
		if r.Port == 0 {
			r.Port = core.WARelayPort
		}
		id := connID(r.IP, r.Port, r.AuthTokenID)
		if _, exists := m.connections[id]; exists || m.dialing[id] {
			continue
		}
		m.dialing[id] = true
		m.pendingDials++
		m.everDialed = true
		toDial = append(toDial, r)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, r := range toDial {
		wg.Add(1)
		go func(rc RelayConfig) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					m.log.Error("panic in relay dial", "id", connID(rc.IP, rc.Port, rc.AuthTokenID), "panic", rec)
				}
			}()
			m.connectToRelay(rc)
		}(r)
	}
	wg.Wait()
}

func (m *SctpRelayManager) connectToRelay(info RelayConfig) {
	id := connID(info.IP, info.Port, info.AuthTokenID)
	// Always release the dial reservation (pending count + id) on every exit
	// path, including panics: ConfigureRelays reserved it before spawning.
	defer m.dialFinished(id)
	m.log.Info("relay connecting", "id", id, "name", info.Name)

	conn := &relayConnection{
		id:     id,
		info:   info,
		stopCh: make(chan struct{}),
	}
	// Recover where conn is in scope: a panic anywhere below must release a
	// published conn (failConnection removes it from the map and tears the
	// pc down) BEFORE dialFinished drops the pending counter, or the entry
	// would block all-lost and re-dial forever. Registered after
	// dialFinished so it runs first (LIFO) on unwind.
	defer func() {
		if rec := recover(); rec != nil {
			m.log.Error("panic in relay dial", "id", id, "panic", rec)
			m.failConnection(conn)
		}
	}()

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		m.log.Error("relay peerconnection failed", "id", id, "err", err)
		m.failConnection(conn)
		return
	}
	conn.pc = pc

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		m.log.Info("relay ice state", "id", id, "state", s.String())
		switch s {
		case webrtc.ICEConnectionStateFailed:
			conn.cancelIceGrace()
			m.failConnection(conn)
		case webrtc.ICEConnectionStateDisconnected:
			// pion recovers from transient Disconnected (consent checks);
			// only give up after a sustained blip. The window restarts on
			// every notification, so a flapping link keeps getting grace.
			conn.armIceGrace(relayIceGrace, func() {
				defer func() {
					if r := recover(); r != nil {
						m.log.Error("panic in relay ice grace", "id", id, "panic", r)
					}
				}()
				m.failConnection(conn)
			})
		case webrtc.ICEConnectionStateConnected, webrtc.ICEConnectionStateCompleted:
			conn.cancelIceGrace()
		}
	})

	ordered := false
	channel, err := pc.CreateDataChannel("wa-web-call", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		m.log.Error("relay datachannel failed", "id", id, "err", err)
		m.failConnection(conn)
		return
	}
	conn.channel = channel

	channel.OnOpen(func() {
		m.log.Info("relay datachannel open", "id", id)
		conn.setState(relayStateOpen)
		m.sendStunRegistration(conn)
		m.startKeepalive(conn)
		if m.onConnected != nil {
			m.onConnected(info.IP, info.Port)
		}
	})
	channel.OnClose(func() { m.closeConnection(id) })
	channel.OnMessage(func(msg webrtc.DataChannelMessage) {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("panic in relay receive callback", "id", id, "panic", r)
			}
		}()
		if m.onReceive != nil {
			m.onReceive(msg.Data)
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		m.failConnection(conn)
		return
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		m.failConnection(conn)
		return
	}

	conn.localUfrag = extractFirst(reUfrag, offer.SDP)
	munged := m.modifySdpForRelay(offer.SDP, info)

	// Publish the fully-built connection BEFORE enabling the signaling that
	// callbacks can fire on (SetRemoteDescription): Broadcast/teardown/
	// closeConnection then never observe half-initialized pointers (pc and
	// channel are assigned above), and an early OnClose always finds the
	// conn in the map. Failures before this point call failConnection on an
	// unpublished conn — a no-op delete — and the pending-dial counter in
	// noteConnectionLost keeps all-lost from firing mid-cycle.
	m.mu.Lock()
	m.connections[id] = conn
	m.mu.Unlock()

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: munged}); err != nil {
		m.log.Error("relay set remote description failed", "id", id, "err", err)
		m.failConnection(conn)
		return
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				m.log.Error("panic in relay watchdog", "id", id, "panic", r)
			}
		}()
		select {
		case <-time.After(relayConnectionTimeout):
			if conn.getState() == relayStateConnecting {
				m.log.Debug("relay connection timeout", "id", id)
				m.failConnection(conn)
			}
		case <-conn.stopCh:
		}
	}()
}

var (
	reSetup       = regexp.MustCompile(`a=setup:actpass`)
	reUfragLine   = regexp.MustCompile(`a=ice-ufrag:[^\r\n]+`)
	rePwdLine     = regexp.MustCompile(`a=ice-pwd:[^\r\n]+`)
	reFingerprint = regexp.MustCompile(`a=fingerprint:[^\r\n]+`)
	reMaxMsg      = regexp.MustCompile(`a=max-message-size:[^\r\n]+`)
	reIceOptions  = regexp.MustCompile(`a=ice-options:[^\r\n]+\r?\n`)
	reCandidate   = regexp.MustCompile(`a=candidate:[^\r\n]+\r?\n`)
	reEndCand     = regexp.MustCompile(`a=end-of-candidates\r?\n?`)
	reUfrag       = regexp.MustCompile(`a=ice-ufrag:([^\r\n]+)`)
)

func (m *SctpRelayManager) modifySdpForRelay(sdp string, info RelayConfig) string {
	out := reSetup.ReplaceAllString(sdp, "a=setup:passive")

	iceUfrag := info.AuthToken
	if iceUfrag == "" {
		iceUfrag = info.Token
	}
	out = reUfragLine.ReplaceAllString(out, "a=ice-ufrag:"+iceUfrag)
	out = rePwdLine.ReplaceAllString(out, "a=ice-pwd:"+info.Key)
	out = reFingerprint.ReplaceAllString(out, "a=fingerprint:"+core.WADTLSFingerprint)
	out = reMaxMsg.ReplaceAllString(out, "a=max-message-size:1500")
	out = reIceOptions.ReplaceAllString(out, "")

	out = reCandidate.ReplaceAllString(out, "")
	out = reEndCand.ReplaceAllString(out, "")
	candidate := fmt.Sprintf("a=candidate:2 1 udp 2122262783 %s %d typ host generation 0 network-cost 5", info.IP, info.Port)
	out += candidate + "\r\n" + "a=end-of-candidates" + "\r\n"
	return out
}

func extractFirst(re *regexp.Regexp, s string) string {
	if mm := re.FindStringSubmatch(s); len(mm) > 1 {
		return mm[1]
	}
	return ""
}

func (m *SctpRelayManager) sendStunRegistration(conn *relayConnection) {
	info := conn.info
	remoteUfrag := info.AuthToken
	if remoteUfrag == "" {
		remoteUfrag = info.Token
	}
	if remoteUfrag == "" {
		return
	}
	localUfrag := conn.localUfrag
	hmacKey := []byte(info.Key)

	send := func() {
		if conn.getState() != relayStateOpen || conn.channel == nil {
			return
		}
		ssrc := m.subscriptionSsrc.Load()
		if ssrc == 0 {
			ssrc = m.audioSsrc.Load()
		}
		if ssrc == 0 {
			return
		}
		subs := BuildSenderSubscriptions(ssrc)

		if localUfrag != "" {
			username := []byte(remoteUfrag + ":" + localUfrag)
			m.sendRaw(conn, BuildBindingRequestWithSubs(username, hmacKey, subs, true, true))
		}
		if info.Token != "" && info.Token != remoteUfrag && localUfrag != "" {
			username := []byte(info.Token + ":" + localUfrag)
			m.sendRaw(conn, BuildBindingRequestWithSubs(username, hmacKey, subs, true, true))
		}
		m.sendRaw(conn, BuildBindingRequestWithSubs(nil, nil, subs, false, false))

		if len(info.RawToken) > 0 {
			var peerSsrcs []uint32
			if sub := m.subscriptionSsrc.Load(); sub != 0 {
				peerSsrcs = []uint32{sub}
			}
			ssrcList := BuildSSRCSubscriptionList([]uint32{m.audioSsrc.Load()}, peerSsrcs, 0, 0)
			m.sendRaw(conn, BuildAllocateForRelay(info.RawToken, ssrcList, hmacKey, info.IP, info.Port))
		}
	}

	send()
	for _, d := range []time.Duration{50, 150, 500, 3000} {
		delay := d * time.Millisecond
		go func() {
			select {
			case <-time.After(delay):
				m.mu.Lock()
				open := conn.getState() == relayStateOpen
				m.mu.Unlock()
				if open {
					send()
				}
			case <-conn.stopCh:
			}
		}()
	}
}

func (m *SctpRelayManager) startKeepalive(conn *relayConnection) {
	m.sendRaw(conn, BuildWhatsAppPing())
	ticker := time.NewTicker(relayKeepaliveInterval)
	go func() {
		for {
			select {
			case <-ticker.C:
				if conn.getState() != relayStateOpen || conn.channel == nil {
					return
				}
				m.sendRaw(conn, BuildWhatsAppPing())
			case <-conn.stopCh:
				ticker.Stop()
				return
			}
		}
	}()
}

func (m *SctpRelayManager) sendRaw(conn *relayConnection, data []byte) {
	if conn.channel == nil || conn.getState() != relayStateOpen {
		return
	}
	if err := conn.channel.Send(data); err != nil {
		m.log.Debug("relay send error", "id", conn.id, "err", err)
	}
}

func (m *SctpRelayManager) Broadcast(data []byte) {
	m.mu.Lock()
	conns := make([]*relayConnection, 0, len(m.connections))
	for _, c := range m.connections {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	for _, c := range conns {
		m.sendRaw(c, data)
	}
}

// BufferedAmount reports the largest SCTP pending-buffer watermark across open
// relay connections (backpressure signal for a stalled browser/relay).
func (m *SctpRelayManager) BufferedAmount() uint64 {
	m.mu.Lock()
	conns := make([]*relayConnection, 0, len(m.connections))
	for _, c := range m.connections {
		conns = append(conns, c)
	}
	m.mu.Unlock()
	var maxBuf uint64
	for _, c := range conns {
		if c.getState() == relayStateOpen && c.channel != nil {
			if b := c.channel.BufferedAmount(); b > maxBuf {
				maxBuf = b
			}
		}
	}
	return maxBuf
}

func (m *SctpRelayManager) HasConnection() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.connections {
		if c.getState() == relayStateOpen {
			return true
		}
	}
	return false
}

func (m *SctpRelayManager) ConnectedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.connections {
		if c.getState() == relayStateOpen {
			n++
		}
	}
	return n
}

func (m *SctpRelayManager) failConnection(conn *relayConnection) {
	m.mu.Lock()
	if conn.getState() == relayStateFailed {
		m.mu.Unlock()
		return
	}
	conn.setState(relayStateFailed)
	// Identity-checked delete: drop the entry only if it still points at
	// THIS conn. A late failure from an already-removed conn (stale ICE,
	// grace or watchdog callback) must not evict a re-dialed conn that now
	// occupies the same id. The state transition above always applies to the
	// caller's conn; when the delete is skipped the map keeps its live
	// occupant, so noteConnectionLost sees len>0 and does not fire.
	if cur, ok := m.connections[conn.id]; ok && cur == conn {
		delete(m.connections, conn.id)
	}
	m.mu.Unlock()
	m.noteConnectionLost()
	m.teardown(conn)
}

// closeConnection resolves conn by id FIRST and then applies the state
// transition and the delete to that same looked-up conn inside a single
// critical section, so state and delete can never diverge (unlike
// failConnection, which is handed a caller-owned pointer). No caller-pointer
// mismatch class of bug exists here — confirmed, no change needed.
func (m *SctpRelayManager) closeConnection(id string) {
	m.mu.Lock()
	conn := m.connections[id]
	if conn == nil {
		m.mu.Unlock()
		return
	}
	conn.setState(relayStateClosed)
	delete(m.connections, id)
	m.mu.Unlock()
	m.noteConnectionLost()
	m.teardown(conn)
}

func (m *SctpRelayManager) teardown(conn *relayConnection) {
	conn.teardownOnce.Do(func() {
		close(conn.stopCh)
		// The keepalive goroutine owns its ticker: it stops it when stopCh
		// closes (or never starts if OnOpen never fired), so there is no
		// ticker field to race with here.
		conn.cancelIceGrace()
		if conn.channel != nil {
			_ = conn.channel.Close()
		}
		if conn.pc != nil {
			_ = conn.pc.Close()
		}
	})
}

func (m *SctpRelayManager) Cleanup() {
	m.mu.Lock()
	conns := make([]*relayConnection, 0, len(m.connections))
	for _, c := range m.connections {
		conns = append(conns, c)
	}
	m.connections = map[string]*relayConnection{}
	m.mu.Unlock()
	m.audioSsrc.Store(0)
	m.subscriptionSsrc.Store(0)
	for _, c := range conns {
		m.teardown(c)
	}
}
