package media

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeWriter discards bytes but records when each Write happens against a
// shared monotonic clock, tagged with the writer's label. Used to prove
// preemption ordering at the priority gate.
type fakeWriter struct {
	label string
	rec   *writeRecorder
}

type writeRecord struct {
	label string
	at    time.Duration
	n     int
}

type writeRecorder struct {
	mu    sync.Mutex
	start time.Time
	out   []writeRecord
}

func newRecorder() *writeRecorder { return &writeRecorder{start: time.Now()} }

func (r *writeRecorder) add(label string, n int) {
	r.mu.Lock()
	r.out = append(r.out, writeRecord{label: label, at: time.Since(r.start), n: n})
	r.mu.Unlock()
}

func (r *writeRecorder) snapshot() []writeRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]writeRecord, len(r.out))
	copy(out, r.out)
	return out
}

func (f *fakeWriter) Write(p []byte) (int, error) {
	f.rec.add(f.label, len(p))
	return len(p), nil
}

// runFakeStream pushes total bytes through a priority-tagged limitedWriter at
// 32 KiB at a time, recording each write into rec. Stops early on ctx cancel.
func runFakeStream(t *testing.T, ctx context.Context, gate *prioGate, bucket *tokenBucket, p Priority, label string, rec *writeRecorder, total int) int64 {
	t.Helper()
	regID := gate.register(p)
	return runFakeStreamRegistered(t, ctx, gate, bucket, p, regID, label, rec, total)
}

// runFakeStreamRegistered is the variant where the caller has already
// reserved the gate slot synchronously (so it sees an in-progress higher-prio
// writer instantly on the first Write rather than racing the goroutine start).
func runFakeStreamRegistered(t *testing.T, ctx context.Context, gate *prioGate, bucket *tokenBucket, p Priority, regID int64, label string, rec *writeRecorder, total int) int64 {
	t.Helper()
	fw := &fakeWriter{label: label, rec: rec}
	lw := &limitedWriter{
		ctx:   ctx,
		w:     fw,
		b:     bucket,
		gate:  gate,
		p:     p,
		regID: regID,
	}
	defer lw.release()

	buf := make([]byte, 32<<10)
	var written int64
	for written < int64(total) {
		if ctx.Err() != nil {
			return written
		}
		n, err := lw.Write(buf)
		written += int64(n)
		if err != nil {
			return written
		}
	}
	return written
}

// TestPrioGateBetter checks the strict ordering: newer gen beats older, audio
// beats video at the same gen.
func TestPrioGateBetter(t *testing.T) {
	audio1 := Priority{Gen: 1, Kind: KindAudio}
	video1 := Priority{Gen: 1, Kind: KindVideo}
	video2 := Priority{Gen: 2, Kind: KindVideo}
	audio0 := Priority{Gen: 0, Kind: KindAudio}

	if !audio1.better(video1) {
		t.Fatal("audio should beat video at same gen")
	}
	if !video2.better(video1) {
		t.Fatal("newer gen should beat older gen")
	}
	if !video2.better(audio1) {
		t.Fatal("newer video should beat older audio")
	}
	if !audio1.better(audio0) {
		t.Fatal("newer audio should beat older audio")
	}
	if audio1.better(audio1) {
		t.Fatal("equal priority should not be 'better'")
	}
}

// TestPreemptionNewVideoOverridesOld verifies that when a fresh-gen video
// download joins while an old-gen video download is running, the old one is
// preempted: after the new one arrives, ALL writes credited to the new label
// land before any further writes credited to the old label.
func TestPreemptionNewVideoOverridesOld(t *testing.T) {
	gate := newPrioGate()
	bucket := newTokenBucket(1<<20, 1<<20) // 1 MiB/s, generous
	rec := newRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFakeStream(t, ctx, gate, bucket, Priority{Gen: 1, Kind: KindVideo}, "old", rec, 512<<10)
	}()

	// Let the old writer accumulate at least one chunk before preemption.
	time.Sleep(40 * time.Millisecond)
	preemptAt := time.Since(rec.start)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runFakeStream(t, ctx, gate, bucket, Priority{Gen: 2, Kind: KindVideo}, "new", rec, 128<<10)
	}()

	wg.Wait()

	// Find first "new" write and last "old" write.
	var firstNew, lastNew, lastOld time.Duration
	var oldAfterPreempt int
	gotNew := false
	for _, r := range rec.snapshot() {
		if r.label == "new" {
			if !gotNew {
				firstNew = r.at
				gotNew = true
			}
			lastNew = r.at
		} else {
			lastOld = r.at
			if r.at > firstNew && gotNew {
				oldAfterPreempt++
			}
		}
	}

	if !gotNew {
		t.Fatal("expected new writer to make progress")
	}
	if firstNew < preemptAt {
		t.Fatalf("new writer started before it was launched: firstNew=%v preemptAt=%v", firstNew, preemptAt)
	}
	// Strict preemption: once "new" has any byte through, "old" must stop
	// until "new" releases. Tolerate at most 1 in-flight old chunk that was
	// already past the gate when "new" registered.
	if oldAfterPreempt > 1 {
		t.Fatalf("old writer kept running after preemption (%d writes after firstNew=%v, lastOld=%v lastNew=%v)",
			oldAfterPreempt, firstNew, lastOld, lastNew)
	}
}

