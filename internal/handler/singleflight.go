package handler

import "sync"

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

	c.val, c.err = fn()

	sf.mu.Lock()
	delete(sf.m, key)
	sf.mu.Unlock()
	close(c.done)

	return c.val, c.err, false
}
