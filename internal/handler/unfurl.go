package handler

import (
	"encoding/json"
	"net/http"

	"github.com/redmemo/redmemo/internal/store"
)

// unfurlResponse is the JSON the lazy link-preview loader (linkPreview.js)
// consumes. status is "ok" (render a card from the fields) or "failed" (leave
// the plain link). The image/video URLs are the REAL third-party URLs — the
// client loads them directly, RedMemo does not proxy preview media.
type unfurlResponse struct {
	Status      string `json:"status"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Image       string `json:"image,omitempty"`
	ImageWide   bool   `json:"image_wide,omitempty"`
	Video       string `json:"video,omitempty"`
	Site        string `json:"site,omitempty"`
}

// handleUnfurl resolves one external link's preview metadata, lazily, on the
// browser's request as a card scrolls into view. The heavy lifting (cache,
// single-flight, SSRF guard, outbound concurrency cap, fetch-failover chain)
// lives in unfurl.Service; this just validates input and serializes the row.
func (h *Handler) handleUnfurl(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	rawURL := r.URL.Query().Get("url")
	if rawURL == "" || h.unfurl == nil {
		writeUnfurlFailed(w)
		return
	}

	row, err := h.unfurl.ResolveOne(r.Context(), rawURL)
	if err != nil || row == nil || row.Status != store.LinkPreviewOK {
		// A "failed" result is often transient (a host 429/timeout during a busy
		// fetch burst). It must NOT be cached by the browser, or the client would
		// replay the stale failure for the whole max-age even after the server's
		// short failTTL has expired and a re-fetch would now succeed.
		w.Header().Set("Cache-Control", "no-store")
		writeUnfurlFailed(w)
		return
	}

	// An "ok" preview is stable and shared across viewers — let the browser cache
	// it so a re-render (sort change, back/forward) doesn't re-hit the endpoint.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	json.NewEncoder(w).Encode(unfurlResponse{
		Status:      "ok",
		URL:         row.URL,
		Title:       row.Title,
		Description: row.Description,
		Image:       row.ImageURL,
		ImageWide:   row.ImageWide,
		Video:       row.VideoURL,
		Site:        row.SiteName,
	})
}

func writeUnfurlFailed(w http.ResponseWriter) {
	w.Write([]byte(`{"status":"failed"}`))
}
