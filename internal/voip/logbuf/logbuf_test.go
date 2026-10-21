package logbuf

import (
	"sync"
	"testing"
	"time"
)

func TestRingEvictsOldestAndKeepsSeq(t *testing.T) {
	b := NewBuffer(3)
	for i := 0; i < 5; i++ {
		b.Add("src", LevelInfo, "m", map[string]any{"i": i})
	}
	if b.Len() != 3 {
		t.Fatalf("len = %d, want 3", b.Len())
	}
	events, next := b.EventsSince(0, 100)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// The two oldest (seq 1,2) were evicted; seq continues past them.
	if events[0].Seq != 3 || events[2].Seq != 5 {
		t.Fatalf("unexpected seqs: %d,%d,%d", events[0].Seq, events[1].Seq, events[2].Seq)
	}
	if next != 5 {
		t.Fatalf("nextSeq = %d, want 5", next)
	}
}

func TestEventsSincePagination(t *testing.T) {
	b := NewBuffer(10)
	for i := 0; i < 7; i++ {
		b.Add("src", LevelDebug, "m", nil)
	}
	page1, next1 := b.EventsSince(0, 3)
	if len(page1) != 3 || next1 != 3 {
		t.Fatalf("page1: %d events, next=%d", len(page1), next1)
	}
	page2, next2 := b.EventsSince(next1, 3)
	if len(page2) != 3 || next2 != 6 {
		t.Fatalf("page2: %d events, next=%d", len(page2), next2)
	}
	page3, next3 := b.EventsSince(next2, 3)
	if len(page3) != 1 || next3 != 7 {
		t.Fatalf("page3: %d events, next=%d", len(page3), next3)
	}
	page4, next4 := b.EventsSince(next3, 3)
	if len(page4) != 0 || next4 != 7 {
		t.Fatalf("page4: %d events, next=%d (cursor must not regress)", len(page4), next4)
	}
}

func TestEventsSinceNeverReturnsNil(t *testing.T) {
	b := NewBuffer(4)
	events, next := b.EventsSince(0, 10)
	if events == nil {
		t.Fatal("events must be non-nil so JSON serializes as []")
	}
	if len(events) != 0 || next != 0 {
		t.Fatalf("expected empty page, got %d events next=%d", len(events), next)
	}
}

func TestDisabledBufferDropsEvents(t *testing.T) {
	b := NewBuffer(10)
	b.SetEnabled(false)
	b.Add("src", LevelInfo, "dropped", nil)
	if b.Len() != 0 {
		t.Fatal("disabled buffer must not store events")
	}
	b.SetEnabled(true)
	b.Add("src", LevelInfo, "kept", nil)
	if b.Len() != 1 {
		t.Fatal("enabled buffer must store events")
	}
}

func TestEventFieldContract(t *testing.T) {
	b := NewBuffer(4)
	fields := map[string]any{"call_id": "C1", "len": 1600, "peak": 0.25}
	b.Add("uplink-mic", LevelDebug, "browser pcm captured", fields)
	events, _ := b.EventsSince(0, 1)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Source != "uplink-mic" || ev.Level != "debug" || ev.Msg != "browser pcm captured" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Seq != 1 {
		t.Fatalf("seq = %d, want 1", ev.Seq)
	}
	if ev.TS.IsZero() || ev.TS.Location() != time.UTC {
		t.Fatalf("ts must be UTC and set, got %v", ev.TS)
	}
	if ev.Fields["call_id"] != "C1" || ev.Fields["len"] != 1600 {
		t.Fatalf("unexpected fields: %v", ev.Fields)
	}
}

func TestLimiterRateLimit(t *testing.T) {
	l := NewLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if !l.Allow("k", 5) {
			t.Fatalf("call %d within the per-second budget must pass", i+1)
		}
	}
	if l.Allow("k", 5) {
		t.Fatal("6th call in the same second must be limited")
	}
	if !l.Allow("other", 5) {
		t.Fatal("a different key must not share the budget")
	}
	now = now.Add(1100 * time.Millisecond)
	if !l.Allow("k", 5) {
		t.Fatal("budget must reset after one second")
	}
}

func TestLimiterRejectsNonPositiveBudget(t *testing.T) {
	l := NewLimiter()
	if l.Allow("k", 0) || l.Allow("k", -1) {
		t.Fatal("non-positive perSec must never allow")
	}
}

func TestPCMStats(t *testing.T) {
	rms, peak := PCMStats(nil)
	if rms != 0 || peak != 0 {
		t.Fatalf("empty frame: rms=%v peak=%v", rms, peak)
	}
	rms, peak = PCMStats([]float32{0.5, -0.5, 0.25, -0.25})
	if peak != 0.5 {
		t.Fatalf("peak = %v, want 0.5", peak)
	}
	// rms = sqrt((0.25+0.25+0.0625+0.0625)/4) = sqrt(0.15625)
	want := 0.3952847
	if rms < want-0.001 || rms > want+0.001 {
		t.Fatalf("rms = %v, want ~%v", rms, want)
	}
}

