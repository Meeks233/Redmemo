package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/useragent"
	"github.com/redmemo/redmemo/internal/versionintel"
)

type ManagedToken struct {
	StoredToken   store.StoredToken
	Identity      SpoofIdentity
	RateRemaining int
	RateResetAt   time.Time
}

type TokenHolder struct {
	mu          sync.RWMutex
	active      *ManagedToken
	client      *Client
	store       *store.TokenStore
	deviceStore *store.DeviceProfileStore
	tracker     *versionintel.Tracker
	cache       *cache.Cache
	cfg         config.OAuthConfig
	browserUA   *useragent.Pool
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	refreshMu       sync.Mutex
	consecutiveFail int
	lastRefreshAt   time.Time
	backend         string // "mobile_spoof" or "generic_web"

	// uaReady is closed once an OAuth-bound User-Agent first becomes available.
	// Callers (notably the media proxy) wait on it instead of falling back to a
	// pool UA — emitting a different UA than the authoritative session token
	// from the same IP within seconds is a stealth tell we can't afford.
	uaReady     chan struct{}
	uaReadyOnce sync.Once

	// tokenReady is closed only when an OAuth token has actually been installed
	// (restored from store or freshly authenticated). Unlike uaReady, this is
	// NOT closed on startup-auth failure — NP must block here, not fall back to
	// public, until a real session token+UA pair exists.
	tokenReady     chan struct{}
	tokenReadyOnce sync.Once
}

const (
	refreshCooldown     = 10 * time.Second
	maxConsecutiveFails = 5
	// maxRefreshCooldown caps the exponential backoff so a long outage can't
	// stretch the effective cooldown beyond a few minutes — we still want to
	// probe for recovery on a bounded cadence.
	maxRefreshCooldown = 5 * time.Minute
)

func NewTokenHolder(cfg config.OAuthConfig, client *Client, tokenStore *store.TokenStore, deviceStore *store.DeviceProfileStore, tracker *versionintel.Tracker, c *cache.Cache, browserUA *useragent.Pool) *TokenHolder {
	return &TokenHolder{
		client:      client,
		store:       tokenStore,
		deviceStore: deviceStore,
		tracker:     tracker,
		cache:       c,
		cfg:         cfg,
		browserUA:   browserUA,
		backend:     "mobile_spoof",
		uaReady:     make(chan struct{}),
		tokenReady:  make(chan struct{}),
	}
}

// markUAReady signals that an OAuth-bound User-Agent is now available. Wrapped
// in sync.Once so concurrent rotations after the first install are no-ops; the
// read path stays consistent because CurrentUserAgent serves from p.active
// under the holder mutex regardless of channel state.
func (p *TokenHolder) markUAReady() {
	p.uaReadyOnce.Do(func() { close(p.uaReady) })
}

// markTokenReady signals that a real OAuth token is now installed. Unlike
// markUAReady, this is only called on actual install success — not on the
// startup-auth failure path — so WaitForToken can be used by background
// workers (NP) to block until a consistent token+UA pair exists instead of
// falling back to an unauthenticated public request.
func (p *TokenHolder) markTokenReady() {
	p.tokenReadyOnce.Do(func() { close(p.tokenReady) })
}

