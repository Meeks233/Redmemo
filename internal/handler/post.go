package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	h.servePost(w, r, r.PathValue("sub"), r.PathValue("id"))
}

func (h *Handler) handleUserPost(w http.ResponseWriter, r *http.Request) {
	h.servePost(w, r, "u_"+r.PathValue("name"), r.PathValue("id"))
}

func (h *Handler) servePost(w http.ResponseWriter, r *http.Request, sub, id string) {
	t := newReqTimer()
	prefs := h.readPreferences(r)
	// DB rows are keyed by reddit.Post.Permalink, which always ends in "/".
	// The trailing-slash middleware strips that off the request URL before we
	// arrive here, so Get(r.URL.Path) would never match an archived row.
	// Re-append the slash so the upstream_removed gate and archive fallback
	// actually find the stored copy.
	urlPath := r.URL.Path
	if !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}

	// Revive ledger marks on every media URL this post references. The user
	// actively opening the post is the one signal we treat as "maybe it's
	// back now": clear marked_unavailable_at so the next /vid/ or /img/ hit
	// goes through to Reddit once more before re-marking on failure. Best-
	// effort and silent — a DB error just means no revive this turn.
	h.reviveMediaForPost(urlPath)
	commentSort := r.URL.Query().Get("sort")
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}
	if commentSort == "" {
		commentSort = "confidence"
	}

	// 1. Cache — keyed by full prefs fingerprint so a zh visitor never receives
	// the page rendered for an en visitor, nor a theme/NSFW variant they didn't
	// pick. htmlCacheKey appends an FNV tag over every Preferences field.
	rawQuery := ""
	if commentSort != "" {
		rawQuery = "sort=" + commentSort
	}
	cacheKey := htmlCacheKey(urlPath, rawQuery, prefs)
	if cached, _ := h.cache.GetHTML(r.Context(), cacheKey); cached != nil {
		t.mark("cache")
		t.writeHeader(w)
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(cached)
		return
	}
	t.mark("cache")

	// 2. HR gate / OAuth quota. If a prior fetch already tagged this permalink
	// upstream_removed=true we skip the upstream call entirely — there is
	// nothing useful left to fetch and re-asking would just burn quota.
	degrade, reason := h.shouldDegrade(r.Context())
	storedPost, err := h.postStore.Get(urlPath)
	if err != nil {
		// Get returns (nil, nil) on a genuine miss; a non-nil error is a transient
		// DB fault. Log it so a DB hiccup that silently bypasses the archive
		// fallback (storedPost stays nil) is observable rather than invisible.
		log.Printf("post: load archived post %q failed: %v", urlPath, err)
	}
	if !degrade && (storedPost == nil || !storedPost.UpstreamRemoved) {
		if h.renderPostFallback(w, r, sub, id, commentSort, prefs, t, cacheKey) {
			return
		}
		// renderPostFallback may have just flipped upstream_removed; re-read so
		// the archive render below sees the Time Machine badge in this turn.
		storedPost, _ = h.postStore.Get(urlPath)
	}

	// 3. Archive fallback. offline=true only when upstream actually failed
	// (reason==""); when degraded, only the amber degraded banner shows.
	if storedPost != nil {
		h.renderPostFromArchive(w, r, storedPost, prefs, commentSort, reason == "", reason, t, cacheKey)
		return
	}

	// 4. Nothing available
	h.serveDegradeMiss(w, r, reason)
}

