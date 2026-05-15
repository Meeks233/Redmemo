package handler

import "context"

// shouldDegrade reports whether an HR-originated request should bypass the
// upstream Reddit call and instead serve archived content with a banner.
//
// Reasons (in priority order):
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
	if !h.oauthPool.HasAvailableTokens() {
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
