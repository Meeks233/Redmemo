package handler

import (
	"sync"
	"time"
)

// reshuffleMinGap is the minimum wall-clock spacing between two full-table
// Reshuffle() writes triggered by /random round completions. The reshuffle is
// the design's only O(N) write; on a large archive it is a multi-second
// blocking UPDATE. A narrow filter (small q= subset) can complete a
// no-replacement sweep in a handful of requests, so without a throttle a steady
// trickle of such requests would fire back-to-back full-table writes. The gate
// coalesces all round-completions inside one window into a single reshuffle.
//
// Landing a reshuffle slightly late is harmless: the golden-ratio origin
// rotation still happens per round (in randomWalk), and an un-reshuffled round
// merely replays the previous permutation from a rotated origin — still a valid,
// well-spread sweep — until the background reshuffle redraws shuffle_key.
const reshuffleMinGap = 12 * time.Minute

// reshuffleSignal is a buffered (size 1) channel used as a dirty flag: a round
// completion does a non-blocking send, so concurrent / rapid completions
// coalesce into at most one pending request (a full buffer drops the extra
// send). The single long-lived worker goroutine drains it and applies the
// throttle, so there is never a goroutine spawned per request.
var (
	reshuffleSignal     = make(chan struct{}, 1)
	reshuffleWorkerOnce sync.Once
)

// signalReshuffle marks the posts table as needing a background reshuffle. It is
// called from the /random walk on round completion WITHOUT holding the shard
// mutex and never blocks: if a reshuffle is already pending the extra signal is
// simply dropped (the pending one will cover this round too).
func (h *Handler) signalReshuffle() {
	h.startReshuffleWorker()
	select {
	case reshuffleSignal <- struct{}{}:
	default:
	}
}

// startReshuffleWorker lazily launches the single background reshuffle goroutine
// on first use. Doing it lazily (sync.Once) keeps reshuffle wiring entirely
// inside the random handler so handler construction (router.go/deps.go/main.go)
// stays untouched; the goroutine is started at most once per process and runs
// for the process lifetime, so it cannot leak per request.
func (h *Handler) startReshuffleWorker() {
	reshuffleWorkerOnce.Do(func() {
		go h.runReshuffleWorker()
	})
}

// runReshuffleWorker is the long-lived worker. It waits for a dirty signal, then
// performs the full-table Reshuffle off the request path, enforcing
// reshuffleMinGap between successive writes so a burst of round-completions
// collapses into a single reshuffle per window.
func (h *Handler) runReshuffleWorker() {
	var last time.Time
	for range reshuffleSignal {
		if h.postStore == nil {
			continue
		}
		// Throttle: if the previous reshuffle was within the min-gap, sleep out
		// the remainder so rapid completions coalesce. Any signals that arrive
		// during the wait fill the size-1 buffer at most once, so we wake to do
		// exactly one reshuffle covering them all.
		if wait := reshuffleMinGap - time.Since(last); !last.IsZero() && wait > 0 {
			time.Sleep(wait)
			// Drain a signal that may have been queued during the wait so it does
			// not immediately re-trigger another (already-covered) reshuffle.
			select {
			case <-reshuffleSignal:
			default:
			}
		}
		// A failed reshuffle is non-fatal: the next round simply replays the
		// existing permutation from a rotated origin until a later attempt
		// succeeds, so we just drop the error rather than block the walk.
		_ = h.postStore.Reshuffle()
		last = time.Now()
	}
}