func (h *Handler) backgroundArchivePost(sub, id, urlPath, commentSort string, htmlSnapshot []byte) {
	existing, _ := h.postStore.Get(urlPath)
	if existing != nil && time.Since(existing.LastUpdated) < 10*time.Minute {
		return
	}
	// Removed-upstream is sticky: skip the background fetch so we never burn
	// quota re-confirming a permalink Reddit will not give back.
	if existing != nil && existing.UpstreamRemoved {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	post, comments, err := h.fetchPost(ctx, sub, id, commentSort)
	if err != nil {
		log.Printf("background archive post %s/%s: %v", sub, id, err)
		return
	}
	h.archiver.ArchivePost(&post, sub, "background")
	h.archiver.ArchiveComments(post.Permalink, comments)
	if h.prefetcher != nil {
		h.prefetcher.RecordL3Fetch(sub, id, len(comments))
	}

	if len(htmlSnapshot) > 0 {
		permalink := post.Permalink
		if permalink == "" {
			permalink = urlPath
		}
		if err := h.postStore.SaveHTML(permalink, htmlSnapshot); err != nil {
			log.Printf("background save html %s: %v", permalink, err)
		}
	}
}

func (h *Handler) handleRefreshPost(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	id := r.PathValue("id")
	commentSort := r.URL.Query().Get("sort")

	// User-triggered refresh is HR-gated like any foreground request.
	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Reason", reason)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `{"ok":false,"error":"degraded","reason":"%s"}`, reason)
		return
	}

	post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
	h.recordUpstream(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "fetch failed: " + err.Error()})
		return
	}

	urlPath := "/r/" + sub + "/comments/" + id + "/"
	// Removed upstream: do NOT spawn ArchivePost (it would no-op anyway thanks
	// to its post.Removed guard) — instead synchronously flip upstream_removed
	// on the existing row before invalidating the HTML cache, so the reload
	// triggered by this 200 response goes straight to the archive render with
	// the Time Machine badge instead of catching a stale cached page.
	if post.Removed {
		if existing, _ := h.postStore.Get(urlPath); existing != nil && !existing.UpstreamRemoved {
			if err := h.postStore.MarkUpstreamRemoved(urlPath); err != nil {
				log.Printf("handler: mark upstream removed %s: %v", urlPath, err)
			}
		}
		go h.archiver.ArchiveComments(post.Permalink, comments)
	} else {
		go func() {
			h.archiver.ArchivePost(&post, sub, "manual_refresh")
			h.archiver.ArchiveComments(post.Permalink, comments)
		}()
	}
	if h.prefetcher != nil {
		h.prefetcher.RecordL3Fetch(sub, id, len(comments))
	}

	// HTML cache keys now embed a prefs fingerprint; drop every variant under
	// this URL path in one SCAN rather than enumerating known languages.
	if err := h.cache.InvalidateHTMLPrefix(r.Context(), urlPath); err != nil {
		log.Printf("handler: invalidate html prefix %s: %v", urlPath, err)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

// handleLoadMoreComments serves a "fetch N more top-level comments" partial.
// It re-fetches the post with a growing `limit` (each click bumps it by 5),
// then returns ONLY the slice the caller didn't already have, so the client
// appends instead of replaces — preserving any expanded reply state above.
// X-Has-More is "1" while Reddit kept returning more comments at the new
// limit, "0" once we've reached the end (server hides the button on 0).
// Uses the same HR/OAuth gate as a normal post fetch.
func (h *Handler) handleLoadMoreComments(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	id := r.PathValue("id")
	commentSort := r.URL.Query().Get("sort")
	prefs := h.readPreferences(r)
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}
	if commentSort == "" {
		commentSort = "confidence"
	}

	// loaded = how many top-level comments the client already shows.
	// step   = how many extra to reveal on this click (capped server-side).
	loaded := 0
	if v := r.URL.Query().Get("loaded"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 500 {
			loaded = n
		}
	}
	// Step caps at 500 — Reddit's /r/.../comments/<id>.json?limit=N call costs
	// 1 OAuth unit regardless of N, so the client asks for the full remaining
	// batch in one shot instead of forcing dozens of round-trips. Comment
	// images and emotes are loading="lazy" so a 500-comment payload won't
	// fire 500 image GETs at once.
	step := 500
	if v := r.URL.Query().Get("step"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			step = n
		}
	}
	limit := loaded + step

	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("X-Degraded", reason)
		w.WriteHeader(http.StatusOK)
		return
	}

	post, comments, err := h.redditCli.FetchPostLimited(r.Context(), sub, id, commentSort, limit)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: load more comments %s/%s: %v", sub, id, err)
		w.Header().Set("X-Degraded", "upstream_error")
		w.WriteHeader(http.StatusOK)
		return
	}

	go func() {
		h.archiver.ArchivePost(&post, sub, "comments_loadmore")
		h.archiver.ArchiveComments(post.Permalink, comments)
	}()

	// Reddit may tack a trailing "more" placeholder onto the comment listing
	// at the requested limit; strip those before computing slice math so the
	// `loaded` counter stays aligned with actual rendered top-level threads.
	// moreCount is the count of top-level comments still available upstream.
	real, moreCount := splitTopLevelMore(comments)

	hasMore := "0"
	if moreCount > 0 {
		hasMore = "1"
	}

	// Slice off what the client already has so we append, not replace.
	slice := real
	if loaded < len(real) {
		slice = real[loaded:]
	} else {
		slice = nil
	}

	// Drop the cached partial page so the next full reload picks up the new
	// comments instead of the 5-comment snapshot.
	urlPath := "/r/" + sub + "/comments/" + id + "/"
	if err := h.cache.InvalidateHTMLPrefix(r.Context(), urlPath); err != nil {
		log.Printf("handler: invalidate html prefix %s: %v", urlPath, err)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Has-More", hasMore)
	w.Header().Set("X-Added", strconv.Itoa(len(slice)))
	w.Header().Set("X-Remaining", strconv.Itoa(moreCount))
	if err := h.renderer.RenderCommentList(w, slice, prefs); err != nil {
		log.Printf("handler: render load-more comments: %v", err)
	}
}

