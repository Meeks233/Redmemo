package handler

import "net/http"

func (h *Handler) staticHandler() http.Handler {
	return h.renderer.StaticHandler()
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
