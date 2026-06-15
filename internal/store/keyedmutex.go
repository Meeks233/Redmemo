package store

import "sync"

// keyedMutex is a registry of per-key mutexes with reference counting, so the
// backing map never accumulates dead entries: an entry exists only while at
// least one goroutine holds or waits on the key. It backs
// (*MediaIndexStore).LockHash, which serializes the disk-file <-> content-row
// critical sections the media package runs for a single content hash.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedMutexEntry
}

type keyedMutexEntry struct {
	mu      sync.Mutex
	waiters int // holders + waiters; the entry is GC'd when this hits zero
}

func newKeyedMutex() *keyedMutex {
	return &keyedMutex{locks: make(map[string]*keyedMutexEntry)}
}

// lock blocks until the mutex for key is held and returns its release function.
// The release MUST be called exactly once; it drops the reference and removes
// the entry from the map once no goroutine is using the key. Distinct keys
// never block each other.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	e := k.locks[key]
	if e == nil {
		e = &keyedMutexEntry{}
		k.locks[key] = e
	}
	e.waiters++
	k.mu.Unlock()

	e.mu.Lock()

	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.waiters--
		if e.waiters == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// LockHash serializes every disk-file + content-row mutation for a single
// content hash. The media publish path (media/hash.go publishContent and
// proxy.go Download) holds it across "rename the file into its content-
// addressed home" + Save; the evictor (media/eviction.go) holds it across
// os.Remove + MarkEvicted. Because a publish and a reclaim of the same hash can
// no longer interleave, eviction can never null a row whose file a concurrent
// re-download just re-created — the download/evict TOCTOU is closed, not merely
// narrowed. Returns the release func; call it exactly once.
func (s *MediaIndexStore) LockHash(hash string) func() {
	return s.hashLocks.lock(hash)
}