// handleLoadMoreReplies serves a "fetch the next batch of hidden children for
// one comment" partial. Quota math: one Reddit /api/morechildren call returns
// only the specific child IDs we ask for (plus whatever Reddit inlines as
// their nested replies), so cost stays at 1 quota unit per click — vs. the
// focus-view alternative that re-fetched the full subtree every click and
// silently expanded all remaining children at once.
//
// Query params:
//   - children: comma-separated child IDs, in original Reddit "more" stub order
//   - sort:     comment sort (defaults to user pref → "confidence")
//
// The client takes the first `step` (typically 5) IDs off its stored list,
// sends them here, then trims those IDs out of its data-children for the next
// click. We just forward them to Reddit and render the returned tree.
func (h *Handler) handleLoadMoreReplies(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	id := r.PathValue("id")
	commentSort := r.URL.Query().Get("sort")
	prefs := h.readPreferences(r)
	if commentSort == "" {
		commentSort = prefs.CommentSort
	}
	if commentSort == "" {
		commentSort = "confidence"
	}

	childrenRaw := r.URL.Query().Get("children")
	childIDs := splitChildrenIDs(childrenRaw)
	if len(childIDs) == 0 {
		w.Header().Set("X-Has-More", "0")
		w.Header().Set("X-Added", "0")
		return
	}

	// Archive short-circuit: if every requested child ID is already in our
	// stored comment tree we render straight from disk, skipping the upstream
	// quota burn entirely. Partial hits (some IDs missing) still fall through
	// to Reddit — splitting one call into "ask only for the missing ones" is
	// not worth the code, since /api/morechildren is per-call billed and
	// asking for the full set costs the same as asking for the subset.
	postPermalink := "/r/" + sub + "/comments/" + id + "/"
	if h.commentStore != nil {
		if prior, _ := h.commentStore.GetLatest(postPermalink); prior != nil && len(prior.JSONData) > 0 {
			var priorTree []reddit.Comment
			if json.Unmarshal(prior.JSONData, &priorTree) == nil {
				idx := archive.IndexCommentsByID(priorTree)
				cached := make([]reddit.Comment, 0, len(childIDs))
				for _, cid := range childIDs {
					if c, ok := idx[cid]; ok {
						c.Replies = archive.ExpandMoreFromArchive(c.Replies, idx)
						cached = append(cached, c)
					}
				}
				if len(cached) == len(childIDs) {
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					w.Header().Set("X-Added", strconv.Itoa(len(cached)))
					w.Header().Set("X-Source", "archive")
					if err := h.renderer.RenderReplyList(w, cached, prefs); err != nil {
						log.Printf("handler: render archived replies: %v", err)
					}
					return
				}
			}
		}
	}

	if degrade, reason := h.shouldDegrade(r.Context()); degrade {
		w.Header().Set("X-Degraded", reason)
		w.WriteHeader(http.StatusOK)
		return
	}

	roots, err := h.redditCli.FetchMoreChildren(r.Context(), sub, id, childIDs, commentSort)
	h.recordUpstream(r.Context())
	if err != nil {
		log.Printf("handler: load more replies %s/%s [%d ids]: %v", sub, id, len(childIDs), err)
		w.Header().Set("X-Degraded", "upstream_error")
		w.WriteHeader(http.StatusOK)
		return
	}

	if len(roots) > 0 {
		go h.archiver.ArchiveComments(postPermalink, roots)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Added", strconv.Itoa(len(roots)))
	if err := h.renderer.RenderReplyList(w, roots, prefs); err != nil {
		log.Printf("handler: render load-more replies: %v", err)
	}
}

// splitChildrenIDs is the comma-list parser for the deeper-replies endpoint's
// `children` query param. Caps at 100 (Reddit's per-morechildren ceiling) and
// drops anything outside the base36 ID alphabet so a malformed URL can't be
// smuggled into the upstream call. Returns nil on empty input.
func splitChildrenIDs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 100 {
		parts = parts[:100]
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if isBase36ID(p) {
			out = append(out, p)
		}
	}
	return out
}

