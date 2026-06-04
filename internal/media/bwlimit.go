package media

import (
	"context"
	"io"
	"sync"
	"time"
)

// cdnBandwidthLimit caps the combined download throughput of every media fetch
// the proxy makes from Reddit's CDN — images, GIFs, video/audio segments, mux
// probes. A single global token bucket means a feed full of fresh videos can't
// saturate the host's uplink (or Reddit's per-IP shaping) regardless of how
// many ServeMedia / ServeMuxed / prefetch jobs are in flight.
const cdnBandwidthLimit = 5 * 1024 * 1024 // bytes per second

// cdnBurstCapacity caps the instantaneous burst the bucket will grant. A full
// second of budget (5 MiB) lets a fresh stream blast its first ~5 MiB
// uncapped, so external observers see ~7 MB/s peaks even though the steady
// rate is 5 MB/s — visually breaking the "max 5 MB/s" promise. ~50 ms of
// budget is enough to absorb scheduler jitter and a typical 32-64 KiB write
// without an extra wait, while keeping the observable peak pinned at the
// configured rate.
const cdnBurstCapacity = cdnBandwidthLimit / 20

// cdnLimiter is the process-wide token bucket. Sustained throughput converges
// on cdnBandwidthLimit; burst is intentionally small so the peak observed
// download speed never exceeds the configured cap.
var cdnLimiter = newTokenBucket(cdnBandwidthLimit, cdnBurstCapacity)

type tokenBucket struct {
	mu       sync.Mutex
	rate     float64 // tokens added per second
	capacity float64
	tokens   float64
	last     time.Time
}

func newTokenBucket(rate, capacity int) *tokenBucket {
	return &tokenBucket{
		rate:     float64(rate),
		capacity: float64(capacity),
		tokens:   float64(capacity),
		last:     time.Now(),
	}
}

// reserve removes up to want tokens from the bucket, refilling first based on
// elapsed time. It returns how many tokens were granted (>=1) and, when fewer
// than want were available, the duration the caller must wait before more
// tokens will accrue. A grant of 0 means "wait first, then retry."
func (b *tokenBucket) reserve(want int) (granted int, wait time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.rate
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens >= 1 {
		take := float64(want)
		if take > b.tokens {
			take = b.tokens
		}
		b.tokens -= take
		return int(take), 0
	}
	need := 1 - b.tokens
	wait = time.Duration(need / b.rate * float64(time.Second))
	return 0, wait
}

// waitN blocks until n tokens have been consumed or ctx is canceled.
func (b *tokenBucket) waitN(ctx context.Context, n int) error {
	remaining := n
	for remaining > 0 {
		got, wait := b.reserve(remaining)
		if got > 0 {
			remaining -= got
			continue
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// prioChunkSize caps the bytes a single Write services before re-checking the
// priority gate. Keeping this small (32 KiB) means a freshly-arrived high-prio
// download preempts in-flight low-prio writers within one chunk's worth of
// shaping latency, instead of waiting out a multi-MiB io.Copy slice.
const prioChunkSize = 32 << 10

// limitedWriter throttles writes through the global CDN bucket AND the
// priority gate. Each Write blocks until it owns the current top priority and
// then reserves token-bucket bandwidth; combined throughput across every
// concurrent media fetch converges on cdnBandwidthLimit, but newer / audio
// downloads preempt older / video ones byte-for-byte.
type limitedWriter struct {
	ctx   context.Context
	w     io.Writer
	b     *tokenBucket
	gate  *prioGate
	p     Priority
	regID int64
}

// newLimitedWriter wraps w so every byte is paced through the global CDN
// limiter and ordered through the global priority gate. The returned writer
// must be released via release() once the download finishes so its priority
// slot doesn't pin the gate open.
func newLimitedWriter(ctx context.Context, w io.Writer) *limitedWriter {
	p := PriorityFromContext(ctx)
	lw := &limitedWriter{ctx: ctx, w: w, b: cdnLimiter, gate: globalGate, p: p}
	lw.regID = globalGate.register(p)
	return lw
}

// release drops this writer's slot from the priority gate. Safe to call
// multiple times; further Writes still work but always lose ties.
func (l *limitedWriter) release() {
	if l.gate != nil && l.regID != 0 {
		l.gate.unregister(l.regID)
		l.regID = 0
	}
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return l.w.Write(p)
	}
	// Consume the entire buffer, but in small slices so a newer /
	// higher-priority writer arriving mid-Write preempts us at the next
	// chunk boundary instead of after the whole buffer drains.
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > prioChunkSize {
			chunk = prioChunkSize
		}
		if l.gate != nil {
			if err := l.gate.wait(l.ctx, l.p); err != nil {
				return total, err
			}
		}
		if err := l.b.waitN(l.ctx, chunk); err != nil {
			return total, err
		}
		n, err := l.w.Write(p[total : total+chunk])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
