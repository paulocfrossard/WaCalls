package main

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"wacalls/internal/voip/call"
)

// newWebRTCTestServer wires a server + one session with an injectable
// newBridge, so doWebRTC can be exercised end to end without a real peer
// connection (the zero-value Bridge is Close/Detach-safe).
func newWebRTCTestServer(t *testing.T) (*server, *Session) {
	t.Helper()
	broker := NewBroker()
	mgr := &SessionManager{
		broker:   broker,
		log:      slog.Default(),
		sessions: map[string]*Session{},
	}
	sess := &Session{
		id:    "s1",
		mgr:   mgr,
		log:   slog.Default(),
		reg:   newCallRegistry(),
		grace: newGraceKeeper(browserGraceWindow, func(string) {}),
	}
	mgr.sessions["s1"] = sess

	srv := &server{broker: broker, sessions: mgr, log: slog.Default()}
	srv.newBridge = func(offerSDP string, log *slog.Logger) (*Bridge, string, error) {
		return &Bridge{}, "v=0-fake-answer", nil
	}
	return srv, sess
}

// A webrtc POST for a callId that is not in the registry must be a 404, never
// a 200 carrying an sdp_answer for a call that does not exist.
func TestWebRTCUnknownCallIs404(t *testing.T) {
	srv, _ := newWebRTCTestServer(t)
	h := srv.routes()

	rec := postJSON(t, h, "/api/sessions/s1/calls/nope/webrtc", map[string]string{"sdp_offer": "v=0"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown call: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sdp_answer") {
		t.Fatalf("404 must not carry an SDP answer: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no such call") {
		t.Fatalf("404 body must say no such call: %s", rec.Body.String())
	}
}

// QA M4b regression: the call exists at the registry check but vanishes
// (endCall/removeCall from another goroutine) before Session.setBridge runs.
// setBridge reports false and the handler must answer 404, not 200 with an
// SDP answer for the bridge it just closed.
func TestWebRTC404WhenCallVanishesBeforeSetBridge(t *testing.T) {
	srv, sess := newWebRTCTestServer(t)
	cm := call.NewCallManagerWithDeps(call.Deps{})
	sess.reg.add("c1", &activeCall{cm: cm})
	srv.broker.upsertCall(CallRecord{SessionID: "s1", CallID: "c1", Status: StatusRinging})

	// The injected bridge factory plays the concurrent teardown: the call is
	// removed from the registry between the handler's reg.get and setBridge.
	srv.newBridge = func(offerSDP string, log *slog.Logger) (*Bridge, string, error) {
		sess.removeCall("c1")
		srv.broker.endCall("s1", "c1", "user_ended")
		return &Bridge{}, "v=0-fake-answer", nil
	}
	h := srv.routes()

	rec := postJSON(t, h, "/api/sessions/s1/calls/c1/webrtc", map[string]string{"sdp_offer": "v=0"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("vanished call: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sdp_answer") {
		t.Fatalf("a vanished call must not get an SDP answer: %s", rec.Body.String())
	}
	if _, ok := sess.reg.get("c1"); ok {
		t.Fatal("the vanished call must stay out of the registry")
	}
}
