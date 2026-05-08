package reddit

import "sync"

type etagEntry struct {
	etag string
	body []byte
}

type etagCache struct {
	entries sync.Map
	maxSize int
}

func newETagCache(maxSize int) *etagCache {
	return &etagCache{maxSize: maxSize}
}

func (c *etagCache) Get(path string) (string, []byte, bool) {
	v, ok := c.entries.Load(path)
	if !ok {
		return "", nil, false
	}
	e := v.(*etagEntry)
	return e.etag, e.body, true
}

func (c *etagCache) Set(path, etag string, body []byte) {
	c.entries.Store(path, &etagEntry{etag: etag, body: body})
}
