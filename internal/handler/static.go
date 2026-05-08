package handler

import "net/http"

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

func (h *Handler) handleWiki(w http.ResponseWriter, r *http.Request) {
	// Wiki pages are proxied only — no fallback rendering yet
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
