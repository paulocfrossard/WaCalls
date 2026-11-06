package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wacalls/internal/voip/call"
)

// newOwnerTestServer wires a minimal server + one session + one live call so
// the HTTP ownership/session guards can be exercised end to end.
func newOwnerTestServer(t *testing.T) (*httptest.Server, *Session) {
	t.Helper()
	broker := NewBroker()
	mgr := &SessionManager{
		broker:   broker,
		log:      slog.Default(),
		sessions: map[string]*Session{},
	}
	sess := &Session{
		id:  "s1",
		mgr: mgr,
		log: slog.Default(),
		reg: newCallRegistry(),
	}
	mgr.sessions["s1"] = sess

	// A live-but-unringing call: CanAcceptID/AlreadyAccepted are both false,
	// which is what the accept guard must reject without touching.
	cm := call.NewCallManagerWithDeps(call.Deps{})
	sess.reg.add("c1", &activeCall{cm: cm, admitted: true})
	broker.upsertCall(CallRecord{SessionID: "s1", CallID: "c1", Status: StatusRinging})

	srv := &server{broker: broker, sessions: mgr, log: slog.Default()}
	return httptest.NewServer(srv.routes()), sess
}

func doReq(t *testing.T, method, url, clientID string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clientID != "" {
		req.Header.Set("X-Client-Id", clientID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

// Foreign-session callIds must 404 before any broker mutation: ending another
// session's call through this handler is an N-isolation break.
func TestEndCallForeignSessionIs404(t *testing.T) {
	ts, _ := newOwnerTestServer(t)
	defer ts.Close()

	code, body := doReq(t, http.MethodDelete, ts.URL+"/api/sessions/sX/calls/c1", "op-A")
	if code != http.StatusNotFound {
		t.Fatalf("foreign session: status=%d body=%s", code, body)
	}
}

// A call claimed by another operator must 409 with a machine-readable code.
func TestEndCallClaimedByOtherIs409(t *testing.T) {
	ts, sess := newOwnerTestServer(t)
	defer ts.Close()

	if !sess.mgr.broker.setOwner("s1", "c1", "op-B") {
		t.Fatal("setup: setOwner failed")
	}
	code, body := doReq(t, http.MethodDelete, ts.URL+"/api/sessions/s1/calls/c1", "op-A")
	if code != http.StatusConflict || !strings.Contains(body, `"code":"call_claimed"`) {
		t.Fatalf("status=%d body=%s", code, body)
	}
}

// Unclaimed calls can be ended by anyone (LAN trust): 204 + slot released.
func TestEndCallUnclaimedIs204AndReleasesSlot(t *testing.T) {
	ts, sess := newOwnerTestServer(t)
	defer ts.Close()

	if !sess.mgr.broker.tryAdmit() {
		t.Fatal("setup: tryAdmit failed")
	}
	code, body := doReq(t, http.MethodDelete, ts.URL+"/api/sessions/s1/calls/c1", "op-A")
	if code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if _, ok := sess.reg.get("c1"); ok {
		t.Fatal("call must be removed from the registry")
	}
	if n := sess.mgr.broker.ActiveCallCount(); n != 0 {
		t.Fatalf("admission not released: active=%d", n)
	}
}

// Accepting a call that is not ringing must 409 with not_acceptable — and
// must NOT end or claim the call (this was a QA-round regression).
func TestAcceptNotRingingIs409WithoutTeardown(t *testing.T) {
	ts, sess := newOwnerTestServer(t)
	defer ts.Close()

	code, body := doReq(t, http.MethodPost, ts.URL+"/api/sessions/s1/calls/c1/accept", "op-A")
	if code != http.StatusConflict || !strings.Contains(body, `"code":"not_acceptable"`) {
		t.Fatalf("status=%d body=%s", code, body)
	}
	if _, ok := sess.reg.get("c1"); !ok {
		t.Fatal("reject of a non-ringing call must not tear it down")
	}
	if rec, ok := sess.mgr.broker.getCall("s1", "c1"); !ok || rec.Status == StatusEnded {
		t.Fatalf("broker record must stay alive: %+v", rec)
	}
}
