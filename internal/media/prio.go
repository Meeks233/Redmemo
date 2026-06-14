package media

import (
	"context"
	"sync"
	"sync/atomic"
)

// Download priority — kind constants. Audio always beats video at the same
// generation so a viewer hears sound the moment the audio bytes land while the
// (larger) video stream is still flowing.
const (
	KindVideo = 1
	KindAudio = 0
)

// Priority orders concurrent CDN downloads at the bandwidth chokepoint.
// Generation is a process-monotonic counter incremented on every new media
// request, so a freshly-issued request has the highest generation and
// preempts older in-flight downloads. Within one generation, audio wins.
//
// Long marks a "large video" download — a video exceeding the user-configured
// explicitly opted into. Any non-long writer (image, audio, short video)
// strictly beats any long writer regardless of generation, so an in-flight
// 30-minute clip never holds up an image thumbnail or a fresh short video
// the user just scrolled to. Among long writers, the standard Gen/Kind rule
// applies — newer long requests preempt older long requests.
type Priority struct {
	Gen  int64
	Kind int
	Long bool
}

// better reports whether a is strictly higher priority than b. Non-long
// always beats long. Among same-Long: higher Gen wins; on tie, lower Kind
// (audio = 0) wins.
func (a Priority) better(b Priority) bool {
	if a.Long != b.Long {
		return !a.Long
	}
	if a.Gen != b.Gen {
		return a.Gen > b.Gen
	}
	return a.Kind < b.Kind
}

var genCounter atomic.Int64

// NextGen returns a new monotonic generation. Call once per incoming media
// request so all bytes the request fetches inherit the same arrival rank.
func NextGen() int64 {
	return genCounter.Add(1)
}

type priorityCtxKey struct{}

// WithPriority returns a child context that carries p. Media writers downstream
// of this context register at p, and any newer/higher-priority writer preempts
// them at the bandwidth gate.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityCtxKey{}, p)
}

// PriorityFromContext returns the priority embedded in ctx, or a default
// (gen 0, video) that loses to every explicitly-tagged request.
func PriorityFromContext(ctx context.Context) Priority {
	if v, ok := ctx.Value(priorityCtxKey{}).(Priority); ok {
		return v
	}
	return Priority{Gen: 0, Kind: KindVideo}
}

// prioGate tracks the set of in-flight CDN writers and lets each Write proceed
// only when no strictly-higher-priority writer is registered. A new high-prio
// arrival closes the wakeup channel, kicking every waiter to re-check.
type prioGate struct {
	mu      sync.Mutex
	ch      chan struct{}
	nextID  int64
	entries map[int64]Priority
}

func newPrioGate() *prioGate {
	return &prioGate{
		ch:      make(chan struct{}),
		entries: map[int64]Priority{},
	}
}

func (g *prioGate) register(p Priority) int64 {
	g.mu.Lock()
	g.nextID++
	id := g.nextID
	g.entries[id] = p
	prev := g.ch
	g.ch = make(chan struct{})
	g.mu.Unlock()
	close(prev)
	return id
}

func (g *prioGate) unregister(id int64) {
	g.mu.Lock()
	if _, ok := g.entries[id]; !ok {
		g.mu.Unlock()
		return
	}
	delete(g.entries, id)
	prev := g.ch
	g.ch = make(chan struct{})
	g.mu.Unlock()
	close(prev)
}

// snapshot returns the highest-priority registered entry and the wakeup
// channel to park on if the caller needs to wait.
func (g *prioGate) snapshot() (best Priority, hasBest bool, ch chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.entries {
		if !hasBest || p.better(best) {
			best = p
			hasBest = true
		}
	}
	return best, hasBest, g.ch
}

// wait blocks until no registered entry is strictly higher priority than mine,
// or ctx is canceled.
func (g *prioGate) wait(ctx context.Context, mine Priority) error {
	for {
		best, has, ch := g.snapshot()
		if !has || !best.better(mine) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// globalGate is the process-wide CDN scheduler shared by every limitedWriter.
var globalGate = newPrioGate()