func isBase36ID(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'z')) {
			return false
		}
	}
	return true
}

// splitTopLevelMore separates real top-level comments from the trailing "more"
// placeholder Reddit appends when a limit truncates the listing. Returns the
// rendered slice and the upstream's count of remaining top-level comments past
// it (0 when no placeholder is present — i.e., the visitor has the full list).
func splitTopLevelMore(comments []reddit.Comment) ([]reddit.Comment, int) {
	real := make([]reddit.Comment, 0, len(comments))
	moreCount := 0
	for _, c := range comments {
		if c.Kind == "more" {
			moreCount += int(c.MoreCount)
			continue
		}
		real = append(real, c)
	}
	return real, moreCount
}

func (h *Handler) renderPostFallback(w http.ResponseWriter, r *http.Request, sub, id, commentSort string, prefs reddit.Preferences, t *reqTimer, cacheKey string) bool {
	// Coalesce concurrent identical post fetches so a viral post under
	// simultaneous load only burns one OAuth quota unit. recordUpstream and
	// the archiver spawn live inside the leader closure so they fire once
	// per real Reddit hit, not once per merged caller.
	type postFetchResult struct {
		post     reddit.Post
		comments []reddit.Comment
		err      error
	}
	// One shot: ask Reddit for the entire comment listing on the initial view.
	// /r/<sub>/comments/<id>.json is billed per request, so a single call with
	// no `limit` returns every text comment Reddit will give us (default depth
	// ~10) for the same 1 OAuth unit a 5-comment cap used to cost. No more
	// "Load N more comments" round-trips, no more click-to-burn-quota.
	flightKey := "post|" + sub + "|" + id + "|" + commentSort
	raw, _, _ := h.upstreamFlight.Do(flightKey, func() (any, error) {
		post, comments, err := h.redditCli.FetchPost(r.Context(), sub, id, commentSort)
		h.recordUpstream(r.Context())
		res := &postFetchResult{post: post, comments: comments, err: err}
		if err != nil {
			return res, nil
		}
		go func() {
			h.archiver.ArchivePost(&post, sub, "oauth_fallback")
			h.archiver.ArchiveComments(post.Permalink, comments)
		}()
		return res, nil
	})
	res := raw.(*postFetchResult)
	t.mark("upstream")
	if res.err != nil {
		log.Printf("handler: fallback fetch post %s/%s: %v", sub, id, res.err)
		return false
	}
	post, comments := res.post, res.comments

	// If upstream now says the post is removed and we have a prior archive
	// copy, flip the sticky verdict and bail out to the archive renderer in
	// servePost — that path already shows the Time Machine badge from the
	// stored JSON. With no prior archive there is nothing to fall back to, so
	// we keep going and render the removed payload upstream gave us.
	if post.Removed {
		urlPath := "/r/" + sub + "/comments/" + id + "/"
		if existing, _ := h.postStore.Get(urlPath); existing != nil {
			if !existing.UpstreamRemoved {
				if err := h.postStore.MarkUpstreamRemoved(urlPath); err != nil {
					log.Printf("handler: mark upstream removed %s: %v", urlPath, err)
				}
			}
			return false
		}
	}

	if sp, _ := h.postStore.Get(post.Permalink); sp != nil {
		post.ArchivedRelTime, post.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
	}

	// Inline-expand "more" stubs using any deeper-reply children we've
	// previously archived for this thread. The fresh upstream tree only knows
	// about the first `initialCommentLimit` top-level threads; merging the
	// archive in means a returning viewer doesn't have to re-click "More
	// replies" to see subtrees they already loaded last visit.
	if h.commentStore != nil {
		if prior, _ := h.commentStore.GetLatest(post.Permalink); prior != nil && len(prior.JSONData) > 0 {
			var priorTree []reddit.Comment
			if json.Unmarshal(prior.JSONData, &priorTree) == nil {
				comments = archive.ExpandMoreFromArchive(comments, archive.IndexCommentsByID(priorTree))
			}
		}
	}

	realComments, moreCount := splitTopLevelMore(comments)

	data := render.PostPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   render.Version,
		},
		Post:            post,
		Comments:        realComments,
		Sort:            commentSort,
		URLWithoutQuery: r.URL.Path,
		HasOAuth:        h.oauthHolder.HasAvailableTokens(),
		MoreComments:    moreCount,
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderPost(&buf, data); err != nil {
		log.Printf("handler: render post: %v", err)
	}
	t.mark("render")

	t.writeHeader(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "fallback")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
	return true
}

