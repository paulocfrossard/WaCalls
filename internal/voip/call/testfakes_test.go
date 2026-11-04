package call

import (
	"context"
	"sync"
	"testing"
	"time"

	"wacalls/internal/voip/media"
	"wacalls/internal/voip/transport"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
)

// fakeVoipSocket implements core.VoipSocket for unit tests.
type fakeVoipSocket struct {
	mu sync.Mutex

	ownLID types.JID
	ownPN  types.JID

	sent      []waBinary.Node
	queried   []waBinary.Node
	queryResp *waBinary.Node
	queryErr  error

	usyncDevices     []types.JID
	assertErr        error
	participantNodes []waBinary.Node
	participantErr   error
	tcToken          []byte
	tcErr            error
	identityNode     waBinary.Node
	identityOk       bool

	resolvedLID types.JID

	createdCalls int
}

func newFakeSocket() *fakeVoipSocket {
	return &fakeVoipSocket{
		ownLID: types.NewJID("123456789012345", types.HiddenUserServer),
		ownPN:  types.NewJID("5511999999999", types.DefaultUserServer),
	}
}

func (f *fakeVoipSocket) OwnPN() types.JID  { return f.ownPN }
func (f *fakeVoipSocket) OwnLID() types.JID { return f.ownLID }

func (f *fakeVoipSocket) AccountDeviceIdentityNode() (waBinary.Node, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.identityNode, f.identityOk
}

func (f *fakeVoipSocket) SendNode(ctx context.Context, node waBinary.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, node)
	return nil
}

func (f *fakeVoipSocket) Query(ctx context.Context, node waBinary.Node) (*waBinary.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queried = append(f.queried, node)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.queryResp != nil {
		cp := *f.queryResp
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeVoipSocket) GetUSyncDevices(ctx context.Context, jids []types.JID) ([]types.JID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usyncDevices != nil {
		out := make([]types.JID, len(f.usyncDevices))
		copy(out, f.usyncDevices)
		return out, nil
	}
	out := make([]types.JID, len(jids))
	copy(out, jids)
	return out, nil
}

func (f *fakeVoipSocket) AssertSessions(ctx context.Context, jids []types.JID, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.assertErr
}

func (f *fakeVoipSocket) CreateParticipantNodes(ctx context.Context, devices []types.JID, callKey []byte, encAttrs waBinary.Attrs) ([]waBinary.Node, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.participantErr != nil {
		return nil, false, f.participantErr
	}
	if f.participantNodes != nil {
		out := make([]waBinary.Node, len(f.participantNodes))
		copy(out, f.participantNodes)
		return out, false, nil
	}
	out := make([]waBinary.Node, len(devices))
	for i := range devices {
		out[i] = waBinary.Node{Tag: "to", Attrs: waBinary.Attrs{"jid": devices[i]}}
	}
	return out, false, nil
}

func (f *fakeVoipSocket) DecryptCallKey(ctx context.Context, from types.JID, encChild *waBinary.Node) ([]byte, error) {
	return media.GenerateCallKey(), nil
}

func (f *fakeVoipSocket) GetTCToken(ctx context.Context, jid types.JID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tcErr != nil {
		return nil, f.tcErr
	}
	if f.tcToken != nil {
		out := make([]byte, len(f.tcToken))
		copy(out, f.tcToken)
		return out, nil
	}
	return nil, nil
}

func (f *fakeVoipSocket) ResolveLIDForPN(ctx context.Context, pn types.JID) types.JID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.resolvedLID.IsEmpty() {
		return f.resolvedLID
	}
	return pn
}

// fakeRelayTransport implements transport.RelayTransport for tests.
type fakeRelayTransport struct {
	mu sync.Mutex

	connected   bool
	connCount   int
	ssrc        uint32
	subSsrc     uint32
	receiveFn   func([]byte)
	connectedFn func(string, int)

	broadcasts  [][]byte
	cleaned     bool
	resendCount int
}

func newFakeRelay() *fakeRelayTransport { return &fakeRelayTransport{} }

func (f *fakeRelayTransport) SetSsrc(ssrc uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ssrc = ssrc
}
func (f *fakeRelayTransport) SetSubscriptionSsrc(ssrc uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subSsrc = ssrc
}
func (f *fakeRelayTransport) SetOnConnected(fn func(string, int)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectedFn = fn
}
func (f *fakeRelayTransport) SetOnReceive(fn func([]byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receiveFn = fn
}
func (f *fakeRelayTransport) ResendSubscriptions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resendCount++
}
func (f *fakeRelayTransport) ConfigureRelays(relays []transport.RelayConfig) {}
func (f *fakeRelayTransport) Broadcast(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.broadcasts = append(f.broadcasts, cp)
}
func (f *fakeRelayTransport) HasConnection() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}
func (f *fakeRelayTransport) ConnectedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connCount
}
func (f *fakeRelayTransport) Cleanup() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleaned = true
	f.connected = false
	f.connCount = 0
}

// Deliver feeds data into the installed onReceive callback (simulates a relay
// delivering a packet to the CallManager).
func (f *fakeRelayTransport) Deliver(data []byte) {
	f.mu.Lock()
	fn := f.receiveFn
	f.mu.Unlock()
	if fn != nil {
		fn(data)
	}
}

// passthroughCodec is a media.Codec that returns samples unchanged, for
// byte-exact isolation assertions.
type passthroughCodec struct{ frame int }

func (p *passthroughCodec) FrameSize() int {
	if p.frame == 0 {
		return 960
	}
	return p.frame
}
func (p *passthroughCodec) Encode(pcm []float32) ([]byte, error) {
	out := make([]byte, len(pcm)*2)
	// simple deterministic byte representation: scale to int16.
	for i, v := range pcm {
		iv := int16(v * 32767)
		out[i*2] = byte(iv)
		out[i*2+1] = byte(iv >> 8)
	}
	return out, nil
}
func (p *passthroughCodec) Decode(b []byte) ([]float32, error) {
	out := make([]float32, len(b)/2)
	for i := range out {
		v := int16(b[i*2]) | int16(b[i*2+1])<<8
		out[i] = float32(v) / 32767.0
	}
	return out, nil
}
func (p *passthroughCodec) SampleRate() int { return 16000 }
func (p *passthroughCodec) Close()          {}

// newTestManager builds a CallManager wired with the given fakes and a current
// call, ready for media tests. The caller must set selfSsrc/peerSsrcs etc.
func newTestManager(t *testing.T, sock *fakeVoipSocket, relay *fakeRelayTransport, codec media.Codec) *CallManager {
	t.Helper()
	m := NewCallManagerWithDeps(Deps{Sock: sock, Log: nil, Relay: relay, Codec: codec})
	if m.relay == nil {
		t.Fatal("relay not set")
	}
	return m
}

// unused guards
var _ = time.Now
var _ = context.Background
var _ = transport.RelayConfig{}
var _ media.Codec = (*passthroughCodec)(nil)
