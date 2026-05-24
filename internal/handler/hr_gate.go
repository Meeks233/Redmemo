package handler

import (
	"context"
	"net/http"
	"net/url"
)

// shouldDegrade reports whether an HR-originated request should bypass the
// upstream Reddit call and instead serve archived content with a banner.
//
// Reasons (in priority order):
//   - "hr_redis_down" — HR rate-limit store (Redis) unreachable; gate fails
//     closed and re-probes Redis with exponential backoff.
//   - "hr_l1" / "hr_l2" / "hr_l3" — HR cooldown active.
//   - "quota_exhausted" — no OAuth token has remaining quota.
//   - ""                — clear to proceed upstream.
//
// HR cooldown wins over quota_exhausted because it's the more specific
// failure mode and informs the user that even idle tokens won't be used.
func (h *Handler) shouldDegrade(ctx context.Context) (degrade bool, reason string) {
	if h.hr != nil {
		if admitted, blockedReason := h.hr.Admit(ctx); !admitted {
			return true, blockedReason
		}
	}
	if !h.oauthHolder.HasAvailableTokens() {
		return true, "quota_exhausted"
	}
	return false, ""
}

// recordUpstream is a shorthand for h.hr.RecordUpstream that tolerates a nil
// manager (e.g. when HR limit is disabled in config).
func (h *Handler) recordUpstream(ctx context.Context) {
	if h.hr != nil {
		h.hr.RecordUpstream(ctx)
	}
}

// redirectFuckReddit issues a 302/307 to the /fuckreddit page, carrying the
// origin request URI (?from=) and degrade reason (?reason=) so the page can
// render context-aware content (a "Go back to ..." escape hatch and the
// specific failure-mode explanation). Both params are optional.
//
// For search routes pass r.URL.RequestURI() (path + raw query) so the user's
// query terms reach the upstream link. For other routes r.URL.Path is enough.
func (h *Handler) redirectFuckReddit(w http.ResponseWriter, r *http.Request, from, reason string) {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if reason != "" {
		q.Set("reason", reason)
	}
	target := "/fuckreddit"
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
}
