package handler

import (
	"fmt"
	"sync"
)

// singleFlight coalesces concurrent calls keyed by a string: when N goroutines
// ask for the same key while a fetch is in flight, only the first actually
// runs the work; the rest wait on the in-flight call and receive its result.
// Used to keep N parallel identical Reddit fetches from burning N OAuth quota
// units on the same upstream listing/post.
//
// Kept in-tree (instead of pulling golang.org/x/sync/singleflight) because the
// implementation is small and the project leans on minimal external deps.
type singleFlight struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	val  any
	err  error
}

func newSingleFlight() *singleFlight {
	return &singleFlight{m: make(map[string]*flightCall)}
}

// Do runs fn for key, deduplicating concurrent calls. Returns fn's result
// (or the leader's result, if a leader was already in flight). shared is true
// when this caller piggy-backed on an existing leader.
func (sf *singleFlight) Do(key string, fn func() (any, error)) (val any, err error, shared bool) {
	sf.mu.Lock()
	if c, ok := sf.m[key]; ok {
		sf.mu.Unlock()
		<-c.done
		return c.val, c.err, true
	}
	c := &flightCall{done: make(chan struct{})}
	sf.m[key] = c
	sf.mu.Unlock()

	// Run fn with panic safety: the map cleanup and channel close MUST happen
	// even if fn panics, otherwise every follower blocked on <-c.done hangs
	// forever AND the poisoned entry is never removed from sf.m, permanently
	// wedging all future requests for this key. On panic we record it in c.err
	// so followers receive an error instead of deadlocking, then re-panic on the
	// leader so the recovery middleware can turn it into a 500.
	var panicked any
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = r
				c.err = fmt.Errorf("singleflight: panic in flight for %q: %v", key, r)
			}
		}()
		c.val, c.err = fn()
	}()

	sf.mu.Lock()
	delete(sf.m, key)
	sf.mu.Unlock()
	close(c.done)

	if panicked != nil {
		panic(panicked)
	}

	return c.val, c.err, false
}
