package handler

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

func (h *Handler) staticHandler() http.Handler {
	return h.renderer.StaticHandler()
}

func (h *Handler) handleRedlibMedia(w http.ResponseWriter, r *http.Request) {
	if h.proxy == nil {
		http.NotFound(w, r)
		return
	}
	resp, body, err := h.proxy.Forward(r)
	if err != nil || resp.StatusCode >= 400 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

var vredditClient = &http.Client{
	Timeout: 60 * 1000 * 1000 * 1000, // 60s as time.Duration
}

func (h *Handler) handleVideoProxy(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	var upstream string
	switch {
	case strings.HasPrefix(path, "/vid/"):
		upstream = "https://v.redd.it/" + strings.TrimPrefix(path, "/vid/")
	case strings.HasPrefix(path, "/hls/"):
		upstream = "https://v.redd.it/" + strings.TrimPrefix(path, "/hls/")
	default:
		http.NotFound(w, r)
		return
	}

	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", upstream, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	for _, hdr := range []string{"Range", "If-Modified-Since", "Cache-Control"} {
		if v := r.Header.Get(hdr); v != "" {
			req.Header.Set(hdr, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		w.Header().Set("Content-Range", cr)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "" {
		w.Header().Set("Accept-Ranges", ar)
	}

	// For HLS manifests, rewrite absolute v.redd.it URLs to local /hls/ paths
	if strings.Contains(ct, "mpegurl") || strings.HasSuffix(path, ".m3u8") {
		body, _ := io.ReadAll(resp.Body)
		s := string(body)
		s = strings.ReplaceAll(s, "https://v.redd.it/", "/vid/")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(s)))
		w.WriteHeader(resp.StatusCode)
		w.Write([]byte(s))
		return
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (h *Handler) handleWiki(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Redlib.Enabled && h.ratelimit.CanRequestRedlib() {
		resp, body, err := h.proxy.Forward(r)
		if err == nil {
			h.ratelimit.Increment()
			w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
	}
	h.renderer.RenderError(w, "Wiki 页面暂时不可用", http.StatusServiceUnavailable)
}
