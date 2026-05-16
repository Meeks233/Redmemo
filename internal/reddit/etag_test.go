package reddit

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
)

func TestETagCache_MissThenHit(t *testing.T) {
	c := newETagCache(10)

	if _, _, ok := c.Get("/r/golang/hot.json"); ok {
		t.Error("expected a miss on an empty cache")
	}

	c.Set("/r/golang/hot.json", `"etag-v1"`, []byte("body-1"))

	etag, body, ok := c.Get("/r/golang/hot.json")
	if !ok {
		t.Fatal("expected a hit after Set")
	}
	if etag != `"etag-v1"` {
		t.Errorf("etag = %q, want \"etag-v1\"", etag)
	}
	if string(body) != "body-1" {
		t.Errorf("body = %q, want body-1", body)
	}
}

func TestETagCache_Overwrite(t *testing.T) {
	c := newETagCache(10)
	c.Set("/path", `"v1"`, []byte("first"))
	c.Set("/path", `"v2"`, []byte("second"))

	etag, body, ok := c.Get("/path")
	if !ok {
		t.Fatal("expected a hit")
	}
	if etag != `"v2"` || string(body) != "second" {
		t.Errorf("after overwrite: etag=%q body=%q, want \"v2\"/second", etag, body)
	}
}

func TestETagCache_DistinctKeys(t *testing.T) {
	c := newETagCache(10)
	c.Set("/a", `"ea"`, []byte("A"))
	c.Set("/b", `"eb"`, []byte("B"))

	if etag, _, _ := c.Get("/a"); etag != `"ea"` {
		t.Errorf("/a etag = %q", etag)
	}
	if etag, _, _ := c.Get("/b"); etag != `"eb"` {
		t.Errorf("/b etag = %q", etag)
	}
}

func TestETagCache_EvictsWhenFull(t *testing.T) {
	c := newETagCache(2)
	c.Set("/a", `"ea"`, []byte("A"))
	c.Set("/b", `"eb"`, []byte("B"))
	c.Set("/c", `"ec"`, []byte("C")) // exceeds cap → "/a" (LRU) evicted

	if _, _, ok := c.Get("/a"); ok {
		t.Error("/a should have been evicted as the least-recently-used entry")
	}
	if _, _, ok := c.Get("/b"); !ok {
		t.Error("/b should still be cached")
	}
	if _, _, ok := c.Get("/c"); !ok {
		t.Error("/c should still be cached")
	}
}

func TestETagCache_GetRefreshesRecency(t *testing.T) {
	c := newETagCache(2)
	c.Set("/a", `"ea"`, []byte("A"))
	c.Set("/b", `"eb"`, []byte("B"))
	// Touch /a so /b becomes the least-recently-used.
	if _, _, ok := c.Get("/a"); !ok {
		t.Fatal("/a should be cached")
	}
	c.Set("/c", `"ec"`, []byte("C")) // evicts the LRU, which is now /b

	if _, _, ok := c.Get("/b"); ok {
		t.Error("/b should have been evicted after /a was touched")
	}
	if _, _, ok := c.Get("/a"); !ok {
		t.Error("/a should survive — it was used more recently than /b")
	}
}

func TestETagCache_BoundedUnderFlood(t *testing.T) {
	const cap = 50
	c := newETagCache(cap)
	for i := 0; i < 5000; i++ {
		c.Set("/path/"+strconv.Itoa(i), `"e"`, []byte("body"))
	}
	if got := c.ll.Len(); got != cap {
		t.Errorf("list length = %d, want %d (cache is unbounded)", got, cap)
	}
	if got := len(c.items); got != cap {
		t.Errorf("item map size = %d, want %d (map leaks entries)", got, cap)
	}
}

// TestETagCache_Concurrent exercises the LRU cache from many goroutines.
// Run with -race to confirm Get/Set are race-free.
func TestETagCache_Concurrent(t *testing.T) {
	c := newETagCache(1000)
	const goroutines = 48
	const iters = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				key := fmt.Sprintf("/path/%d", (g+i)%64)
				c.Set(key, fmt.Sprintf(`"e%d"`, i), []byte(key))
				if _, body, ok := c.Get(key); ok && len(body) == 0 {
					t.Errorf("hit for %s returned an empty body", key)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
