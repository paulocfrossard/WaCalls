package logbuf

import (
	"context"
	"sync"
	"time"
)

// DupDetector watches WritePCM timestamps per call and warns when two writes
// land closer together than closeGap. Audio frames are 960 samples @16 kHz
// (60 ms), so the healthy cadence is ~60 ms between WritePCM calls; two writes
// within <50 ms means two callbacks fired for the same frame (OnPeerAudio
// registered twice / two bridges writing) — i.e. the "ouvi o áudio duplicado"
// symptom.
type DupDetector struct {
	mu       sync.Mutex
	writes   map[string][]time.Time // callID → recent WritePCM timestamps
	lastEmit map[string]time.Time   // callID → when we last warned (cooldown)
	now      func() time.Time

	window   time.Duration // lookback for candidate pairs (2s)
	closeGap time.Duration // pair closer than this is a duplicate (<50ms)
	interval time.Duration // check cadence (1s)
}

// NewDupDetector returns a detector with the default windows.
func NewDupDetector() *DupDetector {
	return &DupDetector{
		writes:   map[string][]time.Time{},
		lastEmit: map[string]time.Time{},
		now:      time.Now,
		window:   2 * time.Second,
		closeGap: 50 * time.Millisecond,
		interval: time.Second,
	}
}

var defaultDup = NewDupDetector()

// RecordWritePCM feeds one WritePCM timestamp for callID into the
// process-wide detector. No-op when the debug log is disabled.
func RecordWritePCM(callID string) {
	if !Enabled() || callID == "" {
		return
	}
	defaultDup.Record(callID)
}

// StartDupDetector runs the process-wide detector until ctx is cancelled.
// Safe to run while logging is disabled (it just idles).
func StartDupDetector(ctx context.Context) { defaultDup.Run(ctx) }

// Record stores a WritePCM timestamp for callID.
func (d *DupDetector) Record(callID string) {
	if callID == "" {
		return
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes[callID] = append(d.writes[callID], now)
}

// Run ticks every interval and checks the recorded timestamps until ctx ends.
func (d *DupDetector) Run(ctx context.Context) {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Check()
		}
	}
}

// Check scans the last window of timestamps per call and emits a warn event
// (source=dup-detector) for the first pair closer than closeGap, rate-limited
// to one warning per callID per window.
func (d *DupDetector) Check() {
	if !Enabled() {
		return
	}
	now := d.now()
	cut := now.Add(-d.window)
	d.mu.Lock()
	defer d.mu.Unlock()
	for callID, ts := range d.writes {
		kept := make([]time.Time, 0, len(ts))
		for _, t := range ts {
			if !t.Before(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(d.writes, callID)
			delete(d.lastEmit, callID)
			continue
		}
		d.writes[callID] = kept

		var a, b time.Time
		found := false
		for i := 1; i < len(kept); i++ {
			if kept[i].Sub(kept[i-1]) < d.closeGap {
				a, b = kept[i-1], kept[i]
				found = true
				break
			}
		}
		if !found {
			continue
		}
		if last, ok := d.lastEmit[callID]; ok && now.Sub(last) < d.window {
			continue // cooldown: one warn per callID per 2s
		}
		d.lastEmit[callID] = now
		LogEvent("dup-detector", LevelWarn,
			"two WritePCM calls closer than 50ms for the same call (duplicated audio path?)",
			map[string]any{
				"call_id":   callID,
				"ts_first":  a.UTC().Format(time.RFC3339Nano),
				"ts_second": b.UTC().Format(time.RFC3339Nano),
				"gap_ms":    Round4(float64(b.Sub(a).Microseconds()) / 1000.0),
			})
	}
}
