package reddit

import (
	"container/list"
	"sync"
)

type etagEntry struct {
	path string
	etag string
	body []byte
}

// etagCache is a bounded LRU cache of ETag + response body keyed by request
// path. It caps memory by evicting the least-recently-used entry once the
// cache grows past maxSize — without the bound a long-running instance would
// retain the full response body of every distinct API path it ever fetched.
type etagCache struct {
	mu      sync.Mutex
	maxSize int
	ll      *list.List               // front = most recently used
	items   map[string]*list.Element // path → element holding *etagEntry
}

func newETagCache(maxSize int) *etagCache {
	if maxSize < 1 {
		maxSize = 1
	}
	return &etagCache{
		maxSize: maxSize,
		ll:      list.New(),
		items:   make(map[string]*list.Element),
	}
}

func (c *etagCache) Get(path string) (string, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.items[path]
	if !ok {
		return "", nil, false
	}
	c.ll.MoveToFront(el)
	e := el.Value.(*etagEntry)
	return e.etag, e.body, true
}

func (c *etagCache) Set(path, etag string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[path]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*etagEntry)
		e.etag = etag
		e.body = body
		return
	}

	el := c.ll.PushFront(&etagEntry{path: path, etag: etag, body: body})
	c.items[path] = el

	for c.ll.Len() > c.maxSize {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*etagEntry).path)
	}
}
