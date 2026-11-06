package call

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wacalls/internal/voip/media"

	"go.mau.fi/whatsmeow/types"
)

// TestEmitPeerAudioOrderedUnderConcurrentCallers exercises the emitMu
// protocol that onRelayData uses (L3) from many goroutines at once: each
// caller takes m.mu exactly like the decode section does, records its turn,
// and emits one tagged frame through emitPeerAudioLocked. Turn order equals
// m.mu acquisition order; emitMu is taken before m.mu is released, so
// emissions must come out in exactly that order — a later turn can never
// overtake an earlier one. The callback yields to widen any interleaving
// window; verified under -race.
func TestEmitPeerAudioOrderedUnderConcurrentCallers(t *testing.T) {
	const callers = 8
	const rounds = 40

	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})

	var (
		emittedMu sync.Mutex
		emitted   []int
	)
	m.OnPeerAudio = func(pcm []float32) {
		emittedMu.Lock()
		emitted = append(emitted, int(pcm[0]))
		emittedMu.Unlock()
		// Yield while holding emitMu (the callback runs under it): other
		// callers pile up on m.mu/emitMu, maximizing the interleaving the
		// ordering guarantee has to survive.
		runtime.Gosched()
	}

	var nextTurn atomic.Int64
	var wg sync.WaitGroup
	for c := 0; c < callers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				m.mu.Lock()
				// Turn is assigned under m.mu; emitPeerAudioLocked takes
				// emitMu before releasing m.mu, so emission order is forced
				// to equal turn order.
				turn := int(nextTurn.Add(1))
				m.emitPeerAudioLocked([][]float32{{float32(turn)}})
			}
		}()
	}
	wg.Wait()

	emittedMu.Lock()
	defer emittedMu.Unlock()
	if len(emitted) != callers*rounds {
		t.Fatalf("emitted %d frames, want %d", len(emitted), callers*rounds)
	}
	for i, tag := range emitted {
		if tag != i+1 {
			t.Fatalf("emission %d carries turn %d, want %d (emission must follow turn order; a later turn overtook an earlier one)",
				i, tag, i+1)
		}
	}
}

// TestEmitPeerAudioNilCallbackSafe: a manager whose OnPeerAudio was never set
// must release both locks and emit nothing (the nil-check stays).
func TestEmitPeerAudioNilCallbackSafe(t *testing.T) {
	m := newTestManager(t, newFakeSocket(), newFakeRelay(), &passthroughCodec{})
	m.OnPeerAudio = nil

	m.mu.Lock()
	m.emitPeerAudioLocked([][]float32{{1}})
	if !m.mu.TryLock() {
		t.Fatal("m.mu still held after emitting with a nil callback")
	}
	m.mu.Unlock()

	// Empty output takes the early-return path (no emitMu taken).
	m.mu.Lock()
	m.emitPeerAudioLocked(nil)
	if !m.mu.TryLock() {
		t.Fatal("m.mu still held after the empty-output path")
	}
	m.mu.Unlock()
	if !m.emitMu.TryLock() {
		t.Fatal("emitMu still held after the empty-output path")
	}
	m.emitMu.Unlock()
}

