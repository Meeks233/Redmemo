package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// handleRandom returns one random post from the local archive matching the
// given filters. It never contacts Reddit; if nothing matches it returns 503.
//
// Query parameters (all optional):
//
//	subs       comma-separated subreddit names (case-insensitive)
//	min_score  integer, post.score >= min_score
//	after      RFC3339 date/timestamp or unix seconds; created_utc >= after
//	before     RFC3339 date/timestamp or unix seconds; created_utc <= before
//	nsfw       "include" (default) | "exclude" | "only"
//	media      "images" — only image/gallery/gif posts with cached media
func (h *Handler) handleRandom(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.RandomPostOpts{
		NSFW: q.Get("nsfw"),
	}

	if raw := q.Get("subs"); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s != "" {
				opts.Subs = append(opts.Subs, s)
			}
		}
	}
	if raw := q.Get("min_score"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			writeRandomError(w, http.StatusBadRequest, "invalid min_score")
			return
		}
		opts.MinScore = &v
	}
	if raw := q.Get("after"); raw != "" {
		t, err := parseFlexibleTime(raw)
		if err != nil {
			writeRandomError(w, http.StatusBadRequest, "invalid after")
			return
		}
		opts.After = &t
	}
	if raw := q.Get("before"); raw != "" {
		t, err := parseFlexibleTime(raw)
		if err != nil {
			writeRandomError(w, http.StatusBadRequest, "invalid before")
			return
		}
		opts.Before = &t
	}
	switch opts.NSFW {
	case "", "include", "exclude", "only":
	default:
		writeRandomError(w, http.StatusBadRequest, "invalid nsfw")
		return
	}
	if q.Get("media") == "images" {
		opts.MediaOnly = true
	}

	sp, err := h.postStore.Random(opts)
	if err != nil {
		writeRandomError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sp == nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"no archived post matches the given filters"}`))
		return
	}

	var post reddit.Post
	_ = json.Unmarshal(sp.JSONData, &post)

	// When media=images, redirect straight to the image bytes via the
	// media proxy instead of returning JSON.
	if opts.MediaOnly {
		imgURL := post.Media.URL
		if imgURL == "" && len(post.Gallery) > 0 {
			imgURL = post.Gallery[0].URL
		}
		if imgURL == "" {
			writeRandomError(w, http.StatusServiceUnavailable, "post has no usable image url")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Random-Post", sp.URLPath)
		http.Redirect(w, r, imgURL, http.StatusFound)
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

func writeRandomError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseFlexibleTime(s string) (time.Time, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, &time.ParseError{Value: s, Layout: time.RFC3339}
}