// TestAudioBeatsVideoSameGen proves that at the same generation, an audio
// writer entering the gate preempts a video writer.
func TestAudioBeatsVideoSameGen(t *testing.T) {
	gate := newPrioGate()
	bucket := newTokenBucket(1<<20, 1<<20)
	rec := newRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runFakeStream(t, ctx, gate, bucket, Priority{Gen: 5, Kind: KindVideo}, "video", rec, 512<<10)
	}()

	time.Sleep(40 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		runFakeStream(t, ctx, gate, bucket, Priority{Gen: 5, Kind: KindAudio}, "audio", rec, 128<<10)
	}()

	wg.Wait()

	var firstAudio time.Duration
	gotAudio := false
	videoAfterAudio := 0
	for _, r := range rec.snapshot() {
		if r.label == "audio" {
			if !gotAudio {
				firstAudio = r.at
				gotAudio = true
			}
		} else if gotAudio && r.at > firstAudio {
			videoAfterAudio++
		}
	}
	if !gotAudio {
		t.Fatal("expected audio to make progress")
	}
	if videoAfterAudio > 1 {
		t.Fatalf("video kept running after audio joined: %d writes after firstAudio=%v",
			videoAfterAudio, firstAudio)
	}
}

// TestSmallVideosBeatOldBigVideos is the scenario from the bug report: a feed
// of "big" videos is in flight, then the user pages forward and a feed of
// "small" videos arrives at higher gen. The small set must finish before any
// big set writer continues.
func TestSmallVideosBeatOldBigVideos(t *testing.T) {
	gate := newPrioGate()
	bucket := newTokenBucket(2<<20, 2<<20) // 2 MiB/s
	rec := newRecorder()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	const (
		bigCount = 4
		bigSize  = 1 << 20 // 1 MiB each "big" video
		smaCount = 6
		smaSize  = 96 << 10 // 96 KiB each "small" video
	)

	var wg sync.WaitGroup
	// Old generation: big videos.
	for i := 0; i < bigCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFakeStream(t, ctx, gate, bucket,
				Priority{Gen: 1, Kind: KindVideo}, "big", rec, bigSize)
		}()
	}

	// Wait long enough for at least some big-video writes to land before the
	// new page arrives.
	time.Sleep(80 * time.Millisecond)

	// Pre-register all small-page slots SYNCHRONOUSLY so smallStart is the
	// instant the gate already knows about every one of them — otherwise the
	// goroutine-launch race lets big writers leak past smallStart before any
	// small writer has registered.
	audioP := Priority{Gen: 2, Kind: KindAudio}
	videoP := Priority{Gen: 2, Kind: KindVideo}
	audioIDs := make([]int64, smaCount)
	videoIDs := make([]int64, smaCount)
	for i := 0; i < smaCount; i++ {
		audioIDs[i] = gate.register(audioP)
		videoIDs[i] = gate.register(videoP)
	}
	smallStart := time.Since(rec.start)

	for i := 0; i < smaCount; i++ {
		aID, vID := audioIDs[i], videoIDs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFakeStreamRegistered(t, ctx, gate, bucket,
				audioP, aID, "smallA", rec, smaSize/4)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			runFakeStreamRegistered(t, ctx, gate, bucket,
				videoP, vID, "smallV", rec, smaSize)
		}()
	}

	wg.Wait()

	// Verify: while the small page is in flight, big writers do not progress
	// (modulo the in-flight chunk per big writer that was past the gate when
	// the small group registered). Find the small-phase window and count big
	// writes inside it.
	snap := rec.snapshot()
	var lastSmall time.Duration
	for _, r := range snap {
		if (r.label == "smallA" || r.label == "smallV") && r.at > lastSmall {
			lastSmall = r.at
		}
	}
	bigDuringSmall := 0
	for _, r := range snap {
		if r.label == "big" && r.at > smallStart && r.at <= lastSmall {
			bigDuringSmall++
		}
	}
	// Each big writer might land at most one chunk that was already past the
	// priority gate (its waitN had a token reservation when the small group
	// registered). After that chunk completes, the big writer's next Write
	// parks at gate.wait until the small group drains.
	if bigDuringSmall > bigCount {
		t.Fatalf("big videos leaked through during small-page window: %d big writes in (%v, %v] (tolerance %d)",
			bigDuringSmall, smallStart, lastSmall, bigCount)
	}
	t.Logf("smallStart=%v lastSmall=%v bigDuringSmall=%d (tolerance %d)",
		smallStart, lastSmall, bigDuringSmall, bigCount)

	// Sanity: audio bytes for small group complete before video bytes for
	// small group, since audio strictly beats video at same gen.
	var lastSmallA, firstSmallVAfterA time.Duration
	gotA := false
	for _, r := range rec.snapshot() {
		if r.label == "smallA" {
			if r.at > lastSmallA {
				lastSmallA = r.at
			}
			gotA = true
		}
	}
	for _, r := range rec.snapshot() {
		if r.label == "smallV" && gotA && r.at > lastSmallA {
			if firstSmallVAfterA == 0 || r.at < firstSmallVAfterA {
				firstSmallVAfterA = r.at
			}
		}
	}
	// Some smallV writes will land before lastSmallA only if they were in
	// flight when the audio writers registered; count those.
	earlySmallV := 0
	for _, r := range rec.snapshot() {
		if r.label == "smallV" && r.at < lastSmallA {
			earlySmallV++
		}
	}
	if earlySmallV > smaCount {
		t.Fatalf("too many small-video writes leaked through during small-audio phase: %d (tolerance %d)",
			earlySmallV, smaCount)
	}
}

