package handler

import "net/http"

func (h *Handler) handleMedia(w http.ResponseWriter, r *http.Request) {
	h.mediaProxy.ServeMedia(w, r)
}
