package logbuf

import (
	"context"
	"math"
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
	writes   map[string][]writeSample // callID → recent WritePCM samples
	lastEmit map[string]time.Time     // callID → when we last warned (cooldown)
	now      func() time.Time

	window   time.Duration // lookback for candidate pairs (2s)
	closeGap time.Duration // pair closer than this is a duplicate (<50ms)
	interval time.Duration // check cadence (1s)
}

// NewDupDetector returns a detector with the default windows.
func NewDupDetector() *DupDetector {
	return &DupDetector{
		writes:   map[string][]writeSample{},
		lastEmit: map[string]time.Time{},
		now:      time.Now,
		window:   2 * time.Second,
		closeGap: 50 * time.Millisecond,
		interval: time.Second,
	}
}

var defaultDup = NewDupDetector()

// writeSample is one WritePCM call: when the PCM payload is known it is
// fingerprinted so a legitimate jitter-buffer catch-up burst (different frames
// released back-to-back) is not mistaken for a duplicated audio path (same
// frame written twice).
type writeSample struct {
	at   time.Time
	hash uint32
	peak float32 // max |sample|; 0 when the payload was not provided
}

// silenceFloor is the peak below which two identical writes are treated as
// silence (concealment / DTX keepalive) instead of a duplicated audio path.
const silenceFloor = 0.001 // ≈ -60 dBFS

// hashPCM fingerprints the float32 bit patterns (FNV-1a) and returns the peak
// amplitude in the same pass.
func hashPCM(pcm []float32) (uint32, float32) {
	h := uint32(2166136261)
	var peak float32
	for _, v := range pcm {
		h ^= math.Float32bits(v)
		h *= 16777619
		a := v
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	return h, peak
}

// RecordWritePCM feeds one WritePCM timestamp (and its PCM fingerprint, when
// available) for callID into the process-wide detector. No-op when the debug
// log is disabled.
func RecordWritePCM(callID string, pcm []float32) {
	if !Enabled() || callID == "" {
		return
	}
	defaultDup.Record(callID, pcm)
}

// StartDupDetector runs the process-wide detector until ctx is cancelled.
// Safe to run while logging is disabled (it just idles).
func StartDupDetector(ctx context.Context) { defaultDup.Run(ctx) }

// Record stores a WritePCM timestamp (and optional PCM fingerprint) for callID.
// pcm may be omitted by legacy callers; a zero hash means "unknown content"
// and falls back to the timing-only rule.
func (d *DupDetector) Record(callID string, pcm ...[]float32) {
	if callID == "" {
		return
	}
	sample := writeSample{at: d.now()}
	if len(pcm) > 0 && len(pcm[0]) > 0 {
		sample.hash, sample.peak = hashPCM(pcm[0])
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes[callID] = append(d.writes[callID], sample)
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
		kept := make([]writeSample, 0, len(ts))
		for _, t := range ts {
			if !t.at.Before(cut) {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(d.writes, callID)
			delete(d.lastEmit, callID)
			continue
		}
		d.writes[callID] = kept

		var a, b writeSample
		found := false
		for i := 1; i < len(kept); i++ {
			if kept[i].at.Sub(kept[i-1].at) >= d.closeGap {
				continue
			}
			// Two writes this close are only suspicious when they carry the
			// SAME audible PCM (a duplicated audio path). Different content
			// within the gap is a jitter-buffer catch-up releasing reordered
			// frames; identical near-silence is concealment/DTX, not a
			// duplicated path. Unknown content (hash 0, legacy callers) keeps
			// the timing-only rule.
			prev, cur := kept[i-1], kept[i]
			if prev.hash != 0 && cur.hash != 0 {
				if prev.hash != cur.hash {
					continue
				}
				if prev.peak < silenceFloor && cur.peak < silenceFloor {
					continue
				}
			}
			a, b = prev, cur
			found = true
			break
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
				"ts_first":  a.at.UTC().Format(time.RFC3339Nano),
				"ts_second": b.at.UTC().Format(time.RFC3339Nano),
				"gap_ms":    Round4(float64(b.at.Sub(a.at).Microseconds()) / 1000.0),
			})
	}
}