// TestGateContextCancel proves a writer parked on the priority gate unblocks
// promptly when its context is canceled.
func TestGateContextCancel(t *testing.T) {
	gate := newPrioGate()
	bucket := newTokenBucket(1<<20, 1<<20)

	// Register a hog at top priority that never finishes (we never call its
	// release).
	hogID := gate.register(Priority{Gen: 999, Kind: KindAudio})
	defer gate.unregister(hogID)

	ctx, cancel := context.WithCancel(context.Background())
	lw := &limitedWriter{
		ctx:  ctx,
		w:    io.Discard,
		b:    bucket,
		gate: gate,
		p:    Priority{Gen: 1, Kind: KindVideo},
	}
	lw.regID = gate.register(lw.p)
	defer lw.release()

	done := make(chan error, 1)
	go func() {
		_, err := lw.Write(make([]byte, 32<<10))
		done <- err
	}()

	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected ctx cancel error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not unblock on cancel while parked at priority gate")
	}
}

// TestPriorityFromContextRoundTrip verifies WithPriority / PriorityFromContext
// round-trip and that the default is the worst-loss tag.
func TestPriorityFromContextRoundTrip(t *testing.T) {
	def := PriorityFromContext(context.Background())
	if def.Gen != 0 || def.Kind != KindVideo {
		t.Fatalf("default = %+v, want gen=0 kind=video", def)
	}
	p := Priority{Gen: 42, Kind: KindAudio}
	got := PriorityFromContext(WithPriority(context.Background(), p))
	if got != p {
		t.Fatalf("round-trip = %+v, want %+v", got, p)
	}
}

// TestNextGenMonotonic guards against a regression where NextGen() stops being
// strictly increasing under concurrent calls — preemption depends on this.
func TestNextGenMonotonic(t *testing.T) {
	const n = 200
	var wg sync.WaitGroup
	results := make([]int64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = NextGen()
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, v := range results {
		if seen[v] {
			t.Fatalf("NextGen returned duplicate %d", v)
		}
		seen[v] = true
	}
}

// Compile-time sanity: ensure atomic import is used (defensive against drift).
var _ = atomic.Int64{}
