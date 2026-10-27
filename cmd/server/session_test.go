package main

import (
	"context"
	"errors"
	"net"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/socket"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// F2 invariants 1-5, exercised through the pure decision function.
func TestDecideConnectInvariants(t *testing.T) {
	cases := []struct {
		name     string
		state    string
		hasCreds bool
		qrActive bool
		want     pairingDecision
	}{
		// inv 1: fresh session => QR channel opens.
		{"inv1 fresh session pairs", "connecting", false, false, decisionPair},
		// inv 5: T2 restores unscanned sessions (auth store intact, no user
		// ID) in a non-open state => pairable.
		{"inv5 restored unscanned pairs", "qr", false, false, decisionPair},
		{"inv5 restored connecting without creds pairs", "connecting", false, false, decisionPair},
		// inv 2: credentials + non-terminal state => auto-Connect, never QR
		// (the hard regression: boot must not force a re-pair).
		{"inv2 connecting with creds connects", "connecting", true, false, decisionConnect},
		{"inv2 open with creds connects", "open", true, false, decisionConnect},
		{"inv2 connecting with creds and live qr connects", "connecting", true, true, decisionConnect},
		// inv 3: terminal / stuck states => fresh QR without delete+recreate.
		{"inv3 logged_out pairs", "logged_out", true, false, decisionPair},
		{"inv3 logged_out without creds pairs", "logged_out", false, false, decisionPair},
		{"inv3 stuck qr without channel pairs", "qr", true, false, decisionPair},
		// inv 4: qr state with a live channel must not open a second one.
		{"inv4 qr with live channel connects", "qr", true, true, decisionConnect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decideConnect(tc.state, tc.hasCreds, tc.qrActive); got != tc.want {
				t.Fatalf("decideConnect(%q, creds=%v, qrActive=%v) = %v, want %v",
					tc.state, tc.hasCreds, tc.qrActive, got, tc.want)
			}
		})
	}
}

// F1: a LoggedOut event must stop the reconnect loop and publish the
// terminal state with Paired cleared.
func TestLoggedOutStopsReconnect(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "logout")
	s.client.EnableAutoReconnect = true
	s.setAuth(AuthSnapshot{State: "open", Paired: true})

	s.handleEvent(&events.LoggedOut{})

	if s.client.EnableAutoReconnect {
		t.Fatal("EnableAutoReconnect must be disabled after LoggedOut (F1)")
	}
	info := s.info()
	if info.State != "logged_out" {
		t.Fatalf("state = %q, want logged_out", info.State)
	}
	if info.Paired {
		t.Fatal("session must not report paired after LoggedOut")
	}
}

// inv 4: only one QR-channel claim per session, and a stale release must not
// free a newer claim.
func TestPairingSlotSingleChannel(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "pair-slot")

	gen, ok := s.beginPairing()
	if !ok {
		t.Fatal("first pairing claim must succeed")
	}
	if _, ok := s.beginPairing(); ok {
		t.Fatal("second concurrent pairing claim must be refused (inv 4)")
	}

	// replaceClient invalidates the in-flight pairing.
	s.replaceClient(whatsmeow.NewClient(m.container.NewDevice(), waLog.Noop))
	if _, ok := s.beginPairing(); !ok {
		t.Fatal("replaceClient must release the pairing slot")
	}
	s.endPairing(gen) // stale release from the superseded pairing
	if _, ok := s.beginPairing(); ok {
		t.Fatal("stale endPairing must not free a newer claim")
	}

	s.endPairing(s.qrGen)
	if _, ok := s.beginPairing(); !ok {
		t.Fatal("slot must be free after endPairing")
	}
}

// connectFailed keeps credentials on transient transport failures and treats
// an already-connected client as success (no QR, no credential wipe).
func TestConnectFailedKeepsCredentialsOnTransientErrors(t *testing.T) {
	m := newTestManager(t)
	s := m.addUnconnected(t, "transient")

	err := s.connectFailed(context.Background(), whatsmeow.ErrAlreadyConnected)
	if err != nil {
		t.Fatalf("ErrAlreadyConnected must be treated as success, got %v", err)
	}

	transient := []error{
		socket.ErrDialFailed,
		context.DeadlineExceeded,
		&net.DNSError{Err: "no such host", Name: "web.whatsapp.com", IsNotFound: true},
	}
	for _, e := range transient {
		if !isTransientConnectError(e) {
			t.Fatalf("expected transient: %v", e)
		}
	}

	// Unknown failures are treated as permanent so inv 3 still surfaces a QR.
	if isTransientConnectError(errors.New("noise handshake failed")) {
		t.Fatal("unknown connect errors must fall back to the QR path (inv 3)")
	}

	// The transient branch must return the original error untouched.
	want := socket.ErrDialFailed
	if got := s.connectFailed(context.Background(), want); !errors.Is(got, want) {
		t.Fatalf("transient connect failure = %v, want %v", got, want)
	}

	// Transient failure must not wipe credentials or open a QR channel:
	// connectFailed must not call replaceClient/startPairing, so qrActive
	// stays false and state is unchanged (LOW-3).
	s2 := m.addUnconnected(t, "transient-keep")
	s2.setAuth(AuthSnapshot{State: "open", Paired: true})
	beforeClient := s2.client
	beforeState := s2.info().State
	beforeQR := s2.qrActive
	if got := s2.connectFailed(context.Background(), socket.ErrDialFailed); !errors.Is(got, socket.ErrDialFailed) {
		t.Fatalf("transient connect failure = %v, want %v", got, socket.ErrDialFailed)
	}
	if s2.client != beforeClient {
		t.Fatal("transient failure must not replace the client (LOW-3)")
	}
	if s2.qrActive != beforeQR {
		t.Fatal("transient failure must not claim the QR slot (LOW-3)")
	}
	if s2.info().State != beforeState {
		t.Fatalf("transient failure changed state: %q -> %q (LOW-3)", beforeState, s2.info().State)
	}
}