func (p *TokenHolder) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)

	// Clean up old dynamic tokens from previous architecture.
	if n, err := p.store.DeleteExpiredByBackend("dynamic_spoof"); err != nil {
		log.Printf("oauth: cleanup expired dynamic tokens: %v", err)
	} else if n > 0 {
		log.Printf("oauth: cleaned up %d expired dynamic tokens", n)
	}

	stored, err := p.store.ListEnabled()
	if err != nil {
		return err
	}

	now := time.Now()

	// Pick the first valid token from DB.
	for _, st := range stored {
		if st.Backend == "dynamic_spoof" {
			continue
		}

		identity := p.restoreIdentity(st)

		mt := &ManagedToken{
			StoredToken: *st,
			Identity:    identity,
		}
		if st.RateResetAt != nil && st.RateResetAt.After(now) {
			mt.RateResetAt = *st.RateResetAt
			if st.RateRemaining != nil {
				mt.RateRemaining = *st.RateRemaining
			} else {
				mt.RateRemaining = 99
			}
		} else {
			mt.RateRemaining = 99
			mt.RateResetAt = now.Add(10 * time.Minute)
		}

		p.active = mt
		p.backend = st.Backend
		p.markUAReady()
		p.markTokenReady()
		log.Printf("oauth: restored token %d (%s), remaining=%d", st.ID, st.Backend, mt.RateRemaining)
		break
	}

	// No tokens in DB — authenticate from config.
	if p.active == nil {
		if err := p.authenticateFromConfig(); err != nil {
			log.Printf("oauth: initial auth failed: %v", err)
		}
	}

	if p.active == nil {
		log.Printf("oauth: WARNING: no active token, will retry on first request")
		// Unblock any UA waiter that arrives before the recovery refresh
		// succeeds — they'd otherwise stall for the full caller timeout when
		// there is no token to wait for. A later installToken still calls
		// markUAReady (sync.Once makes it a no-op) and CurrentUserAgent
		// reflects the new token regardless of channel state.
		p.markUAReady()
	}

	p.wg.Add(1)
	go p.refreshLoop(ctx)

	return nil
}

func (p *TokenHolder) authenticateFromConfig() error {
	for _, tcfg := range p.cfg.Tokens {
		if tcfg.Backend == "" {
			tcfg.Backend = "mobile_spoof"
		}
		result, err := p.client.Authenticate(tcfg)
		if err != nil {
			log.Printf("oauth: auth failed for %s/%s: %v", tcfg.Backend, tcfg.ClientID, err)
			continue
		}
		p.installToken(result, tcfg.ClientID, tcfg.ClientSecret, tcfg.Backend)
		return nil
	}

	// No config tokens — try mobile_spoof anonymous.
	result, err := p.client.Authenticate(config.OAuthTokenConfig{Backend: p.backend})
	if err != nil {
		return fmt.Errorf("anonymous auth (%s): %w", p.backend, err)
	}
	p.installToken(result, "", "", p.backend)
	return nil
}

func (p *TokenHolder) installToken(result *TokenResult, clientID, clientSecret, backend string) {
	now := time.Now()
	expiresAt := now.Add(time.Duration(result.ExpiresIn) * time.Second)
	remaining := 99

	st := &store.StoredToken{
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AccessToken:   result.AccessToken,
		ExpiresAt:     &expiresAt,
		RateRemaining: &remaining,
		Backend:       backend,
		Enabled:       true,
		LastUsed:      &now,
		HeadersJSON:   p.identityToJSON(result.Identity),
	}

	if p.active != nil && p.active.StoredToken.ID > 0 {
		st.ID = p.active.StoredToken.ID
	}

	if err := p.store.Upsert(st); err != nil {
		log.Printf("oauth: failed to store token: %v", err)
	}

	p.mu.Lock()
	p.active = &ManagedToken{
		StoredToken:   *st,
		Identity:      result.Identity,
		RateRemaining: 99,
		RateResetAt:   now.Add(10 * time.Minute),
	}
	p.backend = backend
	p.consecutiveFail = 0
	p.lastRefreshAt = now
	p.mu.Unlock()
	p.markUAReady()
	p.markTokenReady()

	log.Printf("oauth: installed new %s token (expires in %ds)", backend, result.ExpiresIn)
}

// effectiveCooldown returns the minimum spacing between refresh attempts given
// the current consecutive-failure count. Below the threshold it is the flat
// refreshCooldown; at and above it the cooldown doubles per extra failure
// (refreshCooldown * 2^(fails-maxConsecutiveFails+1)), capped at
// maxRefreshCooldown. A successful install resets consecutiveFail to 0, so the
// backoff collapses back to the flat cooldown automatically.
func effectiveCooldown(fails int) time.Duration {
	if fails < maxConsecutiveFails {
		return refreshCooldown
	}
	shift := uint(fails - maxConsecutiveFails + 1)
	// Cap the shift before the bit-shift can overflow the duration; any shift
	// past the cap already exceeds maxRefreshCooldown anyway.
	if shift > 16 {
		return maxRefreshCooldown
	}
	cd := refreshCooldown << shift
	if cd > maxRefreshCooldown {
		return maxRefreshCooldown
	}
	return cd
}

