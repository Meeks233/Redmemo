package store

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestKeyedMutex_MutualExclusionSameKey proves two goroutines contending on the
// same key never run their critical sections concurrently.
func TestKeyedMutex_MutualExclusionSameKey(t *testing.T) {
	k := newKeyedMutex()
	const goroutines, iters = 16, 500
	var inside int32
	var maxSeen int32
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				unlock := k.lock("same-hash")
				n := atomic.AddInt32(&inside, 1)
				for {
					old := atomic.LoadInt32(&maxSeen)
					if n <= old || atomic.CompareAndSwapInt32(&maxSeen, old, n) {
						break
					}
				}
				atomic.AddInt32(&inside, -1)
				unlock()
			}
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Fatalf("observed %d goroutines inside the same-key critical section; want 1", maxSeen)
	}
}

// TestKeyedMutex_DistinctKeysDontBlock confirms distinct keys are independent:
// a goroutine holding key A must not stop another from acquiring key B.
func TestKeyedMutex_DistinctKeysDontBlock(t *testing.T) {
	k := newKeyedMutex()
	relA := k.lock("A")
	defer relA()

	done := make(chan struct{})
	go func() {
		relB := k.lock("B")
		relB()
		close(done)
	}()
	// If "B" had blocked on "A", done would never close and the test would hang
	// (caught by the package test timeout). Reaching here means they were
	// independent.
	<-done
}

// TestKeyedMutex_NoEntryLeak verifies the reference-counted map releases every
// entry once no goroutine holds or waits on its key.
func TestKeyedMutex_NoEntryLeak(t *testing.T) {
	k := newKeyedMutex()
	const goroutines, iters = 32, 1000
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			keys := []string{"a", "b", "c", "d"}
			for i := 0; i < iters; i++ {
				key := keys[(g+i)%len(keys)]
				unlock := k.lock(key)
				unlock()
			}
		}(g)
	}
	wg.Wait()

	k.mu.Lock()
	n := len(k.locks)
	k.mu.Unlock()
	if n != 0 {
		t.Fatalf("keyedMutex leaked %d entries; want 0 after all releases", n)
	}
}
