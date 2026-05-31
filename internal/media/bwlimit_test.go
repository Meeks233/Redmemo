package media

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestTokenBucketSingleStream pushes one writer through a small bucket and
// checks that the sustained throughput is bounded by the configured rate.
func TestTokenBucketSingleStream(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s
	bucket := newTokenBucket(rate, rate)
	w := &limitedWriter{ctx: context.Background(), w: io.Discard, b: bucket}

	const dur = 3 * time.Second
	deadline := time.Now().Add(dur)
	buf := make([]byte, 64<<10) // 64 KiB writes
	var total int64
	start := time.Now()
	for time.Now().Before(deadline) {
		n, err := w.Write(buf)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		total += int64(n)
	}
	elapsed := time.Since(start)

	// Expected = full burst granted upfront + steady rate × elapsed.
	expected := float64(rate) + float64(rate)*elapsed.Seconds()
	low, high := expected*0.85, expected*1.15
	if float64(total) < low || float64(total) > high {
		t.Fatalf("wrote %d B in %v, expected ~%.0f B (range [%.0f, %.0f])",
			total, elapsed, expected, low, high)
	}
}

// TestTokenBucketConcurrentStreams is the real stress test: many goroutines
// write through the SAME bucket in parallel, simulating a page full of media
// fetches all racing for the global 5 MB/s allowance. The combined throughput
// must still converge on the bucket's rate — not N × rate.
func TestTokenBucketConcurrentStreams(t *testing.T) {
	const (
		rate    = 2 << 20 // 2 MiB/s
		writers = 32
		dur     = 2 * time.Second
	)
	bucket := newTokenBucket(rate, rate)

	var total atomic.Int64
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	wg.Add(writers)
	start := time.Now()
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			w := &limitedWriter{ctx: ctx, w: io.Discard, b: bucket}
			buf := make([]byte, 32<<10)
			for {
				n, err := w.Write(buf)
				total.Add(int64(n))
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	bytes := total.Load()

	expected := float64(rate) + float64(rate)*elapsed.Seconds()
	low, high := expected*0.85, expected*1.15
	if float64(bytes) < low || float64(bytes) > high {
		t.Fatalf("%d writers wrote %d B in %v, expected ~%.0f B (range [%.0f, %.0f])",
			writers, bytes, elapsed, expected, low, high)
	}
	t.Logf("%d writers wrote %.2f MiB in %v (target rate %.2f MiB/s)",
		writers, float64(bytes)/(1<<20), elapsed, float64(rate)/(1<<20))
}

// TestTokenBucketContextCancel proves a blocked writer unblocks promptly when
// its context is canceled, so a client disconnect can't pin a goroutine forever
// waiting on tokens.
func TestTokenBucketContextCancel(t *testing.T) {
	// Tiny rate so a single ~1 MB write would otherwise sleep ~10 seconds.
	bucket := newTokenBucket(100<<10, 100<<10) // 100 KiB/s
	// Drain the burst.
	if err := bucket.waitN(context.Background(), 100<<10); err != nil {
		t.Fatalf("drain: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &limitedWriter{ctx: ctx, w: io.Discard, b: bucket}

	done := make(chan error, 1)
	go func() {
		_, err := w.Write(make([]byte, 1<<20))
		done <- err
	}()

	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not unblock on cancel")
	}
}

// TestGlobalCDNLimiterObservedPeak drives the SAME process-wide cdnLimiter
// production paths use, with realistic 32 KiB writes from many concurrent
// "video" streams, and verifies the peak throughput observed over a rolling
// 200 ms window never exceeds the configured 5 MB/s cap (plus jitter slack).
// This is the regression guard for the bug where a 5 MiB burst capacity let
// observers see ~7 MB/s peaks on a fresh video download.
func TestGlobalCDNLimiterObservedPeak(t *testing.T) {
	const (
		streams    = 8
		dur        = 3 * time.Second
		windowSize = 200 * time.Millisecond
		bucketDur  = 20 * time.Millisecond
		buckets    = int(dur / bucketDur)
	)
	// Drain whatever burst is in the global limiter so we measure from a
	// fresh, full bucket — i.e. the worst case the user actually hits when
	// a new video starts.
	_ = cdnLimiter.waitN(context.Background(), int(cdnLimiter.capacity))
	// Let it refill so the test starts with a representative full bucket.
	time.Sleep(time.Duration(float64(cdnLimiter.capacity)/cdnLimiter.rate*float64(time.Second)) + 50*time.Millisecond)

	var counts [buckets]atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	var wg sync.WaitGroup
	start := time.Now()
	wg.Add(streams)
	for i := 0; i < streams; i++ {
		go func() {
			defer wg.Done()
			w := &limitedWriter{ctx: ctx, w: io.Discard, b: cdnLimiter}
			buf := make([]byte, 32<<10)
			for {
				n, err := w.Write(buf)
				if n > 0 {
					idx := int(time.Since(start) / bucketDur)
					if idx >= 0 && idx < buckets {
						counts[idx].Add(int64(n))
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	// Slide a windowSize window over the 20 ms buckets, compute peak Bps.
	bucketsPerWindow := int(windowSize / bucketDur)
	var peakBytes int64
	for i := 0; i+bucketsPerWindow <= buckets; i++ {
		var sum int64
		for j := 0; j < bucketsPerWindow; j++ {
			sum += counts[i+j].Load()
		}
		if sum > peakBytes {
			peakBytes = sum
		}
	}
	peakBps := float64(peakBytes) / windowSize.Seconds()

	// Allow 25% jitter slack for scheduler hiccups; without the fix the
	// peak would be ~7 MB/s (≈40% over the cap), which this still catches.
	limit := float64(cdnBandwidthLimit) * 1.25
	if peakBps > limit {
		t.Fatalf("peak %.2f MB/s over %v window exceeds cap %.2f MB/s (limit with slack %.2f MB/s)",
			peakBps/(1<<20), windowSize, float64(cdnBandwidthLimit)/(1<<20), limit/(1<<20))
	}
	t.Logf("peak %.2f MB/s over %v window (cap %.2f MB/s)",
		peakBps/(1<<20), windowSize, float64(cdnBandwidthLimit)/(1<<20))
}

// TestTokenBucketBurstThenSteady checks that the first ~1 second of burst
// drains quickly, then subsequent throughput is the steady rate. This guards
// against a regression where the bucket refills past capacity or undercounts
// elapsed time.
func TestTokenBucketBurstThenSteady(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s, 1 MiB burst
	bucket := newTokenBucket(rate, rate)
	w := &limitedWriter{ctx: context.Background(), w: io.Discard, b: bucket}

	// Drain the full burst quickly — should take well under the steady-state
	// floor of 1 second.
	burstStart := time.Now()
	if _, err := w.Write(make([]byte, rate)); err != nil {
		t.Fatalf("burst write: %v", err)
	}
	if d := time.Since(burstStart); d > 200*time.Millisecond {
		t.Fatalf("burst took %v, expected <200ms", d)
	}

	// Now write another 1 MiB — must take ~1 s at the steady rate.
	steadyStart := time.Now()
	written := 0
	buf := make([]byte, 64<<10)
	for written < rate {
		n, err := w.Write(buf)
		if err != nil {
			t.Fatalf("steady write: %v", err)
		}
		written += n
	}
	d := time.Since(steadyStart)
	if d < 700*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("steady 1 MiB took %v, expected ~1s", d)
	}
}