func (p *TokenHolder) refreshLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		p.mu.RLock()
		var sleepDur time.Duration
		if p.active != nil && p.active.StoredToken.ExpiresAt != nil {
			sleepDur = time.Until(*p.active.StoredToken.ExpiresAt) - 120*time.Second
		} else {
			sleepDur = 22 * time.Minute
		}
		p.mu.RUnlock()

		if sleepDur < 10*time.Second {
			sleepDur = 10 * time.Second
		}

		timer := time.NewTimer(sleepDur)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}

		// An out-of-band refresh (401 / low_quota) may have installed a fresh
		// token while we slept. If so, the current token is nowhere near
		// expiry — skip this tick and recompute the sleep against the new
		// ExpiresAt, avoiding a needless token mint (extra device ID + UA).
		p.mu.RLock()
		var untilExpiry time.Duration
		if p.active != nil && p.active.StoredToken.ExpiresAt != nil {
			untilExpiry = time.Until(*p.active.StoredToken.ExpiresAt)
		}
		p.mu.RUnlock()
		if untilExpiry > 150*time.Second {
			continue
		}

		log.Printf("oauth: scheduled refresh (pre-expiry)")
		p.forceRefresh("scheduled")
	}
}

// ForceRefresh re-authenticates with a new device identity. Thread-safe with cooldown.
func (p *TokenHolder) forceRefresh(reason string) {
	if p.client == nil {
		return
	}
	if !p.refreshMu.TryLock() {
		return
	}
	defer p.refreshMu.Unlock()

	// lastRefreshAt and consecutiveFail are written under p.mu (installToken /
	// the failure path below), so read them under the same lock — reading them
	// under refreshMu alone is a data race. The effective cooldown escalates
	// once the failure threshold is crossed (see effectiveCooldown).
	p.mu.RLock()
	lastRefresh := p.lastRefreshAt
	fails := p.consecutiveFail
	p.mu.RUnlock()
	if time.Since(lastRefresh) < effectiveCooldown(fails) {
		return
	}

	log.Printf("oauth: force refresh (%s), backend=%s, consecutive_fail=%d", reason, p.backend, fails)

	backend := p.backend

	// Build auth config.
	tcfg := config.OAuthTokenConfig{Backend: backend}
	p.mu.RLock()
	if p.active != nil {
		tcfg.ClientID = p.active.StoredToken.ClientID
		tcfg.ClientSecret = p.active.StoredToken.ClientSecret
	}
	p.mu.RUnlock()

	// Advance the long-term version rotation before minting a token, so the
	// fresh token's spoofed identity tracks the real world. Blocking and
	// bounded — failures degrade gracefully (see versionintel.Tracker).
	p.rotateDeviceVersion()

	result, err := p.client.Authenticate(tcfg)
	if err != nil {
		// consecutiveFail is reset under p.mu in installToken, so mutate it under
		// the same lock to keep the backoff gate's read (above) race-free.
		p.mu.Lock()
		p.consecutiveFail++
		fails = p.consecutiveFail
		p.mu.Unlock()
		log.Printf("oauth: refresh failed (%s): %v (consecutive=%d)", backend, err, fails)
		// generic_web auto-switch is intentionally removed: mobile_spoof is the
		// only active backend, so a failed refresh just retries mobile_spoof.
		// Instead of switching, we slow down: once the failure threshold is
		// crossed the effective cooldown grows exponentially (capped). Log the
		// crossing exactly once so it stands apart from the per-failure line.
		if fails == maxConsecutiveFails {
			log.Printf("oauth: refresh backoff engaged after %d consecutive failures, cooldown now escalating (max %s)", fails, maxRefreshCooldown)
		}
		return
	}

	p.installToken(result, tcfg.ClientID, tcfg.ClientSecret, backend)
}

