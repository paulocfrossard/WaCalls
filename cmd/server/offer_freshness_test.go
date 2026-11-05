package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestIsStaleOffer(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name             string
		offerTS          time.Time
		offlineReplaying bool
		want             bool
	}{
		{"fresh live offer", now.Add(-1 * time.Second), false, false},
		{"old buffered offer", now.Add(-5 * time.Minute), false, true},
		{"just under threshold", now.Add(-(staleOfferThreshold - time.Second)), false, false},
		{"just over threshold", now.Add(-(staleOfferThreshold + time.Second)), false, true},
		{"zero timestamp, live", time.Time{}, false, false},
		{"zero timestamp during replay", time.Time{}, true, true},
		{"fresh timestamp during replay is still dropped", now.Add(-1 * time.Second), true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isStaleOffer(c.offerTS, c.offlineReplaying, now); got != c.want {
				t.Fatalf("isStaleOffer(%v, replaying=%v) = %v, want %v", c.offerTS, c.offlineReplaying, got, c.want)
			}
		})
	}
}

func waitForBool(t *testing.T, get func() bool, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for value to become %v", want)
}

// A stale backstop timer from an earlier OfflineSyncPreview must not close the
// replay window opened by a later, still-active one.
func TestReplayWindowGenerationTokenSurvivesConsecutivePreviews(t *testing.T) {
	s := &Session{replayWindow: 300 * time.Millisecond}

	s.handleEvent(&events.OfflineSyncPreview{}) // gen 1
	time.Sleep(80 * time.Millisecond)
	s.handleEvent(&events.OfflineSyncPreview{}) // gen 2

	// At ~320ms total, gen 1's stale timer has fired but must not clear the
	// flag, since gen 2 is now current.
	time.Sleep(240 * time.Millisecond)
	if !s.offlineReplaying.Load() {
		t.Fatal("gen 1's stale backstop timer incorrectly closed gen 2's active replay window")
	}

	waitForBool(t, s.offlineReplaying.Load, false, time.Second)
}

// OfflineSyncCompleted closes the window immediately, without waiting for the
// backstop.
func TestReplayWindowClosesOnOfflineSyncCompleted(t *testing.T) {
	s := &Session{replayWindow: time.Hour}

	s.handleEvent(&events.OfflineSyncPreview{})
	if !s.offlineReplaying.Load() {
		t.Fatal("OfflineSyncPreview should open the replay window")
	}
	s.handleEvent(&events.OfflineSyncCompleted{})
	if s.offlineReplaying.Load() {
		t.Fatal("OfflineSyncCompleted should close the replay window immediately")
	}
}
