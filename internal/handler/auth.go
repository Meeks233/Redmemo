package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/totp"
)

// Settings auth gate. Flow per visitor IP:
//   1. Safe-environment confirmation (first contact only — recorded in a
//      cookie so repeat visits skip the prompt).
//   2. Server-secret entry (matched against cfg.Auth.ServerSecret). On the
//      very first successful entry, the gate generates and persists a fresh
//      TOTP secret, then displays the otpauth QR ONCE. Subsequent visits go
//      straight to step 3.
//   3. TOTP code entry. Three wrong codes in the same round lock the IP out
//      until the next 30s TOTP window, and the response redirects to
//      /fuckreddit. A correct code mints an ephemeral access token (HttpOnly
//      cookie) that authorises every /settings request — the token is verified
//      on each call and expires server-side; no sliding window. The lifetime
//      is user-configurable via settings_token_ttl (defaults to 10 minutes,
//      capped at 60).

const (
	authTokenCookie = "redmemo_settings_token"
	safeEnvCookie   = "redmemo_env_ack"
	// trustedCookie carries the persistent "Trust this device" long token. Unlike
	// authTokenCookie (in-memory, minutes) this one is DB-backed, so /settings
	// opens straight away on a trusted browser without a fresh TOTP. Its lifetime
	// is a SLIDING window (trustedTokenTTL): every validated request pushes the
	// expiry forward from that moment, so an actively-used browser never lapses
	// while one left idle past the window dies on its own.
	trustedCookie = "redmemo_trusted_device"
	// trustedTokenTTL is the trusted-device window. It is not an absolute cap but
	// a sliding lifetime renewed on each use (see refreshTrustedDevice / store
	// Check). 30 days bounds the exposure of a stolen-then-abandoned cookie to a
	// month of inactivity — far tighter than an absolute year — without ever
	// logging out a browser that keeps visiting. Brute-forcing the token itself is
	// infeasible at 256 bits regardless of this window; the window only governs
	// blast radius, not guessability.
	trustedTokenTTL = 30 * 24 * time.Hour
	// maxTrustedDevices caps how many live long tokens an instance will hold. A
	// 4th request is dropped with a grace warning — the operator either has too
	// many devices (revoke some) or the gate has been breached.
	maxTrustedDevices = 3
	// trustedSweepInterval is how often expired long tokens are reaped. Validity
	// is already gated on expires_at, so this is hygiene, not correctness.
	trustedSweepInterval = 24 * time.Hour
	// trustFailWindow / trustMaxFailures form the abuse tripwire on trusted-cookie
	// validation: within any rolling trustFailWindow we tolerate at most
	// trustMaxFailures rejected cookie checks. The trustMaxFailures-th failure
	// trips a global ban (trustBanUntil) that, for the rest of the window, refuses
	// ALL trusted-device validation AND new trust requests — a burst of bad
	// cookies reads as an attack, so trust is sealed instance-wide until it cools.
	trustFailWindow  = 30 * time.Minute
	trustMaxFailures = 3
	defaultTokenTTL       = 10 * time.Minute
	maxTokenTTL           = 60 * time.Minute
	maxAttempts           = 3
	lockoutWindow   = totp.Period * time.Second
	// globalMaxAttempts is the instance-wide failure ceiling that backstops the
	// per-IP lockout: when RedMemo sits behind a proxy with no TrustedProxyCIDRs,
	// every client collapses to one IP and the per-IP bucket alone is useless
	// (one attacker locks everyone, or — flipped — the shared bucket dilutes
	// protection). The global counter trips independently of source IP. It is set
	// above maxAttempts so a single legitimate fat-fingered round (up to
	// maxAttempts misses) never trips it on a correctly-configured instance; only
	// a sustained burst does. Self-clears via the same lockoutWindow semantics.
	globalMaxAttempts = 10
	// triesRetention bounds how long an idle per-IP attempt record is kept. The
	// sweep in registerFailure drops entries past this with no active lockout, so
	// a flood of one-off failures from many distinct source IPs cannot grow
	// a.tries without bound (mirrors the tokens / usedCodes GC).
	triesRetention = 10 * time.Minute
	// safeEnvCookieTTL keeps the "this environment is safe" answer short-lived
	// (was 1 year). A day-long ack respects the user's intent without freezing
	// in a stale answer — if they later open /settings from a coffee shop,
	// they re-consent.
	safeEnvCookieTTL = 24 * time.Hour
)

type AuthManager struct {
	serverSecret string
	store        *store.TOTPStore
	devices      trustedDeviceStore

	mu        sync.Mutex
	tokens    map[string]sessionToken // token -> {expiry, bound IP}
	tries     map[string]*attempt     // ip    -> attempt state
	usedCodes map[string]time.Time    // code  -> expiry; single-use enforcement

	// Global backstop (guarded by mu, same as the per-IP state). globalCount is a
	// rolling failed-attempt tally over the current window; globalUntil is set
	// when the ceiling trips and locks ALL source IPs until it elapses. Self-
	// clearing: a failure that lands after globalWindowStart+lockoutWindow resets
	// the tally, so there is no permanent lockout.
	globalCount       int
	globalWindowStart time.Time
	globalUntil       time.Time

	// Trusted-device validation tripwire (guarded by mu). trustFailCount is the
	// rolling tally of rejected cookie checks in the current trustFailWindow;
	// trustFailWindowStart opens that window; trustBanUntil seals all trust
	// access + issuance once trustMaxFailures is reached. Self-clearing on the
	// same window semantics as the brute-force backstop above.
	trustFailCount       int
	trustFailWindowStart time.Time
	trustBanUntil        time.Time
}