// rotateDeviceVersion runs the version-intel rotation gates and persists the
// result. Most calls do no network work (the OS poll is monthly and the APK
// rotation is gated by a token-refresh counter); when a gate trips it blocks
// on a bounded external fetch. The updated profile is always persisted — even
// a no-op call advances the rotation counters — and pushed into the auth
// client so the next token is minted with the current identity.
func (p *TokenHolder) rotateDeviceVersion() {
	if p.tracker == nil || p.deviceStore == nil || p.client == nil {
		return
	}
	current := p.client.Profile()
	if current == nil {
		return
	}

	updated, changed := p.tracker.Rotate(context.Background(), *current)
	if err := p.deviceStore.Update(&updated); err != nil {
		log.Printf("oauth: persist rotated device profile: %v", err)
		return
	}
	p.client.SetProfile(&updated)
	if changed {
		log.Printf("oauth: device identity rotated (android=%d, app=%s, build=%s)",
			updated.AndroidVersion, updated.AppVersion, updated.Build)
	}
}

// NotifyUnauthorized is called when a 401 is received. Triggers re-auth.
func (p *TokenHolder) NotifyUnauthorized() {
	go p.forceRefresh("401_unauthorized")
}

// NotifyLowQuota is called when remaining is critically low.
func (p *TokenHolder) NotifyLowQuota() {
	go p.forceRefresh("low_quota")
}

func (p *TokenHolder) Token() *ManagedToken {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active == nil {
		// No token at all (e.g. startup auth failed). Kick off recovery now
		// instead of waiting up to 22 min for the refresh loop's idle tick.
		go p.forceRefresh("no_active_token")
		return nil
	}

	now := time.Now()

	// Reject an expired access token instead of handing it out for a request
	// that would only 401. Trigger a refresh so the next caller recovers.
	if exp := p.active.StoredToken.ExpiresAt; exp != nil && now.After(*exp) {
		log.Printf("oauth: active token expired, triggering refresh")
		go p.forceRefresh("token_expired")
		return nil
	}

	if now.After(p.active.RateResetAt) {
		p.active.RateRemaining = 99
		p.active.RateResetAt = now.Add(10 * time.Minute)
		remaining := 99
		resetAt := p.active.RateResetAt
		p.active.StoredToken.RateRemaining = &remaining
		p.active.StoredToken.RateResetAt = &resetAt
		if p.store != nil {
			snapshot := p.active.StoredToken
			go func() {
				if err := p.store.UpdateToken(&snapshot); err != nil {
					log.Printf("oauth: persist reset: %v", err)
				}
			}()
		}
	}

	if p.active.RateRemaining <= 0 {
		return nil
	}

	return p.active
}

func (p *TokenHolder) OnRequestComplete(tokenID int, resp *http.Response) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active == nil || p.active.StoredToken.ID != tokenID {
		return
	}

	mt := p.active

	if v := resp.Header.Get("X-Ratelimit-Remaining"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			mt.RateRemaining = int(f)
			remaining := int(f)
			mt.StoredToken.RateRemaining = &remaining
		}
	}
	if v := resp.Header.Get("X-Ratelimit-Reset"); v != "" {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			resetAt := time.Now().Add(time.Duration(secs) * time.Second)
			mt.RateResetAt = resetAt
			mt.StoredToken.RateResetAt = &resetAt
		}
	}

	now := time.Now()
	mt.StoredToken.LastUsed = &now

	if mt.RateRemaining < 2 {
		log.Printf("oauth: quota critically low (%d), triggering refresh", mt.RateRemaining)
		go p.NotifyLowQuota()
	}

	if p.store != nil {
		snapshot := mt.StoredToken
		go func() {
			if err := p.store.UpdateToken(&snapshot); err != nil {
				log.Printf("oauth: persist rate state: %v", err)
			}
		}()
	}
}

// CurrentUserAgent returns the active session token's bound User-Agent, or
// "" if no token is loaded. Side-effect free — callers like the media proxy
// use this to keep media fetches on the same identity as the OAuth session.
func (p *TokenHolder) CurrentUserAgent() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == nil {
		return ""
	}
	return p.active.Identity.UserAgent
}

