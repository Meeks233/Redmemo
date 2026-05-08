package handler

import (
	"fmt"
	"log"
	"net/http"
	"regexp"

	"github.com/redmemo/redmemo/internal/render"
)

var redlibVersionRe = regexp.MustCompile(`v([\d]+\.[\d]+\.[\d]+)`)

func (h *Handler) handleInfo(w http.ResponseWriter, r *http.Request) {
	prefs := readPreferences(r)

	alive := h.isRedlibAlive(r)

	var redlibVersion string
	if alive {
		probeReq, err := http.NewRequestWithContext(r.Context(), "GET", "/", nil)
		if err == nil {
			probeReq.URL.Path = "/info"
			resp, body, err := h.proxy.Forward(probeReq)
			if err == nil && resp.StatusCode == 200 {
				if m := redlibVersionRe.FindSubmatch(body); len(m) > 1 {
					redlibVersion = string(m[1])
				}
			}
		}
	}

	var postCount, subCount int64
	if h.postStore != nil {
		postCount, _ = h.postStore.Count()
		subCount, _ = h.postStore.SubredditCount()
	}

	var mediaCount, mediaSize int64
	if h.mediaStore != nil {
		mediaCount, mediaSize, _ = h.mediaStore.Stats()
	}

	var prefetchSubs []string
	for _, ps := range h.cfg.Prefetch.Subreddits {
		prefetchSubs = append(prefetchSubs, ps.Name)
	}

	data := render.InfoPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		RedlibOnline:   alive,
		RedlibUpstream: h.cfg.Redlib.Upstream,
		RedlibVersion:  redlibVersion,
		PostCount:      postCount,
		SubredditCount: subCount,
		MediaCount:     mediaCount,
		MediaSize:      formatBytes(mediaSize),
		OAuthEnabled:   len(h.cfg.OAuth.Tokens) > 0,
		PrefetchSubs:   prefetchSubs,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderInfo(w, data); err != nil {
		log.Printf("handler: render info: %v", err)
	}
}

func formatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.2f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