type attempt struct {
	count      int
	lockedUntil time.Time
	lastSeen    time.Time
}

// sessionToken is one live ephemeral /settings session: its absolute expiry and
// the client IP it was minted for. Binding the IP means a cookie exfiltrated to
// another host is useless within its short lifetime — it only authorises
// requests coming from the address that passed the TOTP gate.
type sessionToken struct {
	exp time.Time
	ip  string
}

func NewAuthManager(serverSecret string, s *store.TOTPStore, d *store.TrustedDeviceStore) *AuthManager {
	a := &AuthManager{
		serverSecret: serverSecret,
		store:        s,
		tokens:       make(map[string]sessionToken),
		tries:        make(map[string]*attempt),
		usedCodes:    make(map[string]time.Time),
	}
	// Guard the typed-nil-into-interface trap: assigning a nil *TrustedDeviceStore
	// straight into the interface field would make `a.devices == nil` read false
	// and every guarded method dereference a nil pointer. Only wire a live store.
	if d != nil {
		a.devices = d
	}
	return a
}

// hashToken returns the hex SHA-256 of a long token — what we persist and look
// up by, so the plaintext cookie value never touches the database.
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// trustBanned reports whether the global trusted-device tripwire is currently
// tripped — within the ban window no cookie is validated and no new trust is
// granted, regardless of source IP.
func (a *AuthManager) trustBanned() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return time.Now().Before(a.trustBanUntil)
}

// registerTrustFailure records one rejected trusted-cookie check in the rolling
// window. The window self-clears once trustFailWindow has elapsed; the
// trustMaxFailures-th failure inside a live window seals trust instance-wide for
// a further trustFailWindow.
func (a *AuthManager) registerTrustFailure() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.trustFailWindowStart.IsZero() || now.Sub(a.trustFailWindowStart) > trustFailWindow {
		a.trustFailWindowStart = now
		a.trustFailCount = 0
	}
	a.trustFailCount++
	if a.trustFailCount >= trustMaxFailures {
		a.trustBanUntil = now.Add(trustFailWindow)
		log.Printf("[auth] trusted-device tripwire: %d rejected cookie checks in %s — trust sealed instance-wide until %s",
			a.trustFailCount, trustFailWindow, a.trustBanUntil.Format(time.RFC3339))
	}
}

// HasValidTrustedDevice passively re-validates the request's trusted-device
// cookie against the table on EVERY call (no in-process trust caching — a
// revoked row goes dead on the very next request). Flow:
//   - no cookie               -> not a trust attempt, reject without counting;
//   - global ban active       -> reject without touching the DB;
//   - TrustValid              -> allow;
//   - TrustExpired/TrustAbsent-> reject and count a failure (Check already
//     reaped an expired row); the failure feeds the 30-min/3-strike tripwire.
//
// A DB error fails closed without counting (a transient blip is not abuse).
func (a *AuthManager) HasValidTrustedDevice(r *http.Request) bool {
	if a.devices == nil {
		return false
	}
	c, err := r.Cookie(trustedCookie)
	if err != nil || c.Value == "" {
		return false
	}
	if a.trustBanned() {
		return false
	}
	// Sliding window: a valid check pushes the DB expiry forward to now+TTL, so a
	// browser in regular use never lapses. refreshTrustedDevice mirrors this onto
	// the browser cookie (the DB extension alone is moot if the cookie itself
	// still expires at its original time).
	verdict, err := a.devices.Check(hashToken(c.Value), time.Now().Add(trustedTokenTTL))
	if err != nil {
		log.Printf("[auth] check trusted device: %v", err)
		return false
	}
	if verdict == store.TrustValid {
		return true
	}
	// Expired (now cleaned) or absent (revoked / forged): a genuine rejection.
	a.registerTrustFailure()
	return false
}