// WaitForUserAgent blocks until the OAuth holder has installed a session token
// and returns its bound User-Agent. Used by the media proxy so a media fetch
// during cold start never emits a pool UA that contradicts the (about-to-be)
// authoritative session identity — emitting two different UAs from one IP in
// the same short window is a stealth tell. Returns "" if ctx is cancelled
// first; the caller should treat that as a transient failure and retry.
func (p *TokenHolder) WaitForUserAgent(ctx context.Context) string {
	if ua := p.CurrentUserAgent(); ua != "" {
		return ua
	}
	waitStart := time.Now()
	log.Printf("oauth: caller blocked waiting for session UA (no active token yet)")
	select {
	case <-p.uaReady:
		ua := p.CurrentUserAgent()
		log.Printf("oauth: session UA available after %s, unblocking caller", time.Since(waitStart).Round(time.Millisecond))
		return ua
	case <-ctx.Done():
		log.Printf("oauth: gave up waiting for session UA after %s: %v", time.Since(waitStart).Round(time.Millisecond), ctx.Err())
		return ""
	}
}

// WaitForToken blocks until an OAuth token has been installed (restored or
// freshly authenticated). Background workers use this instead of falling back
// to a public/unauthenticated request when Token() returns nil: emitting an
// unauthenticated request from the same IP that's about to carry the session
// token contradicts the single-identity stealth model. Returns true once a
// token is ready, or false if ctx is cancelled first.
func (p *TokenHolder) WaitForToken(ctx context.Context) bool {
	select {
	case <-p.tokenReady:
		return true
	default:
	}
	waitStart := time.Now()
	log.Printf("oauth: caller blocked waiting for session token")
	select {
	case <-p.tokenReady:
		log.Printf("oauth: session token available after %s, unblocking caller", time.Since(waitStart).Round(time.Millisecond))
		return true
	case <-ctx.Done():
		log.Printf("oauth: gave up waiting for session token after %s: %v", time.Since(waitStart).Round(time.Millisecond), ctx.Err())
		return false
	}
}

// TokenInstalled reports whether a real OAuth token has ever been installed
// (the one-shot tokenReady signal has fired). It deliberately does NOT report
// current usability: an installed token still makes Token() return nil while it
// is expired-and-refreshing or rate-limited (RateRemaining <= 0). Callers use
// this to tell a cold-start "no token yet" — where blocking on WaitForToken is
// correct — apart from the post-install "token momentarily unusable" case,
// where WaitForToken returns instantly and a retry would fail identically.
func (p *TokenHolder) TokenInstalled() bool {
	select {
	case <-p.tokenReady:
		return true
	default:
		return false
	}
}

// TokenUsable reports whether a session token is usable right now: installed,
// not expired, not mid-refresh, and with rate budget left. It is a thin wrapper
// over Token() (which returns nil in all the unusable cases) so background
// callers can poll local token recovery without issuing an upstream request.
// Like Token(), it may kick a refresh as a side-effect when the active token is
// missing or expired — which is exactly what a recovery poll wants.
func (p *TokenHolder) TokenUsable() bool {
	return p.Token() != nil
}

// RemainingBudget reports the active token's remaining rate-limit budget for the
// current window. Consumed by the handler's TokenSource interface (deps.go).
func (p *TokenHolder) RemainingBudget(_ context.Context) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return 0, nil
	}

	remaining := p.active.RateRemaining
	if time.Now().After(p.active.RateResetAt) {
		remaining = 99
	}
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}

type TokenStatusInfo struct {
	Backend       string
	RateRemaining int
	RateResetAt   time.Time
	Dynamic       bool
	UserAgent     string
	DeviceID      string
	Loid          string
	Session       string
	ExpiresAt     *time.Time
}