// TestOnRelayDataConcurrentEmissionInSequenceOrder delivers real SRTP packets
// from two relay goroutines at once (L3 regression test): the jitter buffer
// releases frames in sequence order under m.mu, and emitMu must carry that
// order through to OnPeerAudio. Without emitMu, goroutine A can decode N,
// unlock, and be overtaken by goroutine B emitting N+1 first — one 60ms
// segment out of order at the browser.
//
// Every sequence position produces exactly one emission (a decoded frame, or
// a concealment frame that repeats the previous frame's first sample when
// three newer packets piled up behind a missing one), so the tag sequence
// must be non-decreasing: equality only for concealment, never a decrease.
func TestOnRelayDataConcurrentEmissionInSequenceOrder(t *testing.T) {
	media.SetAuthVerify(true)
	key := media.GenerateCallKey()
	peer := types.NewJID("5511888800003", types.DefaultUserServer)
	j := newJitterPeer(t, "call-emit-seq", key, peer)

	// 32 packets keeps |seq - next| under jitterResetThreshold (50), so the
	// buffer never resets mid-test.
	const packets = 32
	payloads := make([][]byte, packets)
	pkts := make([][]byte, packets)
	for i := range payloads {
		// Distinct little-endian int16 payload per sequence number → one
		// distinguishable decoded sample per frame (firstSample decodes it).
		payloads[i] = []byte{byte(i + 1), byte((i + 1) >> 8)}
		pkts[i] = j.packet(payloads[i])
	}

	var (
		emittedMu sync.Mutex
		emitted   []float32
	)
	j.m.OnPeerAudio = func(pcm []float32) {
		if len(pcm) == 0 {
			return
		}
		emittedMu.Lock()
		emitted = append(emitted, pcm[0])
		emittedMu.Unlock()
	}

	// Seed the buffer synchronously: without it, the first concurrent packet
	// could start the stream at seq 1 and drop seq 0 as "late", weakening the
	// ordering check to a single frame.
	j.m.onRelayData(pkts[0])

	var wg sync.WaitGroup
	deliver := func(from, step int) {
		defer wg.Done()
		for i := from; i < packets; i += step {
			j.m.onRelayData(pkts[i])
		}
	}
	wg.Add(2)
	go deliver(1, 2) // odd sequence numbers
	go deliver(2, 2) // even sequence numbers
	wg.Wait()

	emittedMu.Lock()
	defer emittedMu.Unlock()

	if len(emitted) != packets {
		t.Fatalf("OnPeerAudio=%d frames, want %d (every sequence position emits exactly once)", len(emitted), packets)
	}
	for i := 1; i < len(emitted); i++ {
		if emitted[i] < emitted[i-1] {
			t.Fatalf("frame %d out of sequence: %v after %v (emission order must match sequence order)", i, emitted[i], emitted[i-1])
		}
	}
	if want := firstSample(payloads[0]); emitted[0] != want {
		t.Fatalf("first emission = %v, want seeded packet %v", emitted[0], want)
	}
	if want := firstSample(payloads[packets-1]); emitted[packets-1] < want {
		t.Fatalf("last emission = %v, want at least the final packet %v", emitted[packets-1], want)
	}
}

// TestEmissionRunsLockFree proves the emission loop runs with m.mu released
// (L3 constraint): while OnPeerAudio is blocked — datachannel backpressure —
// call-state methods that take m.mu must still proceed. emitMu is held during
// emission (that is what serializes it), but no callback path takes m.mu, so
// the locks never cycle. The time.After selects are deadlock watchdogs, not
// timing assertions: they fire only if the corresponding step never completes.
func TestEmissionRunsLockFree(t *testing.T) {
	media.SetAuthVerify(true)
	key := media.GenerateCallKey()
	peer := types.NewJID("5511888800004", types.DefaultUserServer)
	j := newJitterPeer(t, "call-emit-lockfree", key, peer)

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFn := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseFn()

	j.m.OnPeerAudio = func(pcm []float32) {
		close(started)
		<-release
	}

	pkt := j.packet([]byte{0x01, 0x00})
	done := make(chan struct{})
	go func() {
		j.m.onRelayData(pkt)
		close(done)
	}()

	select {
	case <-started: // emission is in progress, blocked inside the callback
	case <-time.After(5 * time.Second):
		t.Fatal("OnPeerAudio never invoked for a valid peer packet")
	}

	// m.mu must be free while the callback runs.
	proceed := make(chan struct{})
	go func() {
		_ = j.m.CurrentCall()
		close(proceed)
	}()
	select {
	case <-proceed:
	case <-time.After(2 * time.Second):
		t.Fatal("m.mu held during OnPeerAudio emission (call state would freeze under backpressure)")
	}

	releaseFn()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onRelayData did not finish after emission was released")
	}
}