// refreshTrustedDevice slides the browser-side trusted cookie forward to match
// the DB expiry that HasValidTrustedDevice just extended. Without re-issuing the
// cookie the browser would still discard it at its original expiry, defeating
// the sliding window. Same flags/value as the original mint — only the lifetime
// moves. No-op when the cookie is gone.
func (a *AuthManager) refreshTrustedDevice(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(trustedCookie)
	if err != nil || c.Value == "" {
		return
	}
	exp := time.Now().Add(trustedTokenTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     trustedCookie,
		Value:    c.Value,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(trustedTokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   isTLSRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// issueTrustedDevice mints, persists and cookies a sliding 30-day long token for
// the current browser. Slot policy (maxTrustedDevices total):
//   - global trust ban active        -> refuse (the tripwire seals issuance too);
//   - all slots held by LIVE tokens   -> refuse, surface the grace warning;
//   - one or more slots expired/empty -> batch-reap every expired row to make
//     room, then insert into a freed/empty slot.
//
// A store error also returns false (the user still got their ephemeral session).
func (a *AuthManager) issueTrustedDevice(w http.ResponseWriter, r *http.Request, ip string) bool {
	if a.devices == nil {
		return false
	}
	if a.trustBanned() {
		return false
	}
	// CountActive ignores expired rows, so this is purely the count of LIVE
	// tokens. At the cap every slot is genuinely occupied — refuse. Otherwise
	// there is room (an empty or an expired slot), so reclaim all expired rows
	// in one batch and insert below.
	n, err := a.devices.CountActive()
	if err != nil {
		log.Printf("[auth] count trusted devices: %v", err)
		return false
	}
	if n >= maxTrustedDevices {
		return false
	}
	if _, err := a.devices.DeleteExpired(); err != nil {
		log.Printf("[auth] reap expired trusted devices: %v", err)
		return false
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		log.Printf("[auth] trusted token gen: %v", err)
		return false
	}
	tok := hex.EncodeToString(buf)
	exp := time.Now().Add(trustedTokenTTL)
	// Show only the leading chars in the management table — enough to tell rows
	// apart, not enough to reconstruct the token.
	prefix := tok[:8]
	if err := a.devices.Create(hashToken(tok), prefix, ip, exp); err != nil {
		log.Printf("[auth] persist trusted device: %v", err)
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     trustedCookie,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(trustedTokenTTL.Seconds()),
		HttpOnly: true,
		Secure:   isTLSRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
	return true
}

// ListTrustedDevices surfaces the live long tokens for the settings management
// table. Returns nil on a nil store / error.
func (a *AuthManager) ListTrustedDevices() ([]store.TrustedDevice, error) {
	if a.devices == nil {
		return nil, nil
	}
	return a.devices.ListActive()
}

// RevokeAllTrustedDevices wipes every trusted-device long token. Wired into the
// TOTP reset/rotation paths: a long-lived trusted cookie outlives the secret it
// was minted under, so changing the second factor must also kill every trusted
// device or a stolen cookie keeps access across the reset. No-op (nil, no error)
// when the store is absent.
func (a *AuthManager) RevokeAllTrustedDevices() (int64, error) {
	if a.devices == nil {
		return 0, nil
	}
	return a.devices.DeleteAll()
}

// RevokeTrustedDevice deletes one long token by id (operator-initiated). If the
// operator revokes the device they are currently on, the next request simply
// fails the DB lookup and falls back to the TOTP gate — the now-orphaned cookie
// is harmless and expires on its own.
func (a *AuthManager) RevokeTrustedDevice(id int64) error {
	if a.devices == nil {
		return nil
	}
	_, err := a.devices.Revoke(id)
	return err
}

// requestIsTrustedDevice reports whether the request's trusted-device cookie
// hashes to wantHash — i.e. the row identified by wantHash is the very browser
// making this request. Used by self-revoke to tell "I revoked my own device"
// from "I revoked some other device" so only the former tears down the caller's
// own session. Deliberately bypasses the abuse tripwire (it is a plain hash
// comparison, not a validation attempt).
func (a *AuthManager) requestIsTrustedDevice(r *http.Request, wantHash string) bool {
	if wantHash == "" {
		return false
	}
	c, err := r.Cookie(trustedCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return hashToken(c.Value) == wantHash
}

// deviceHashByID returns the stored hash for one trusted-device row (or "" when
// the store is absent / the row is gone).
func (a *AuthManager) deviceHashByID(id int64) (string, error) {
	if a.devices == nil {
		return "", nil
	}
	return a.devices.HashByID(id)
}

// logout fully de-authorises the request's browser: it drops the in-memory
// ephemeral session token (so the still-cookied value can't authorise another
// request), then clears both the session and the trusted-device cookies. Used
// on self-revoke so access stops on the spot rather than living out the cookie.
func (a *AuthManager) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(authTokenCookie); err == nil && c.Value != "" {
		a.mu.Lock()
		delete(a.tokens, c.Value)
		a.mu.Unlock()
	}
	a.clearToken(w)
	http.SetCookie(w, &http.Cookie{
		Name:     trustedCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// StartTrustedSweeper runs a daily reaper that drops expired long tokens. It
// fires once on start, then on a 24h ticker, and exits when ctx is cancelled —
// the same lifecycle shape as the media evictor.
func (a *AuthManager) StartTrustedSweeper(ctx context.Context) {
	if a.devices == nil {
		return
	}
	go func() {
		sweep := func() {
			if n, err := a.devices.DeleteExpired(); err != nil {
				log.Printf("[auth] trusted device sweep: %v", err)
			} else if n > 0 {
				log.Printf("[auth] trusted device sweep: revoked %d expired token(s)", n)
			}
		}
		sweep()
		ticker := time.NewTicker(trustedSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sweep()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// HasValidToken reports whether the request carries a still-live ephemeral auth
// cookie that was issued to this same client IP. Expired tokens are GC'd on the
// spot. A token replayed from a different IP (theft / exfiltration) is rejected
// but NOT deleted — the address it was minted for can still use it until expiry.
func (a *AuthManager) HasValidToken(r *http.Request, ip string) bool {
	c, err := r.Cookie(authTokenCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t, ok := a.tokens[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(t.exp) {
		delete(a.tokens, c.Value)
		return false
	}
	// IP binding: the cookie only authorises the address that passed the gate.
	if t.ip != ip {
		return false
	}
	return true
}

// issueToken mints a fresh ephemeral session cookie bound to ip. ttl is clamped
// to (0, maxTokenTTL] — a zero/negative argument falls back to defaultTokenTTL so
// a misconfigured setting can never produce an immediately-expired cookie or
// outrun the gate's threat model. The token is recorded against ip so a later
// request must come from the same address to be honoured (see HasValidToken).
func (a *AuthManager) issueToken(w http.ResponseWriter, r *http.Request, ttl time.Duration, ip string) {
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	if ttl > maxTokenTTL {
		ttl = maxTokenTTL
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "token gen failed", http.StatusInternalServerError)
		return
	}
	tok := hex.EncodeToString(buf)
	exp := time.Now().Add(ttl)
	a.mu.Lock()
	a.tokens[tok] = sessionToken{exp: exp, ip: ip}
	// opportunistic GC of expired tokens
	for k, v := range a.tokens {
		if time.Now().After(v.exp) {
			delete(a.tokens, k)
		}
	}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     authTokenCookie,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   isTLSRequest(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// isTLSRequest returns true when the request reached the server over TLS,
// either directly or via a trusted reverse proxy that set X-Forwarded-Proto.
// Used to gate the Secure cookie flag — never lie to the browser by setting
// Secure on a plain-HTTP connection (the cookie would be silently dropped).
func isTLSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		return true
	}
	return false
}

// resolveTokenTTL maps the siteDefaults setting to a concrete duration, clamped
// to the allowed band. The save handler already whitelists the input, but the
// clamp here keeps a hand-edited DB or stale value from issuing an out-of-band
// cookie lifetime.
func (h *Handler) resolveTokenTTL() time.Duration {
	v := h.siteDefault("settings_token_ttl")
	if v == "" {
		return defaultTokenTTL
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultTokenTTL
	}
	d := time.Duration(n) * time.Minute
	if d > maxTokenTTL {
		d = maxTokenTTL
	}
	return d
}

func (a *AuthManager) clearToken(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     authTokenCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// registerFailure bumps the per-IP miss counter; on the 3rd miss it parks
// the IP until the next TOTP rotation and returns locked=true (caller must
// redirect to /fuckreddit). remaining is how many tries are left in this round
// before lockout — 0 once locked, otherwise maxAttempts-count.
func (a *AuthManager) registerFailure(ip string) (locked bool, remaining int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	// Opportunistic GC: drop stale, un-locked attempt records so a stream of
	// failed attempts from many distinct source IPs cannot grow a.tries without
	// bound (the tokens / usedCodes maps sweep the same way).
	for k, t := range a.tries {
		if now.After(t.lockedUntil) && now.Sub(t.lastSeen) > triesRetention {
			delete(a.tries, k)
		}
	}
	// Global backstop: a failure increments BOTH the per-IP and the instance-wide
	// tally; the gate locks if EITHER trips. The global window is rolling and
	// self-clearing — once lockoutWindow has elapsed since it opened, the next
	// failure starts a fresh window rather than accumulating forever.
	if a.globalWindowStart.IsZero() || now.Sub(a.globalWindowStart) > lockoutWindow {
		a.globalWindowStart = now
		a.globalCount = 0
	}
	a.globalCount++
	if a.globalCount >= globalMaxAttempts {
		a.globalUntil = now.Add(lockoutWindow)
		a.globalCount = 0
		return true, 0
	}

	st := a.tries[ip]
	if st == nil {
		st = &attempt{}
		a.tries[ip] = st
	}
	st.count++
	st.lastSeen = now
	if st.count >= maxAttempts {
		st.lockedUntil = now.Add(lockoutWindow)
		st.count = 0
		return true, 0
	}
	return false, maxAttempts - st.count
}

// locked reports whether the IP is currently in the cool-down window.
func (a *AuthManager) locked(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Global backstop wins regardless of source IP — under a shared-IP misconfig
	// this is the only lockout that still discriminates a brute-force burst. The
	// window self-clears as time.Until goes non-positive.
	if d := time.Until(a.globalUntil); d > 0 {
		return true, d
	}
	st := a.tries[ip]
	if st == nil {
		return false, 0
	}
	if d := time.Until(st.lockedUntil); d > 0 {
		return true, d
	}
	return false, 0
}

// consumeCode atomically verifies code against secret and, on success, marks
// it consumed for its remaining validity window. Returns (ok, replay):
//   - ok=false                  -> code invalid
//   - ok=true,  replay=true     -> code valid but already used; caller MUST
//                                  refuse to mint a token (surface compromise)
//   - ok=true,  replay=false    -> caller may mint a token
//
// Atomicity matters: a naive verify-then-mark sequence lets two concurrent
// requests both pass Verify before either records the code, defeating the
// single-use guarantee.
func (a *AuthManager) consumeCode(secret, code string, now time.Time) (ok, replay bool) {
	if !totp.Verify(secret, code, now) {
		return false, false
	}
	const ttl = 3 * totp.Period * time.Second
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, exp := range a.usedCodes {
		if now.After(exp) {
			delete(a.usedCodes, k)
		}
	}
	if exp, used := a.usedCodes[code]; used && now.Before(exp) {
		return true, true
	}
	a.usedCodes[code] = now.Add(ttl)
	return true, false
}

func (a *AuthManager) resetAttempts(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tries, ip)
}

// constantTimeEqual avoids leaking secret length via comparison timing.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// requireSettingsAuth gates every /settings entry point. Behaviour:
//   - holds a valid ephemeral token -> next.ServeHTTP
//   - otherwise -> render the auth page (GET) or process the form (POST),
//     never falling through to the underlying settings handler.
// Every POST also gets an Origin/Referer same-origin check (a belt to go with
// SameSite=Lax's suspenders) so a cross-site form submission can't drive any
// /settings action even if a browser ever relaxes its Lax cookie behaviour.
func (h *Handler) requireSettingsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Bypass mode: TOTP gate disabled instance-wide. POSTs still get the
		// same-origin CSRF check — that's the only brake left, and it costs
		// nothing on legitimate browser submissions.
		if h.cfg.Auth.BypassAuth {
			if r.Method == http.MethodPost && !isSameOriginPost(r) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if h.auth == nil { // safety: tests / misconfig — fail closed
			http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && !isSameOriginPost(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		ip := h.clientIP(r)
		if h.auth.HasValidToken(r, ip) {
			next.ServeHTTP(w, r)
			return
		}
		if h.auth.HasValidTrustedDevice(r) {
			// Slide the browser cookie forward to match the DB expiry the check
			// just extended, then admit the request.
			h.auth.refreshTrustedDevice(w, r)
			next.ServeHTTP(w, r)
			return
		}
		h.serveAuthGate(w, r)
	}
}

// isSameOriginPost reports whether a POST's Origin/Referer header points back
// at the same host the request reached us on. Absent both, refuse — a modern
// browser submitting a real form always emits at least one; only some old
// non-browser tooling skips them, and /settings is not an automation target.
func isSameOriginPost(r *http.Request) bool {
	host := r.Host
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, host)
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		if err != nil || u.Host == "" {
			return false
		}
		return strings.EqualFold(u.Host, host)
	}
	return false
}

// authStage selects which form to display at any given moment.
type authStage int

const (
	stageSafeEnv authStage = iota
	stageServerSecret
	stageEnrollTOTP // QR + first code
	stageTOTPCode
)

func (h *Handler) currentStage(r *http.Request) authStage {
	real := h.realStage()
	// The safe-environment warning only guards the stages that put a long-lived
	// secret on screen or in an input: typing the server secret, or scanning the
	// enrollment QR. The routine post-enrollment flow only asks for an ephemeral
	// 6-digit TOTP code, which exposes nothing worth warning about — so it skips
	// the prompt entirely. We still gate that warning behind the safe-env cookie
	// for the sensitive stages.
	if real == stageServerSecret || real == stageEnrollTOTP {
		if c, _ := r.Cookie(safeEnvCookie); c == nil || c.Value != "ok" {
			return stageSafeEnv
		}
	}
	return real
}

// realStage reports the actual gate stage given enrollment state, independent of
// the safe-environment warning (which currentStage layers on top for the
// secret-exposing stages only).
func (h *Handler) realStage() authStage {
	// stageServerSecret is implicit — the gate only advances past it when the
	// secret has been submitted in the same request. Stateless on purpose:
	// the server secret must be re-entered on every fresh round (no cookie).
	secret, err := h.auth.store.Secret()
	if err != nil {
		// Fail closed: a transient DB read error must NOT be read as "not
		// enrolled" - that path would let the server_secret POST mint a brand
		// new secret over an existing enrollment. Show the server-secret form;
		// its POST handler fails closed on a read error too.
		return stageServerSecret
	}
	if secret == "" {
		return stageServerSecret // first enrollment needs server-secret first
	}
	// A persisted-but-unconfirmed secret means enrollment was interrupted after
	// the QR was shown. Re-show the QR (stageEnrollTOTP) so the owner can finish
	// instead of being stranded at a bare code prompt for a secret they never
	// captured. On a confirmed-flag read error, assume confirmed so a transient
	// blip can't re-expose the QR.
	if confirmed, cerr := h.auth.store.Confirmed(); cerr == nil && !confirmed {
		return stageEnrollTOTP
	}
	return stageTOTPCode
}

func (h *Handler) serveAuthGate(w http.ResponseWriter, r *http.Request) {
	ip := h.clientIP(r)

	// Lock-out wins over everything else: in the cooldown window the only
	// response is a redirect to /fuckreddit (the goal's "wait for next round").
	if locked, _ := h.auth.locked(ip); locked {
		http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodPost {
		h.handleAuthPost(w, r, ip)
		return
	}
	h.renderAuthPage(w, r, h.currentStage(r), "")
}

func (h *Handler) handleAuthPost(w http.ResponseWriter, r *http.Request, ip string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	stage := r.FormValue("stage")
	switch stage {
	case "safe_env":
		if r.FormValue("confirm") == "yes" {
			http.SetCookie(w, &http.Cookie{
				Name:     safeEnvCookie,
				Value:    "ok",
				Path:     "/",
				MaxAge:   int(safeEnvCookieTTL.Seconds()),
				HttpOnly: true,
				Secure:   isTLSRequest(r),
				SameSite: http.SameSiteLaxMode,
			})
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/fuckreddit?reason=unsafe_env", http.StatusSeeOther)

	case "server_secret":
		entered := r.FormValue("secret")
		if !constantTimeEqual(entered, h.auth.serverSecret) {
			if locked, _ := h.auth.registerFailure(ip); locked {
				http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
				return
			}
			h.renderAuthPage(w, r, stageServerSecret, "incorrect server secret")
			return
		}
		// Re-enrollment guard: when a TOTP secret already exists, rotating it
		// requires proof of the CURRENT authenticator code in the same submit.
		// Without this, a leaked server secret alone is enough to silently
		// rotate the second factor and lock the legitimate owner out. The
		// admin escape hatch stays `redmemo --reset-totp` (clears the secret
		// from the DB, after which the next server_secret POST enrolls fresh).
		existing, err := h.auth.store.Secret()
		if err != nil {
			// Fail closed: a DB read error here must never be treated as "no
			// secret enrolled" - that path mints a fresh secret and would let a
			// leaked server secret silently rotate the second factor whenever the
			// read transiently fails.
			http.Error(w, "auth backend unavailable", http.StatusServiceUnavailable)
			return
		}
		if existing != "" {
			code := strings.TrimSpace(r.FormValue("current_code"))
			if code == "" {
				h.renderAuthPage(w, r, stageServerSecret, "TOTP is already enrolled — also enter the current 6-digit code to rotate it, or run `redmemo --reset-totp` first")
				return
			}
			ok, replay := h.auth.consumeCode(existing, code, time.Now())
			if !ok || replay {
				if locked, _ := h.auth.registerFailure(ip); locked {
					http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
					return
				}
				h.renderAuthPage(w, r, stageServerSecret, "current TOTP code did not match")
				return
			}
		}
		// Correct. Mint and persist the TOTP secret (one-shot) and reveal QR.
		secret, err := totp.NewSecret()
		if err != nil {
			http.Error(w, "secret gen failed", http.StatusInternalServerError)
			return
		}
		if err := h.auth.store.SetSecret(secret); err != nil {
			http.Error(w, "secret persist failed", http.StatusInternalServerError)
			return
		}
		// Rotation (existing != "") swaps the second factor. Any "Trust this
		// device" cookie was minted under the OLD secret and would otherwise keep
		// authorising /settings for weeks — defeating the rotation. Wipe
		// every trusted device so changing the factor truly re-gates all browsers.
		// First-enrollment (existing == "") has no rows, so this only matters on
		// rotation; a wipe error is logged, not fatal (the new secret is already
		// persisted and the daily sweep / next reset will catch any stragglers).
		if existing != "" {
			if n, rerr := h.auth.RevokeAllTrustedDevices(); rerr != nil {
				log.Printf("[auth] revoke trusted devices on TOTP rotation: %v", rerr)
			} else if n > 0 {
				log.Printf("[auth] TOTP rotated — revoked %d trusted device(s)", n)
			}
		}
		h.auth.resetAttempts(ip)
		h.renderEnrollment(w, r, secret, "")

	case "enroll_confirm":
		secret, _ := h.auth.store.Secret()
		if secret == "" {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		code := r.FormValue("code")
		ok, replay := h.auth.consumeCode(secret, code, time.Now())
		if !ok {
			if locked, _ := h.auth.registerFailure(ip); locked {
				// Roll back the enrollment so the next round starts fresh —
				// otherwise an attacker who tripped the lockout would gain a
				// pre-baked secret on retry.
				h.auth.store.Reset()
				http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
				return
			}
			h.renderEnrollment(w, r, secret, "code did not match — try the next 30s window")
			return
		}
		if replay {
			http.Redirect(w, r, "/fuckreddit?reason=totp_replay", http.StatusSeeOther)
			return
		}
		h.auth.resetAttempts(ip)
		// Mark enrollment confirmed so an interrupted enrollment (secret
		// persisted but first code never entered) is no longer mistaken for a
		// completed one. A persist failure here is non-fatal: the worst case is
		// the QR is re-shown on the next visit, which is recoverable.
		if err := h.auth.store.MarkConfirmed(); err != nil {
			log.Printf("[auth] mark TOTP confirmed: %v", err)
		}
		h.redirectAfterUnlock(w, r, ip)

	case "totp":
		secret, _ := h.auth.store.Secret()
		if secret == "" {
			// Enrollment was wiped mid-flight (admin reset). Restart.
			h.renderAuthPage(w, r, stageServerSecret, "")
			return
		}
		code := r.FormValue("code")
		ok, replay := h.auth.consumeCode(secret, code, time.Now())
		if !ok {
			locked, remaining := h.auth.registerFailure(ip)
			if locked {
				http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
				return
			}
			noun := "attempts"
			if remaining == 1 {
				noun = "attempt"
			}
			h.renderAuthPage(w, r, stageTOTPCode, fmt.Sprintf("Incorrect code — %d %s left", remaining, noun))
			return
		}
		if replay {
			http.Redirect(w, r, "/fuckreddit?reason=totp_replay", http.StatusSeeOther)
			return
		}
		h.auth.resetAttempts(ip)
		h.redirectAfterUnlock(w, r, ip)

	default:
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	}
}

// redirectAfterUnlock finishes a successful code entry by handing the browser
// exactly ONE credential, never two. If the operator ticked "Trust this device"
// and a long-token slot is free, we mint the sliding 30-day trusted cookie and
// stop — that cookie authorises every future /settings call on its own, so also
// minting an ephemeral session token would just burn a map slot for no added
// access. Otherwise (trust not requested, or the maxTrustedDevices cap is full)
// we fall back to the short-lived, IP-bound session token; the cap case
// additionally routes to /settings?trusted=limit so the management area can
// surface the grace warning.
func (h *Handler) redirectAfterUnlock(w http.ResponseWriter, r *http.Request, ip string) {
	if r.FormValue("trust_device") == "on" {
		if h.auth.issueTrustedDevice(w, r, ip) {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		// Cap full: no long token issued. Fall through to an ephemeral session so
		// the user still gets in this round, and flag the limit.
		h.auth.issueToken(w, r, h.resolveTokenTTL(), ip)
		http.Redirect(w, r, "/settings?trusted=limit", http.StatusSeeOther)
		return
	}
	h.auth.issueToken(w, r, h.resolveTokenTTL(), ip)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// qrDataURI renders the otpauth QR as a base64 data: URI so the enrollment
// page can embed it inline. The QR is only ever produced server-side during
// the freshly-completed server-secret POST and rendered ONCE in the response —
// no public endpoint exposes the secret. This eliminates the prior attack
// where any unauthenticated visitor could GET /settings/qr and recover the
// TOTP secret after first enrollment.
func qrDataURI(secret string) (string, error) {
	png, err := totp.QRCodePNG(secret, 256)
	if err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), nil
}

var authPageTpl = template.Must(template.New("auth").Parse(`<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>RedMemo · Authenticate</title>
<link rel="stylesheet" href="/style.css">
{{if .ThemeStylesheet}}<link rel="stylesheet" href="/themes/{{.Theme}}.css">{{end}}
{{if .AutoThemeCSS}}{{.AutoThemeCSS}}{{end}}
<style>
  /* Layout + responsiveness reuse style.css's centred single-panel pattern
     (#error wrapper + .prefs panel: width:100% capped at max-width). Unlike the
     error pages (top-anchored), the auth card is centred both axes like .popup
     — full-viewport flex centring. min-height (not height) keeps tall content
     such as the enrollment QR scrollable instead of clipped. 100dvh tracks the
     mobile dynamic viewport so the card stays centred under browser chrome. */
  #error{align-items:center;min-height:100vh;min-height:100dvh;padding:2em 1em;box-sizing:border-box}
  .prefs{padding:2rem;margin-bottom:0}
  h1{margin-top:0;font-size:1.2rem;color:var(--text)}
  input[type=text],input[type=password]{width:100%;padding:.6rem;margin:.5rem 0 1rem;background:var(--outside);color:var(--text);border:var(--panel-border);border-radius:4px;font-family:monospace;font-size:1rem;box-sizing:border-box}
  input[type=text]:focus,input[type=password]:focus{outline:2px solid var(--accent);outline-offset:1px}
  button{padding:.6rem 1.2rem;background:var(--accent);color:var(--foreground);border:0;border-radius:4px;cursor:pointer;font-weight:bold}
  button.alt{background:var(--highlighted);color:var(--text)}
  .err{color:var(--nsfw);margin:.5rem 0}
  .muted{color:var(--visited);font-size:.85rem}
  /* "Trust this device" opt-in — mirrors the settings page's checkbox rows. */
  label.trust{display:flex;align-items:center;gap:.5rem;margin:.25rem 0 1rem;color:var(--text);font-size:.9rem;cursor:pointer;user-select:none}
  label.trust input{width:auto;margin:0;flex:0 0 auto}
  img.qr{display:block;margin:1rem auto;background:#fff;padding:.5rem;border-radius:4px}
  code{display:block;padding:.5rem;background:var(--outside);color:var(--text);border-radius:4px;font-family:monospace;word-break:break-all}
  code.inline{display:inline;padding:.1em .35em;word-break:normal}
  p{color:var(--text)}
  /* Segmented one-time-code input (progressive enhancement of .otp-input) */
  .otp{display:flex;gap:.5rem;margin:.5rem 0 1rem;justify-content:center}
  .otp-cell{width:100%;max-width:3rem;aspect-ratio:3/4;padding:0;text-align:center;font-family:monospace;font-size:1.5rem;font-weight:600;color:var(--text);background:var(--outside);border:2px solid transparent;border-radius:8px;box-shadow:var(--panel-border) 0 0 0 1px inset;transition:border-color .15s,box-shadow .15s;box-sizing:border-box;-moz-appearance:textfield}
  .otp-cell::-webkit-outer-spin-button,.otp-cell::-webkit-inner-spin-button{-webkit-appearance:none;margin:0}
  .otp-cell:hover{border-color:var(--highlighted)}
  .otp-cell:focus{outline:none;border-color:var(--accent);box-shadow:var(--accent) 0 0 0 1px inset,0 0 0 3px color-mix(in srgb,var(--accent) 25%,transparent)}
  .otp-cell.filled{border-color:var(--accent)}
  .otp.error .otp-cell{border-color:var(--nsfw)}
  .otp.shake{animation:otp-shake .4s cubic-bezier(.36,.07,.19,.97) both}
  @keyframes otp-shake{10%,90%{transform:translateX(-1px)}20%,80%{transform:translateX(2px)}30%,50%,70%{transform:translateX(-5px)}40%,60%{transform:translateX(5px)}}
  @media (prefers-reduced-motion:reduce){.otp.shake{animation:none}}
  /* Narrow phones: reclaim horizontal room so the 6 cells never overflow, and
     trim the card padding — mirrors style.css's screen-qualified breakpoints. */
  @media screen and (max-width:480px){
    /* The only horizontal gutter is 10vw each side, so the card spans 80vw —
       no stacked hard padding squeezing it. Drop the 520px cap so it fills. */
    #error{padding:1.5em 10vw}
    #error .prefs{max-width:none}
    .prefs{padding:1.25rem .6rem}
    /* Big cells: high max-width lets the 6 cells grow to fill the wider card,
       the 3/4 aspect-ratio scales their height to match. */
    .otp{gap:.4rem}
    .otp-cell{max-width:4rem;font-size:1.9rem;border-radius:6px}
  }
</style></head><body class="{{.BodyClass}}"><div id="error"><div class="prefs">
{{if .Err}}<div class="err">{{.Err}}</div>{{end}}
{{if eq .Stage "safe_env"}}
  <h1>Is this environment safe?</h1>
  <p class="muted">RedMemo is about to ask you for the server secret and a TOTP code. Only continue if no untrusted observer can see this screen or your inputs.</p>
  <form method="post" action="/settings">
    <input type="hidden" name="stage" value="safe_env">
    <button name="confirm" value="yes">Yes, environment is safe</button>
    <button name="confirm" value="no" class="alt">No</button>
  </form>
{{else if eq .Stage "server_secret"}}
  <h1>Server secret</h1>
  <p class="muted">Enter the secret configured via <code class="inline">REDMEMO_SERVER_SECRET</code>.{{if .HasTOTP}} TOTP is already enrolled — to rotate it, also enter the current 6-digit code.{{end}}</p>
  <form method="post" action="/settings" autocomplete="off">
    <input type="hidden" name="stage" value="server_secret">
    <input type="password" name="secret" autofocus required>
    {{if .HasTOTP}}<input type="text" class="otp-input" name="current_code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code" required placeholder="current 6-digit code">{{end}}
    <button>Continue</button>
  </form>
{{else if eq .Stage "enroll"}}
  <h1>Scan this QR — shown once</h1>
  <p class="muted">Add it to your authenticator (Google Authenticator, Aegis, 1Password…). Enter the current 6-digit code to finish enrollment. This QR will not be shown again.</p>
  <img class="qr" src="{{.QRDataURI}}" alt="TOTP QR">
  <p class="muted">Or import manually:</p>
  <code>{{.Secret}}</code>
  <form method="post" action="/settings" autocomplete="off" style="margin-top:1rem">
    <input type="hidden" name="stage" value="enroll_confirm">
    <input type="text" class="otp-input" data-autosubmit name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code" autofocus required placeholder="6-digit code">
    <label class="trust"><input type="checkbox" name="trust_device" value="on">Trust this device (skip the code on this browser)</label>
    <button>Verify &amp; enter settings</button>
  </form>
{{else if eq .Stage "totp"}}
  <h1>Authenticate</h1>
  <p class="muted">Enter the current 6-digit code from your authenticator. Three wrong codes lock this round.</p>
  <form method="post" action="/settings" autocomplete="off">
    <input type="hidden" name="stage" value="totp">
    <input type="text" class="otp-input" data-autosubmit name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autocomplete="one-time-code" autofocus required>
    <label class="trust"><input type="checkbox" name="trust_device" value="on">Trust this device (skip the code on this browser)</label>
    <button>Unlock settings</button>
  </form>
{{end}}
</div></div>
<script src="/otpInput.js" defer></script>
</body></html>`))

type authPageView struct {
	Stage           string
	Err             string
	Secret          string
	QRDataURI       template.URL
	Theme           string
	BodyClass       string
	HasTOTP         bool
	ThemeStylesheet bool
	AutoThemeCSS    template.HTML
}

// themeView fills in the theme-tracking fields so the auth gate's chrome (body
// classes, theme stylesheet link, auto-palette inline CSS) matches the user's
// current /settings preferences without depending on the templ layout.
func (h *Handler) themeView(r *http.Request, v *authPageView) {
	prefs := h.readPreferences(r)
	v.Theme = prefs.Theme
	// Mirror bodyClass() — only the theme name matters here (no layout/wide/
	// fixed_navbar on a centred single-card auth gate).
	if prefs.Theme != "" && prefs.Theme != "system" {
		v.BodyClass = prefs.Theme
	}
	v.ThemeStylesheet = render.ShowThemeStylesheet(prefs.Theme)
	if prefs.Theme == "auto" {
		v.AutoThemeCSS = template.HTML(render.AutoThemeStyle(prefs.AutoThemeDay, prefs.AutoThemeNight))
	}
}

func (h *Handler) renderAuthPage(w http.ResponseWriter, r *http.Request, stage authStage, errMsg string) {
	if stage == stageEnrollTOTP {
		// Re-display the one-time enrollment QR for an interrupted (persisted but
		// unconfirmed) enrollment so it stays recoverable. Fall back to the
		// server-secret form if the secret can't be read right now.
		if secret, err := h.auth.store.Secret(); err == nil && secret != "" {
			h.renderEnrollment(w, r, secret, errMsg)
			return
		}
		stage = stageServerSecret
	}
	v := authPageView{Err: errMsg}
	switch stage {
	case stageSafeEnv:
		v.Stage = "safe_env"
	case stageServerSecret:
		v.Stage = "server_secret"
		if s, _ := h.auth.store.Secret(); s != "" {
			v.HasTOTP = true
		}
	case stageTOTPCode:
		v.Stage = "totp"
	}
	h.themeView(r, &v)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := authPageTpl.Execute(w, v); err != nil {
		log.Printf("[auth] template: %v", err)
	}
}

func (h *Handler) renderEnrollment(w http.ResponseWriter, r *http.Request, secret, errMsg string) {
	dataURI, err := qrDataURI(secret)
	if err != nil {
		http.Error(w, "qr gen failed", http.StatusInternalServerError)
		return
	}
	// dataURI is a server-generated data:image/png URI. html/template's URL
	// filter only trusts http/https/mailto and would rewrite a plain-string
	// data: URI to "#ZgotmplZ", blanking the QR. template.URL marks it trusted.
	v := authPageView{Stage: "enroll", Secret: secret, QRDataURI: template.URL(dataURI), Err: errMsg}
	h.themeView(r, &v)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := authPageTpl.Execute(w, v); err != nil {
		log.Printf("[auth] template: %v", err)
	}
}
