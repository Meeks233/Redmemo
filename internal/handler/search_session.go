package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
)

// search_session.go implements cross-page repost-spam folding within one
// user's browsing session. RepostKey-based in-page folding (FoldReposts +
// SQL DISTINCT ON repost_key) collapses duplicates that appear ON THE SAME
// PAGE; this layer extends the policy across pagination so a cluster that
// rendered on page 1 cannot reappear on page 3.
//
// State lives in Redis keyed by (sid, query, sort, t) — different sort or
// timeframe is a different search, so each carries its own seen set. The
// value is JSON {titleHash: timesSeen}; truncating titles to 8-byte sha1
// hashes keeps the blob small enough to drop into a single Redis string
// even after dozens of pages.

const (
	searchSessionCookie = "rs_sid"
	searchSessionTTL    = 30 * time.Minute
	searchSessionCookieAge = 7 * 24 * 60 * 60 // 7 days — cookie lifetime ≫ Redis TTL on purpose
)

// ensureSearchSID returns the caller's stable search-session ID, minting a
// fresh cookie if absent. The cookie is HttpOnly + SameSite=Lax + Path=/;
// it has no security purpose beyond being a stable per-browser handle for
// the Redis seen-set.
func (h *Handler) ensureSearchSID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(searchSessionCookie); err == nil && c.Value != "" {
		return c.Value
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should not fail; if it does we degrade to a non-
		// dedup session rather than panic. Empty sid means seenTitles
		// load/save short-circuit and the page just renders un-folded.
		return ""
	}
	sid := hex.EncodeToString(b[:])
	http.SetCookie(w, &http.Cookie{
		Name:     searchSessionCookie,
		Value:    sid,
		Path:     "/",
		MaxAge:   searchSessionCookieAge,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return sid
}

// searchSessionKey composes the Redis key for one (sid, query, sort, t)
// search. Hashing the query fields keeps the key length bounded even for
// extreme constraint-heavy queries.
func searchSessionKey(sid, query, sort, t string) string {
	if sid == "" {
		return ""
	}
	h := sha1.Sum([]byte(query + "\x00" + sort + "\x00" + t))
	return "searchsess:" + sid + ":" + hex.EncodeToString(h[:])
}

// titleFingerprint hashes a normalized title to a short stable handle for
// the seen-set. 8 bytes ⇒ 64 bits; collision risk for the realistic per-
// session seen-set size (a few hundred entries) is negligible.
func titleFingerprint(normalized string) string {
	h := sha1.Sum([]byte(normalized))
	return hex.EncodeToString(h[:8])
}

// seenEntry tags a cluster's token set with the cursor (page) it was first
// rendered for. Subsequent renders use that tag to skip self-suppression on
// the SAME cursor — without this, a cache-busted reload of page 1 would
// absorb every cluster head it just rendered as "already presented" and the
// next render would come back empty.
//
// MediaKey records the cluster head's PrimaryMediaKey at first-render. Two
// posts that share a title-token signature but differ in MediaKey are
// genuinely different content (separate media resources surfaced under
// similar titles — coincidence, or a real distinct repost) and must NOT
// suppress one another across pages.
type seenEntry struct {
	Cursor   string   `json:"c"`
	Tokens   []string `json:"t"`
	MediaKey string   `json:"m,omitempty"`
	// Author is the lowercased author name of the cluster head. When
	// non-empty it acts as a secondary fold key: a later post by the same
	// author with very-high title similarity is treated as the same
	// content even if its MediaKey drifted (Reddit mints a fresh asset ID
	// for every re-upload, so spam-crossposting the same image to N subs
	// yields N different MediaKeys for visually identical content).
	Author string `json:"a,omitempty"`
}

// seenTitles maps titleFingerprint → entry. The fingerprint stays the same
// across reloads of the same cluster so a re-render replaces its prior
// entry in-place. Cross-cursor entries are what session dedup actually
// suppresses against; same-cursor entries are intentionally ignored.
type seenTitles map[string]seenEntry

// loadSeenTitles fetches the per-session map of titleFingerprint → token
// list already presented. A missing key returns an empty (writable) map so
// callers can unconditionally mutate.
func (h *Handler) loadSeenTitles(ctx context.Context, key string) seenTitles {
	if h.cache == nil || key == "" {
		return seenTitles{}
	}
	raw, err := h.cache.Get(ctx, key)
	if err != nil || raw == "" {
		return seenTitles{}
	}
	var m seenTitles
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m == nil {
		return seenTitles{}
	}
	return m
}

// saveSeenTitles writes the updated map back with a sliding TTL. The TTL
// resets on every page render, so a user who pages steadily for two hours
// keeps the same session; a user who walks away has it expire cleanly.
func (h *Handler) saveSeenTitles(ctx context.Context, key string, m seenTitles) {
	if h.cache == nil || key == "" || len(m) == 0 {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = h.cache.Set(ctx, key, string(data), searchSessionTTL)
}

// applySessionDedup drops posts whose title is Jaccard-similar (≥
// jaccardThreshold) to any title rendered for a DIFFERENT cursor of this
// search session. Same-cursor entries are skipped, so a cache-busted reload
// at the same pagination position re-renders the same clusters instead of
// suppressing itself.
//
// The cursor is the after-token (live search) / offset (archive partial),
// or "" for the entry-point first page. Each rendered survivor's entry is
// stamped with the current cursor; entries from prior cursors stay in
// place and continue to suppress matching content downstream.
//
// Posts with too few distinctive tokens (TitleTokens nil) are always kept;
// they are too generic to safely cluster.
func applySessionDedup(posts []reddit.Post, seen seenTitles, cursor string) []reddit.Post {
	if len(posts) == 0 || seen == nil {
		return posts
	}
	// Drop every prior record stamped with the current cursor — this is a
	// reload of the same page, so its previous render's tokens must NOT
	// participate as "already-presented" against itself.
	for fp, e := range seen {
		if e.Cursor == cursor {
			delete(seen, fp)
		}
	}
	out := posts[:0]
	for i := range posts {
		p := posts[i]
		tokens := reddit.TitleTokens(p.Title)
		if tokens == nil {
			out = append(out, p)
			continue
		}
		mk := reddit.PrimaryMediaKey(&posts[i])
		author := strings.ToLower(strings.TrimSpace(p.Author.Name))
		if author == "[deleted]" || author == "deleted" {
			author = ""
		}
		if matchesSeen(tokens, mk, author, seen) {
			continue
		}
		fp := titleFingerprint(strings.Join(tokens, " ") + "\x00" + mk)
		seen[fp] = seenEntry{Cursor: cursor, Tokens: tokens, MediaKey: mk, Author: author}
		out = append(out, p)
	}
	return out
}

// matchesSeen reports whether (tokens, mediaKey, author) collides with any
// entry in the session's seen set. Two ways to match — the same shape as
// FoldReposts so in-page and cross-page folding agree:
//
//  1. Same MediaKey AND title-Jaccard ≥ JaccardThreshold — a genuine
//     repost of the same asset.
//  2. Same Author AND title-Jaccard ≥ jaccardThresholdSameAuthor — the
//     spam-crossposting case where one user re-uploads the same image
//     to N subs; Reddit assigns each upload its own asset ID so MediaKey
//     drifts even though the content is identical.
//
// Callers must already have dropped same-cursor entries before invoking
// this. O(|seen| · t) per candidate; for realistic session sizes (≤ a few
// hundred entries) this is negligible.
func matchesSeen(tokens []string, mediaKey, author string, seen seenTitles) bool {
	for _, e := range seen {
		sim := reddit.JaccardTokens(tokens, e.Tokens)
		if sim >= reddit.JaccardThreshold() {
			if e.MediaKey == mediaKey {
				return true
			}
			if author != "" && e.Author == author && sim >= reddit.JaccardThresholdSameAuthor() {
				return true
			}
		}
		if author != "" && e.Author == author &&
			len(tokens) >= reddit.MinContainmentTokens() && len(e.Tokens) >= reddit.MinContainmentTokens() &&
			reddit.ContainmentTokens(tokens, e.Tokens) >= reddit.ContainmentThresholdSameAuthor() {
			return true
		}
	}
	return false
}
