package unfurl

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
)

// Service is the cached, deduplicated unfurl layer behind the lazy /api/unfurl
// endpoint. Links are no longer unfurled in a burst at page-render time (which
// hammered hosts like GitHub from the server IP and got later links rate-limited
// down to plain links). Instead the browser requests one preview at a time as a
// card scrolls into view, and this Service:
//
//   - serves a persistent DB cache (link_preview) so a link is fetched at most
//     once per freshness window, across restarts and across every viewer;
//   - single-flights concurrent requests for the same link onto one fetch;
//   - caps server-side outbound fetches with a small semaphore, so even a burst
//     of viewport hits can never fan out an unbounded number of cross-site
//     requests (the second guard against host rate-limiting, on top of the
//     client's own lazy + concurrency-limited loader).
type Service struct {
	store   *store.LinkPreviewStore
	fetcher *fetcher

	cacheTTL time.Duration // how long an "ok" row stays fresh
	failTTL  time.Duration // how long a "failed" row suppresses retries
	perFetch time.Duration // per-link fetch ceiling

	sem chan struct{} // outbound-fetch concurrency cap

	mu       sync.Mutex
	inflight map[string]*flightCall
}

type flightCall struct {
	done chan struct{}
	row  *store.LinkPreview
}

// Config tunes the Service. Zero values fall back to sensible defaults in New.
type Config struct {
	Enabled      bool
	JinaFallback bool
	Timeout      time.Duration
}

// New builds the Service. The fetcher uses the same uTLS-spoofed transport every
// other outbound client uses, so unfurl fetches share the project's TLS
// fingerprint posture rather than presenting a naked Go stack.
func New(st *store.LinkPreviewStore, cfg Config) *Service {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &Service{
		store: st,
		fetcher: &fetcher{
			client:       transport.NewSpoofedClient(timeout),
			jinaFallback: cfg.JinaFallback,
		},
		cacheTTL: 14 * 24 * time.Hour,
		failTTL:  1 * time.Hour,
		perFetch: timeout,
		sem:      make(chan struct{}, 3),
		inflight: make(map[string]*flightCall),
	}
}

// ResolveOne returns the cached-or-freshly-fetched preview row for a single
// external link. A fresh "ok" row is returned from cache immediately; a fresh
// "failed" row is returned as-is (so the caller renders nothing and the client
// stops retrying); a miss triggers one single-flighted, concurrency-capped
// fetch whose result (ok or a negative "failed" row) is persisted and returned.
// rawURL that is not a public http(s) link yields a "failed" row without any
// network access (the SSRF boundary lives in the fetcher, but we short-circuit
// here too so a blocked URL is cached as failed rather than re-vetted each call).
func (s *Service) ResolveOne(ctx context.Context, rawURL string) (*store.LinkPreview, error) {
	key := reddit.CanonicalKey(rawURL)

	row, err := s.store.Get(key, s.cacheTTL)
	if err != nil {
		log.Printf("unfurl: cache read %s: %v", key, err)
	} else if row != nil {
		// An "ok" row serves for the full cacheTTL. A "failed" row is only a
		// SHORT negative cache (failTTL): it stops a hammering retry on every
		// viewport hit, but expires soon so a TRANSIENT failure (a host 429 or
		// timeout during a busy fetch burst) self-heals on the next view rather
		// than entombing the link as plain text for the whole 14-day window.
		if row.Status == store.LinkPreviewOK || time.Since(row.FetchedAt) < s.failTTL {
			return row, nil
		}
	}
	return s.fetchOne(ctx, key, rawURL), nil
}

// fetchOne resolves a single key through the single-flight gate and the outbound
// concurrency cap, persisting the outcome. A nil/unusable result is stored as a
// "failed" negative-cache row so the link is not re-fetched on every viewport
// hit. Always returns a non-nil row (ok or failed) unless the context dies.
func (s *Service) fetchOne(ctx context.Context, key, rawURL string) *store.LinkPreview {
	s.mu.Lock()
	if call, ok := s.inflight[key]; ok {
		s.mu.Unlock()
		select {
		case <-call.done:
			return call.row
		case <-ctx.Done():
			return nil
		}
	}
	call := &flightCall{done: make(chan struct{})}
	s.inflight[key] = call
	s.mu.Unlock()

	// Re-check the cache: a sibling may have SUCCEEDED between our miss and taking
	// the lead. Only an "ok" row short-circuits — a stale "failed" row is exactly
	// what we're here to retry, so don't let it block the re-fetch.
	if row, _ := s.store.Get(key, s.cacheTTL); row != nil && row.Status == store.LinkPreviewOK {
		call.row = row
	} else {
		call.row = s.doFetch(ctx, key, rawURL)
	}

	s.mu.Lock()
	delete(s.inflight, key)
	s.mu.Unlock()
	close(call.done)
	return call.row
}

func (s *Service) doFetch(ctx context.Context, key, rawURL string) *store.LinkPreview {
	// Outbound concurrency cap: at most len(s.sem) cross-site fetches in flight
	// across the whole instance.
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil
	}

	fctx, cancel := context.WithTimeout(ctx, s.perFetch)
	p, err := s.fetcher.Fetch(fctx, rawURL)
	cancel()
	if err != nil {
		log.Printf("unfurl: fetch %s: %v", rawURL, err)
	}

	row := &store.LinkPreview{URLKey: key, URL: rawURL, Status: store.LinkPreviewFailed}
	if p.Usable() {
		row = &store.LinkPreview{
			URLKey:      key,
			URL:         p.URL,
			Title:       p.Title,
			Description: p.Description,
			ImageURL:    p.ImageURL,
			SiteName:    p.SiteName,
			ImageWide:   p.ImageWide,
			VideoURL:    p.VideoURL,
			Status:      store.LinkPreviewOK,
		}
	}
	if err := s.store.Upsert(row); err != nil {
		log.Printf("unfurl: persist %s: %v", key, err)
	}
	return row
}