func TestRound4(t *testing.T) {
	if got := Round4(0.123456789); got != 0.1235 {
		t.Fatalf("Round4 = %v", got)
	}
	if got := Round4(1.00004); got != 1.0 {
		t.Fatalf("Round4 = %v", got)
	}
}

func TestDupDetectorFlagsClosePair(t *testing.T) {
	Default().Reset()
	Default().SetEnabled(true)
	d := NewDupDetector()
	base := time.Now()
	now := base
	d.now = func() time.Time { return now }

	d.Record("call-1")
	now = base.Add(2 * time.Millisecond) // same tick → duplicate
	d.Record("call-1")
	now = base.Add(10 * time.Millisecond)
	d.Check()

	events, _ := Default().EventsSince(0, 100)
	found := false
	for _, e := range events {
		if e.Source == "dup-detector" && e.Level == LevelWarn && e.Fields["call_id"] == "call-1" {
			found = true
			if e.Fields["ts_first"] == "" || e.Fields["ts_second"] == "" {
				t.Fatalf("warn event must carry both timestamps: %v", e.Fields)
			}
		}
	}
	if !found {
		t.Fatal("expected a dup-detector warn event for the close pair")
	}
	Default().Reset()
}

func TestDupDetectorIgnoresNormalCadence(t *testing.T) {
	Default().Reset()
	Default().SetEnabled(true)
	d := NewDupDetector()
	base := time.Now()
	now := base
	d.now = func() time.Time { return now }

	// Healthy cadence: 60ms frames (960 samples @16kHz).
	for i := 0; i < 10; i++ {
		d.Record("call-1")
		now = now.Add(60 * time.Millisecond)
	}
	d.Check()

	events, _ := Default().EventsSince(0, 100)
	for _, e := range events {
		if e.Source == "dup-detector" {
			t.Fatalf("normal 60ms cadence must not warn, got: %+v", e)
		}
	}
	Default().Reset()
}

func TestDupDetectorCooldownPreventsSpam(t *testing.T) {
	Default().Reset()
	Default().SetEnabled(true)
	d := NewDupDetector()
	base := time.Now()
	now := base
	d.now = func() time.Time { return now }

	d.Record("call-1")
	now = base.Add(1 * time.Millisecond)
	d.Record("call-1")
	now = base.Add(10 * time.Millisecond)
	d.Check() // warns
	now = now.Add(time.Second)
	d.Record("call-1")
	now = now.Add(1 * time.Millisecond)
	d.Record("call-1")
	now = now.Add(10 * time.Millisecond)
	d.Check() // same pair still inside the 2s cooldown

	events, _ := Default().EventsSince(0, 100)
	warns := 0
	for _, e := range events {
		if e.Source == "dup-detector" {
			warns++
		}
	}
	if warns != 1 {
		t.Fatalf("expected exactly 1 warn within the cooldown, got %d", warns)
	}
	Default().Reset()
}

func TestDupDetectorRequiresCallID(t *testing.T) {
	d := NewDupDetector()
	d.Record("")
	d.Record("call-1")
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.writes[""]; ok {
		t.Fatal("empty callID must not be recorded")
	}
	if len(d.writes["call-1"]) != 1 {
		t.Fatal("expected one timestamp for call-1")
	}
}

func TestConcurrentAddIsRaceFree(t *testing.T) {
	b := NewBuffer(100)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				b.Add("src", LevelDebug, "m", map[string]any{"g": g})
				b.EventsSince(0, 10)
			}
		}(g)
	}
	wg.Wait()
	if b.Len() != 100 {
		t.Fatalf("len = %d, want 100", b.Len())
	}
	events, _ := b.EventsSince(0, 100)
	// Seq must be unique and ordered after a concurrent storm.
	seen := make(map[int64]bool, len(events))
	for i, e := range events {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
		if i > 0 && e.Seq <= events[i-1].Seq {
			t.Fatalf("seqs out of order: %d then %d", events[i-1].Seq, e.Seq)
		}
	}
}

func TestGlobalEnabledToggle(t *testing.T) {
	Default().Reset()
	SetEnabled(false)
	LogEvent("src", LevelInfo, "dropped", nil)
	if Default().Len() != 0 {
		t.Fatal("global LogEvent must respect SetEnabled(false)")
	}
	SetEnabled(true)
	LogEvent("src", LevelInfo, "kept", nil)
	if Default().Len() != 1 {
		t.Fatal("global LogEvent must respect SetEnabled(true)")
	}
	Default().Reset()
}