func (p *TokenHolder) TokenStatuses() []TokenStatusInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return nil
	}

	return []TokenStatusInfo{{
		Backend:       p.active.StoredToken.Backend,
		RateRemaining: p.active.RateRemaining,
		RateResetAt:   p.active.RateResetAt,
		UserAgent:     p.active.Identity.UserAgent,
		DeviceID:      p.active.Identity.DeviceID,
		Loid:          p.active.Identity.Headers["x-reddit-loid"],
		Session:       p.active.Identity.Headers["x-reddit-session"],
		ExpiresAt:     p.active.StoredToken.ExpiresAt,
	}}
}

// WindowInfo returns the rate limit window state.
func (p *TokenHolder) WindowInfo() (resetAt time.Time, capacity int, remaining int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return
	}

	const window = 10 * time.Minute
	capacity = 99
	now := time.Now()
	tokenReset := p.active.RateResetAt
	if now.After(tokenReset) {
		remaining = 99
		elapsed := now.Sub(tokenReset)
		tokenReset = tokenReset.Add((elapsed/window + 1) * window)
	} else if p.active.RateRemaining > 0 {
		remaining = p.active.RateRemaining
	}
	resetAt = tokenReset
	return
}

// EarliestReset returns (seconds until reset, window total seconds).
func (p *TokenHolder) EarliestReset() (int, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	const window = 10 * time.Minute
	windowSec := int(window.Seconds())

	if p.active == nil {
		return 0, windowSec
	}

	now := time.Now()
	resetAt := p.active.RateResetAt
	if !resetAt.After(now) {
		elapsed := now.Sub(resetAt)
		resetAt = resetAt.Add((elapsed/window + 1) * window)
	}
	secs := int(time.Until(resetAt).Seconds())
	if secs < 0 {
		secs = 0
	}
	return secs, windowSec
}

func (p *TokenHolder) EarliestResetSeconds() int {
	s, _ := p.EarliestReset()
	return s
}

// HasAvailableTokens reports whether the active token has remaining quota.
func (p *TokenHolder) HasAvailableTokens() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.active == nil {
		return false
	}
	// An expired access token is not usable regardless of remaining quota.
	if exp := p.active.StoredToken.ExpiresAt; exp != nil && time.Now().After(*exp) {
		return false
	}
	if p.active.RateRemaining > 0 {
		return true
	}
	return time.Now().After(p.active.RateResetAt)
}

// SpawnTokenIfNeeded is a no-op kept for API compatibility. Single-token model
// handles recovery via NotifyUnauthorized / NotifyLowQuota.
func (p *TokenHolder) SpawnTokenIfNeeded(_ context.Context) {}

func (p *TokenHolder) Stop() {
	// Release any caller blocked in WaitForUserAgent so shutdown isn't held
	// hostage by their per-call timeout. They observe a closed channel,
	// re-read CurrentUserAgent (still "" if we never minted one), and bail.
	p.markUAReady()
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

// --- Identity persistence helpers ---

func (p *TokenHolder) identityToJSON(id SpoofIdentity) *string {
	data := map[string]string{
		"_user_agent": id.UserAgent,
		"_device_id":  id.DeviceID,
	}
	for k, v := range id.Headers {
		data[k] = v
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func (p *TokenHolder) restoreIdentity(st *store.StoredToken) SpoofIdentity {
	if st.HeadersJSON != nil && *st.HeadersJSON != "" {
		var data map[string]string
		if err := json.Unmarshal([]byte(*st.HeadersJSON), &data); err == nil && len(data) > 0 {
			ua := data["_user_agent"]
			deviceID := data["_device_id"]
			delete(data, "_user_agent")
			delete(data, "_device_id")
			log.Printf("oauth: restored identity from DB for token %d (ua=%q)", st.ID, ua)
			return SpoofIdentity{
				UserAgent: ua,
				DeviceID:  deviceID,
				Headers:   data,
			}
		}
	}

	// Fallback: no persisted headers (token row predates identity persistence).
	log.Printf("oauth: no persisted identity for token %d, using pinned device profile", st.ID)
	if st.Backend == "mobile_spoof" || st.Backend == "" {
		if p.client != nil {
			return p.client.DeviceIdentity()
		}
		return GenerateIdentity()
	}
	return genericWebIdentity(p.browserUA)
}