// reviveMediaForPost pulls every media URL referenced by the archived post
// (main media, gallery, thumbnail) and asks the proxy to clear any ledger
// mark on them. No-op when the post isn't archived yet or has no media. The
// upstream form is what the media_unavailable rows were recorded against
// (the local /vid//img/ paths get unfolded by reddit.UnformatURL first).
func (h *Handler) reviveMediaForPost(urlPath string) {
	if h.mediaProxy == nil || h.postStore == nil {
		return
	}
	sp, _ := h.postStore.Get(urlPath)
	if sp == nil || len(sp.JSONData) == 0 {
		return
	}
	var post reddit.Post
	if err := json.Unmarshal(sp.JSONData, &post); err != nil {
		return
	}
	urls := make([]string, 0, 4+len(post.Gallery))
	if u := reddit.UnformatURL(post.Media.URL); u != "" {
		urls = append(urls, u)
	}
	if u := reddit.UnformatURL(post.Media.AltURL); u != "" {
		urls = append(urls, u)
	}
	if u := reddit.UnformatURL(post.Thumbnail.URL); u != "" {
		urls = append(urls, u)
	}
	for _, g := range post.Gallery {
		if u := reddit.UnformatURL(g.URL); u != "" {
			urls = append(urls, u)
		}
	}
	h.mediaProxy.ReviveMedia(urls)
}

func (h *Handler) renderPostFromArchive(w http.ResponseWriter, r *http.Request, sp *store.StoredPost, prefs reddit.Preferences, commentSort string, offline bool, degradedReason string, t *reqTimer, cacheKey string) {
	var post reddit.Post
	if err := json.Unmarshal(sp.JSONData, &post); err != nil {
		h.renderer.RenderError(w, prefs.Lang, "存档数据解析失败", http.StatusInternalServerError)
		return
	}
	post.ArchivedRelTime, post.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
	// The sticky upstream_removed verdict on the StoredPost row drives the
	// Time Machine badge. Old archive JSON that pre-dates the Removed field
	// keeps Removed=false in the JSON itself; the DB row is the source of
	// truth here, so OR them together.
	if sp.UpstreamRemoved {
		post.Removed = true
	}

	var comments []reddit.Comment
	stored, _ := h.commentStore.GetLatest(sp.URLPath)
	if stored != nil {
		json.Unmarshal(stored.JSONData, &comments)
	}
	// Inline-expand any "more" stub whose children are already archived in this
	// same tree (a result of prior partial /api/morechildren writes being merged
	// into the snapshot). The viewer sees the resolved replies directly and
	// doesn't have to click "More replies" again to re-fetch what we already
	// have on disk.
	comments = archive.ExpandMoreFromArchive(comments, archive.IndexCommentsByID(comments))
	t.mark("archive-decode")

	realComments, moreCount := splitTopLevelMore(comments)

	data := render.PostPageData{
		BasePage: render.BasePage{
			URL:            r.URL.Path,
			Prefs:          prefs,
			BrandName:      h.cfg.Render.BrandName,
			Version:        render.Version,
			DegradedReason: degradedReason,
		},
		Post:            post,
		Comments:        realComments,
		Sort:            commentSort,
		URLWithoutQuery: r.URL.Path,
		HasOAuth:        h.oauthHolder.HasAvailableTokens(),
		IsOffline:       offline,
		MoreComments:    moreCount,
	}

	var buf bytes.Buffer
	if err := h.renderer.RenderPost(&buf, data); err != nil {
		log.Printf("handler: render post from archive: %v", err)
	}
	t.mark("render")

	t.writeHeader(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Source", "archive")
	w.Write(buf.Bytes())
	h.cacheHTMLAsync(cacheKey, buf.Bytes())
}
