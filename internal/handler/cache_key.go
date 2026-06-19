package handler

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
)

// htmlCacheTTL bounds how long a rendered HTML blob may serve. A short TTL is
// the primary correctness lever: archive updates, L4 icon refreshes, and any
// other render-affecting state become visible within this window without
// explicit invalidation. User-driven mutations (settings save, manual refresh)
// still call InvalidateAllHTML / InvalidateHTMLPrefix for immediate effect.
const htmlCacheTTL = 60 * time.Second

// prefsCacheTag fingerprints every Preferences field that can change the
// rendered HTML output. Hashing %+v stays correct when new fields are added —
// the cost is that any change to the struct partitions the cache. This is a
// fingerprint, not a security token: an FNV collision merely lets two prefs
// variants share an entry, which short-TTL natural expiry recovers from.
func prefsCacheTag(prefs reddit.Preferences) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%+v", prefs)
	const hexdigits = "0123456789abcdef"
	sum := h.Sum64()
	var out [16]byte
	for i := 15; i >= 0; i-- {
		out[i] = hexdigits[sum&0xf]
		sum >>= 4
	}
	return string(out[:])
}

// htmlCacheKey composes the Redis key. urlPath + rawQuery stays as a literal
// prefix so InvalidateHTMLPrefix can target one page across all prefs variants;
// the prefs tag lives at the tail.
func htmlCacheKey(urlPath, rawQuery string, prefs reddit.Preferences) string {
	if rawQuery == "" {
		return urlPath + "#" + prefsCacheTag(prefs)
	}
	return urlPath + "?" + rawQuery + "#" + prefsCacheTag(prefs)
}

// cacheHTMLAsync persists a rendered HTML blob off the request goroutine so a
// slow/failing Redis round-trip never delays the response. A nil cache (test
// or misconfig) is a no-op. The buffer is copied because the caller may reuse
// the underlying array after returning.
func (h *Handler) cacheHTMLAsync(key string, body []byte) {
	if h.cache == nil || len(body) == 0 {
		return
	}
	dup := make([]byte, len(body))
	copy(dup, body)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := h.cache.PutHTML(ctx, key, dup, htmlCacheTTL); err != nil {
			log.Printf("cacheHTMLAsync %s: %v", key, err)
		}
	}()
}
