// Package logbuf implements a bounded, thread-safe in-memory ring buffer of
// debug events used to diagnose audio-path problems (duplicated audio, echo
// loops, mic/relay errors). The HTTP server exposes the buffer over
// GET /api/debug/logs so a front-end page can poll it.
//
// Emission is controlled globally by the -debug-log flag (default ON).
package logbuf

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// defaultCapacity is the number of events kept in the ring; older events are
// discarded when it overflows. ~2000 events ≈ a few seconds of instrumented
// audio traffic (frame logs are rate-limited to ~5/s per call and site).
const defaultCapacity = 2000

// Event levels.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is a single diagnostic record. JSON field names are the exact
// contract consumed by the front-end logs page.
type Event struct {
	Seq    int64          `json:"seq"`
	TS     time.Time      `json:"ts"`
	Source string         `json:"source"`
	Level  string         `json:"level"`
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Buffer is a fixed-size ring of events. The zero value is not usable; call
// NewBuffer. Safe for concurrent use.
type Buffer struct {
	mu      sync.Mutex
	events  []Event
	w       int // next write index
	count   int // number of stored events (≤ len(events))
	nextSeq int64

	enabled atomic.Bool
}

// NewBuffer returns a Buffer holding at most capacity events.
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	b := &Buffer{events: make([]Event, capacity)}
	b.enabled.Store(true)
	return b
}

// Default returns the process-wide buffer used by LogEvent.
func Default() *Buffer { return defaultBuf }

var defaultBuf = NewBuffer(defaultCapacity)

// SetEnabled toggles emission on the process-wide buffer (-debug-log flag).
func SetEnabled(on bool) { defaultBuf.SetEnabled(on) }

// Enabled reports whether the process-wide buffer accepts events.
func Enabled() bool { return defaultBuf.Enabled() }

// LogEvent appends an event to the process-wide buffer. No-op when disabled.
func LogEvent(source, level, msg string, fields map[string]any) {
	defaultBuf.Add(source, level, msg, fields)
}

// SetEnabled toggles emission on this buffer.
func (b *Buffer) SetEnabled(on bool) { b.enabled.Store(on) }

// Enabled reports whether this buffer accepts events.
func (b *Buffer) Enabled() bool { return b.enabled.Load() }

// Add appends one event with a monotonically increasing seq. When the ring is
// full the oldest event is overwritten. No-op when the buffer is disabled.
func (b *Buffer) Add(source, level, msg string, fields map[string]any) {
	if !b.enabled.Load() {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSeq++
	b.events[b.w] = Event{
		Seq:    b.nextSeq,
		TS:     time.Now().UTC(),
		Source: source,
		Level:  level,
		Msg:    msg,
		Fields: fields,
	}
	b.w = (b.w + 1) % len(b.events)
	if b.count < len(b.events) {
		b.count++
	}
}

// EventsSince returns up to limit events with Seq > since, oldest first, plus
// the cursor to send as ?since= on the next poll (seq of the last returned
// event, or the incoming since when nothing is new). The returned slice is
// never nil so it serializes as [] in JSON.
func (b *Buffer) EventsSince(since int64, limit int) ([]Event, int64) {
	if limit <= 0 {
		limit = 100
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, 0, min(limit, b.count))
	next := since
	if b.count == 0 {
		return out, next
	}
	oldest := (b.w - b.count + len(b.events)) % len(b.events)
	for i := 0; i < b.count && len(out) < limit; i++ {
		ev := b.events[(oldest+i)%len(b.events)]
		if ev.Seq <= since {
			continue
		}
		out = append(out, ev)
		next = ev.Seq
	}
	return out, next
}

// Reset clears the buffer and restarts seq at 0 (test helper).
func (b *Buffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = make([]Event, len(b.events))
	b.w = 0
	b.count = 0
	b.nextSeq = 0
}

// Len reports how many events are currently stored.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.count
}

// --- rate limiting -----------------------------------------------------------

// Limiter is a small per-key rate limiter (max perSec grants per key per
// wall-clock second). Used to keep 125 msg/s frame paths from flooding the
// ring buffer.
type Limiter struct {
	mu        sync.Mutex
	entries   map[string]*limitEntry
	lastPrune time.Time
	now       func() time.Time
}

type limitEntry struct {
	windowStart time.Time
	count       int
}

// NewLimiter returns an empty limiter.
func NewLimiter() *Limiter {
	now := time.Now
	return &Limiter{entries: map[string]*limitEntry{}, now: now, lastPrune: now()}
}

var frameLimiter = NewLimiter()

// Allow reports whether an event for key is within perSec grants this second
// on the process-wide frame limiter.
func Allow(key string, perSec int) bool { return frameLimiter.Allow(key, perSec) }

// Allow reports whether this call may emit for key (max perSec per second).
func (l *Limiter) Allow(key string, perSec int) bool {
	if perSec <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastPrune) > 5*time.Second {
		l.prune(now)
		l.lastPrune = now
	}
	e, ok := l.entries[key]
	if !ok {
		l.entries[key] = &limitEntry{windowStart: now, count: 1}
		return true
	}
	if now.Sub(e.windowStart) >= time.Second {
		e.windowStart = now
		e.count = 1
		return true
	}
	if e.count >= perSec {
		return false
	}
	e.count++
	return true
}

// prune drops keys whose window is long over so the map cannot grow forever.
func (l *Limiter) prune(now time.Time) {
	for k, e := range l.entries {
		if now.Sub(e.windowStart) > 10*time.Second {
			delete(l.entries, k)
		}
	}
}

// --- PCM helpers -------------------------------------------------------------

// PCMStats returns the RMS and peak amplitude of a float32 PCM frame.
func PCMStats(pcm []float32) (rms, peak float64) {
	if len(pcm) == 0 {
		return 0, 0
	}
	var sum float64
	for _, s := range pcm {
		v := float64(s)
		if v < 0 {
			v = -v
		}
		sum += v * v
		if v > peak {
			peak = v
		}
	}
	return math.Sqrt(sum / float64(len(pcm))), peak
}

// Round4 rounds to 4 decimal places to keep log JSON small.
func Round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
