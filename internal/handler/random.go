package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
	"github.com/redmemo/redmemo/internal/store"
)

// handleRandom returns one random post from the local archive matching the
// filters in the `q` query parameter. It never contacts Reddit; if nothing
// matches it returns 503.
//
// The filter syntax is the SAME unified search-box grammar used by the navbar
// (see internal/searchquery and docs/reddit-search.md) — there is no separate
// /random grammar. So `?q=sub:golang+linux ups>200 after:2025-01-01` filters by
// subreddit scope, score and date exactly as the search box would.
//
// As a browser-friendly convenience, `&` may be used in place of a space to
// separate clauses, so `?q=sub:golang&ups>1000&type:image` is equivalent to
// `?q=sub:golang ups>1000 type:image` (see randomQueryExpr).
//
// One difference: if the query pins a media type (type:image / type:video /
// type:gif), /random redirects straight to the concrete cached media resource
// instead of returning the post as JSON.
func (h *Handler) handleRandom(w http.ResponseWriter, r *http.Request) {
	parsed := searchquery.Parse(randomQueryExpr(r))
	opts := parsedToArchiveOpts(parsed)

	// A pinned media type means "give me the bytes": redirect to the cached
	// media resource. The redirect must only ever land on a post whose media is
	// genuinely on local disk — otherwise the proxy live-fetches it from Reddit,
	// violating /random's "never contacts Reddit" contract and blocking for
	// seconds (a stale media_done flag does not guarantee the bytes survived;
	// see serveRandomMedia).
	if parsed.Instant {
		// Instant mode walks the three media kinds in a fixed video → image →
		// gif preference, restricted to the user's `t:` allow-set if one was
		// given (e.g. `type:vid-gif mode:ins` only walks video). The first kind with a
		// resident match wins. If every allowed kind misses, drop the media
		// filter entirely and fall through to the text path so a matching
		// non-media post can be returned as plain text.
		instantPriority := []string{"video", "image", "gif"}
		allowed := make(map[string]bool, 3)
		if len(parsed.MediaTypes) == 0 {
			for _, t := range instantPriority {
				allowed[t] = true
			}
		} else {
			for _, t := range parsed.MediaTypes {
				allowed[t] = true
			}
		}
		for _, t := range instantPriority {
			if !allowed[t] {
				continue
			}
			single := parsed
			single.MediaTypes = []string{t}
			if h.serveRandomMedia(w, r, single, parsedToArchiveOpts(single)) {
				return
			}
		}
		// No resident media of any allowed kind. Re-walk without the media
		// constraint so a text post can satisfy the request.
		parsed.MediaTypes = nil
		opts = parsedToArchiveOpts(parsed)
	} else if len(parsed.MediaTypes) > 0 {
		if h.serveRandomMedia(w, r, parsed, opts) {
			return
		}
		// No resident media matched (e.g. type:vid where no videos have been
		// downloaded and muxed yet). Fall through to the JSON path so the
		// user gets a matching post's metadata instead of a 503 — the SQL
		// filter still constrains to the requested media types.
	}

	// A cache_score: constraint filters by the media cache eviction score, which can't
	// be expressed in the SQL walk — it is resolved per-post in Go. Pull pages of
	// candidates and keep walking the sweep until one matches; without the
	// constraint a single one-row page is enough (SQL already did all filtering).
	filtered := parsed.CacheScore != nil && h.mediaProxy != nil
	pageN, maxPages := 1, 1
	if filtered {
		pageN, maxPages = randomMediaPoolSize, randomFilterMaxPages
	}
	var sp *store.StoredPost
	rounds := 0
	for page := 0; page < maxPages && sp == nil; page++ {
		posts, roundDone, err := h.randomWalk(r.Context(), parsed, opts, false, pageN)
		if err != nil {
			writeRandomError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(posts) == 0 {
			break // matching subset is empty
		}
		for _, cand := range posts {
			if filtered {
				var cp reddit.Post
				_ = json.Unmarshal(cand.JSONData, &cp)
				if !h.matchCacheScore(parsed.CacheScore, &cp) {
					continue
				}
			}
			sp = cand
			break
		}
		// The cursor starts wherever the previous request left off, so the first
		// roundDone only marks the end of a partial sweep (and a reshuffle into a
		// fresh round). Scan that whole fresh round too — break on the second
		// roundDone, by which point every matching post has been inspected once.
		if roundDone {
			rounds++
			if rounds >= 2 {
				break
			}
		}
	}
	if sp == nil {
		if parsed.Instant {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("no archived post matches the given filters\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"no archived post matches the given filters"}`))
		return
	}

	var post reddit.Post
	_ = json.Unmarshal(sp.JSONData, &post)
	post.ArchivedRelTime, post.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))

	// Instant mode: the user asked for the raw resource, not a JSON envelope.
	// serveRandomMedia already handled the cached-media case and returned true,
	// so reaching here means the matched post has no resident bytes — write its
	// text body (selftext) or fall back to its title.
	if parsed.Instant {
		body := strings.TrimSpace(string(post.Body))
		if body == "" {
			body = sp.Title
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Random-Post", sp.URLPath)
		w.Write([]byte(body))
		return
	}

	resp := map[string]any{
		"url":         sp.URLPath,
		"subreddit":   sp.Subreddit,
		"post_id":     sp.PostID,
		"title":       sp.Title,
		"author":      sp.Author,
		"score":       sp.Score,
		"created_utc": sp.CreatedUTC.Unix(),
		"nsfw":        post.NSFW,
		"post_type":   post.PostType,
		"domain":      post.Domain,
		"media_done":  sp.MediaDone,
	}
	if post.Media.URL != "" {
		resp["media"] = map[string]any{
			"url":     post.Media.URL,
			"alt_url": post.Media.AltURL,
			"width":   post.Media.Width,
			"height":  post.Media.Height,
		}
	}
	if len(post.Gallery) > 0 {
		gallery := make([]map[string]any, len(post.Gallery))
		for i, g := range post.Gallery {
			gallery[i] = map[string]any{
				"url":    g.URL,
				"width":  g.Width,
				"height": g.Height,
			}
		}
		resp["gallery"] = gallery
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

// randomMediaPoolSize bounds how many random candidates the media=images path
// pulls per request. It redirects to the first candidate whose served image is
// genuinely cached on disk, so the redirect always lands on a warm cache (~ms)
// instead of triggering a live Reddit fetch (seconds) on a stale media_done
// flag. A meaningful fraction of media_done rows can be uncached (legacy
// backfill, or a media cache that was dropped and never re-fetched), so a pool
// this size makes an all-miss outcome vanishingly unlikely whenever any cached
// match exists, while keeping the ORDER BY RANDOM() scan trivially cheap.
const randomMediaPoolSize = 64

// randomFilterMaxPages caps how many random-walk pages a Go-post-filtered
// /random request (a cache_score: query) scans before giving up. A highly selective
// filter can leave only a handful of matches in a subset of thousands, so a
// single page often contains none — the cause of intermittent 503s. The walk
// therefore keeps pulling pages until it finds a match OR a full no-replacement
// sweep completes (roundDone) OR it hits this cap. For small/medium subsets
// roundDone stops it after one full pass (reliable: any existing match is found);
// the cap only bites on very large subsets, bounding worst-case work.
const randomFilterMaxPages = 32

// serveRandomMedia answers a /random request whose query pins a media type. It
// pulls a small random pool of matching posts and redirects to the first one
// whose served media is actually on local disk; returns true once it has
// written the response. Returns false when no resident match is found — the
// caller then falls back to the JSON path so the user still gets a random
// matching post (rather than 503 because no bytes happen to be cached).
func (h *Handler) serveRandomMedia(w http.ResponseWriter, r *http.Request, parsed searchquery.Parsed, opts store.ArchiveSearchOpts) bool {
	// A cache_score: constraint is resolved per-post in Go and can be highly selective,
	// so one page often holds no match. Keep walking the sweep until a match is
	// found, a full no-replacement pass completes, or the page cap is hit. Without
	// it a single pool suffices (the cached-on-disk hit rate is high).
	maxPages := 1
	if parsed.CacheScore != nil {
		maxPages = randomFilterMaxPages
	}
	rounds := 0
	for page := 0; page < maxPages; page++ {
		candidates, roundDone, err := h.randomWalk(r.Context(), parsed, opts, true, randomMediaPoolSize)
		if err != nil {
			writeRandomError(w, http.StatusInternalServerError, err.Error())
			return true
		}
		if len(candidates) == 0 {
			break // matching subset is empty
		}
		for _, sp := range candidates {
			var post reddit.Post
			_ = json.Unmarshal(sp.JSONData, &post)
			imgURL := post.Media.URL
			if imgURL == "" && len(post.Gallery) > 0 {
				imgURL = post.Gallery[0].URL
			}
			if imgURL == "" {
				continue
			}
			// Filter on the dynamic existence score (v22): only redirect to a
			// candidate whose media row is scored as physically resident (score
			// <> -1). The score mirrors the disk via the score↔file_path invariant,
			// so this avoids a per-candidate stat() and never lands on a cold URL the
			// proxy would have to live-fetch from Reddit (slow) on a stale media_done
			// flag.
			if !h.mediaProxy.IsResident(reddit.UnformatURL(imgURL)) {
				continue
			}
			// A cache_score: constraint additionally requires the resident media's eviction
			// score to satisfy the threshold (offline-only filter; see matchCacheScore).
			if parsed.CacheScore != nil && !h.matchCacheScore(parsed.CacheScore, &post) {
				continue
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Random-Post", sp.URLPath)
			http.Redirect(w, r, render.WithDownloadTitle(imgURL, sp.Title), http.StatusFound)
			return true
		}
		// The cursor resumes mid-sweep, so the first roundDone ends only a partial
		// pass (and reshuffles into a fresh round). Scan that fresh round too and
		// break on the second roundDone, having now inspected every match once.
		if roundDone {
			rounds++
			if rounds >= 2 {
				break
			}
		}
	}
	return false
}

// randomQueryExpr extracts the /random filter expression, treating `&` as
// equivalent to a space between clauses. net/url splits the raw query on `&`,
// so a browser-friendly URL like `?q=sub:golang&ups>1000&type:image` would otherwise
// arrive as a `q` value of just `sub:golang` plus stray keyless params. We instead
// stitch the raw segments back together — stripping a leading `q=` and
// percent-decoding each — so the whole query string parses identically to
// `?q=sub:golang ups>1000 type:image`. A literal `&` inside a value still works when
// percent-encoded as `%26`, since decoding happens after the split.
//
// Decoding uses PathUnescape (not QueryUnescape) so `+` stays a literal `+`.
// `+` is a meaningful joiner inside grammar values — `sub:golang+rust`,
// `type:vid-gif`, `type:image+vid` — and treating it as the form-encoded space
// would silently shatter those values into separate tokens. Users wanting a
// literal space should percent-encode it as `%20`.
func randomQueryExpr(r *http.Request) string {
	raw := r.URL.RawQuery
	if raw == "" {
		return ""
	}
	var clauses []string
	for _, seg := range strings.Split(raw, "&") {
		if seg == "" {
			continue
		}
		seg = strings.TrimPrefix(seg, "q=")
		if dec, err := url.PathUnescape(seg); err == nil {
			seg = dec
		}
		if seg = strings.TrimSpace(seg); seg != "" {
			clauses = append(clauses, seg)
		}
	}
	return strings.Join(clauses, " ")
}

// randomWalkTTL bounds how long a per-filter cursor survives idle in Redis.
// Long enough that an active browse keeps advancing the same no-replacement
// sweep; short enough that a stale subset (e.g. a one-off filter never revisited)
// is reclaimed and simply restarts its sweep from a fresh golden-ratio origin.
const randomWalkTTL = 24 * time.Hour

// randomWalk drives the no-replacement /random traversal. It keys a per-filter
// cursor by sha256(Canonical()) — so each distinct filter subset (and the media
// vs JSON variant) carries its own independent sweep state in Redis — reads the
// current (origin, cursor), pulls the next page via PostStore.RandomWalk, and
// persists the advanced cursor. When a round completes it reshuffles the whole
// permutation (PostStore.Reshuffle) and rotates the origin by the golden-ratio
// step (x → frac(x + φ)), restarting the next sweep at a maximally-spread entry
// point into a freshly-redrawn shuffle_key. Redis being unavailable is non-fatal:
// the walk degrades to a fresh round from origin 0 each call.
//
// It also returns roundDone: true when this page completed a full no-replacement
// sweep of the matching subset. A caller applying a Go-side post-filter (e.g.
// cache_score:) loops over pages until it finds a match or sees roundDone — at which
// point it has inspected the entire subset once and can conclude "no match"
// rather than 503-ing off a single unlucky page.
func (h *Handler) randomWalk(ctx context.Context, parsed searchquery.Parsed, opts store.ArchiveSearchOpts, mediaOnly bool, n int) ([]*store.StoredPost, bool, error) {
	keyInput := parsed.Canonical()
	if mediaOnly {
		keyInput += "|media"
	}
	sum := sha256.Sum256([]byte(keyInput))
	stateKey := "random:walk:" + hex.EncodeToString(sum[:])

	// Serialize the read-modify-write of this filter's cursor. The sequence below
	// (readWalkState → RandomWalk → writeWalkState) is not atomic, so without this
	// lock concurrent same-filter requests — an image wall firing many
	// <img src="/random?…"> loads at once, or rapid refreshes — would all read the
	// same cursor, walk the same page and serve the same first-resident post: the
	// "clustered identical content" symptom. The lock makes same-filter walks
	// sequential (each advances the cursor before the next reads it) while distinct
	// filters still walk in parallel. Single-instance scope is sufficient: the
	// self-hosted deployment runs one redmemo process.
	unlock := lockRandomWalk(stateKey)

	origin, cursor := h.readWalkState(ctx, stateKey)

	posts, newCursor, roundDone, err := h.postStore.RandomWalk(opts, mediaOnly, origin, cursor, n)
	if err != nil {
		unlock()
		return nil, false, err
	}

	if roundDone {
		// Wrap-around: a full no-replacement sweep just finished. Rotate the origin
		// by the golden-ratio step so the next round enters the permutation at a
		// maximally-spread point, and SIGNAL a background reshuffle to redraw the
		// whole shuffle_key permutation.
		//
		// The reshuffle is the design's only O(N) write — a multi-second blocking
		// UPDATE on a large archive. It must NOT run on this public request path
		// nor under the shard mutex: a narrow filter (small q= subset) completes a
		// sweep in a few requests and would otherwise trigger repeated full-table
		// writes. signalReshuffle hands the work to a single throttled background
		// worker (see random_reshuffle.go) and never blocks. The next round
		// meanwhile replays the existing permutation from the rotated origin —
		// still a valid, well-spread sweep — until the worker redraws shuffle_key.
		origin = frac(origin + store.GoldenRatio)
		cursor = origin
	} else {
		cursor = newCursor
	}
	h.writeWalkState(ctx, stateKey, origin, cursor)
	// Release the shard mutex BEFORE signalling so the (non-blocking) signal — and
	// any worker work it wakes — never happens while holding the lock.
	unlock()
	if roundDone {
		h.signalReshuffle()
	}
	return posts, roundDone, nil
}

// randomWalkLockArr is a fixed array of mutexes that serialize the non-atomic
// cursor read-modify-write of each /random filter, indexed by a hash of the walk
// state key. A previous per-key sync.Map grew without bound because /random is
// public and its free-text q= yields unlimited distinct keys (a slow memory
// leak / DoS on an internet-exposed instance). A fixed array removes the growth
// entirely; an occasional hash collision merely serializes two unrelated filters,
// which is harmless — each still advances its own Redis cursor.
const randomWalkLockShards = 512

var randomWalkLockArr [randomWalkLockShards]sync.Mutex

// lockRandomWalk acquires the shard lock for stateKey and returns its unlock func.
func lockRandomWalk(stateKey string) func() {
	// Inlined FNV-1a over stateKey's bytes: avoids the per-request hasher
	// allocation (fnv.New32a returns a heap pointer) and the string->[]byte
	// copy. Same algorithm and constants as fnv.New32a, so the shard index is
	// identical to the previous implementation.
	const offset32 = 2166136261
	const prime32 = 16777619
	var h uint32 = offset32
	for i := 0; i < len(stateKey); i++ {
		h ^= uint32(stateKey[i])
		h *= prime32
	}
	mu := &randomWalkLockArr[h%randomWalkLockShards]
	mu.Lock()
	return mu.Unlock
}

// readWalkState loads the "origin cursor" float pair for a sweep. A missing or
// malformed value starts a fresh round at origin 0 (which scans the whole subset
// once before the first golden-ratio rotation).
func (h *Handler) readWalkState(ctx context.Context, key string) (origin, cursor float64) {
	if h.cache == nil {
		return 0, 0
	}
	v, err := h.cache.Get(ctx, key)
	if err != nil || v == "" {
		return 0, 0
	}
	parts := strings.Fields(v)
	if len(parts) != 2 {
		return 0, 0
	}
	o, err1 := strconv.ParseFloat(parts[0], 64)
	c, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil {
		return 0, 0
	}
	return o, c
}

func (h *Handler) writeWalkState(ctx context.Context, key string, origin, cursor float64) {
	if h.cache == nil {
		return
	}
	val := strconv.FormatFloat(origin, 'g', -1, 64) + " " + strconv.FormatFloat(cursor, 'g', -1, 64)
	_ = h.cache.Set(ctx, key, val, randomWalkTTL)
}

// frac returns the fractional part of x in [0,1), the wrap used by the golden-
// ratio origin rotation.
func frac(x float64) float64 { return x - math.Floor(x) }

func writeRandomError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
