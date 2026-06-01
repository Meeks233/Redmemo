package handler

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
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
	defaultTokenTTL = 10 * time.Minute
	maxTokenTTL     = 60 * time.Minute
	maxAttempts     = 3
	lockoutWindow   = totp.Period * time.Second
	// safeEnvCookieTTL keeps the "this environment is safe" answer short-lived
	// (was 1 year). A day-long ack respects the user's intent without freezing
	// in a stale answer — if they later open /settings from a coffee shop,
	// they re-consent.
	safeEnvCookieTTL = 24 * time.Hour
)

type AuthManager struct {
	serverSecret string
	store        *store.TOTPStore

	mu        sync.Mutex
	tokens    map[string]time.Time // token -> expiry
	tries     map[string]*attempt  // ip   -> attempt state
	usedCodes map[string]time.Time // code  -> expiry; single-use enforcement
}

type attempt struct {
	count      int
	lockedUntil time.Time
}

func NewAuthManager(serverSecret string, s *store.TOTPStore) *AuthManager {
	return &AuthManager{
		serverSecret: serverSecret,
		store:        s,
		tokens:       make(map[string]time.Time),
		tries:        make(map[string]*attempt),
		usedCodes:    make(map[string]time.Time),
	}
}

// HasValidToken reports whether the request carries a still-live ephemeral
// auth cookie. Expired tokens are GC'd on the spot.
func (a *AuthManager) HasValidToken(r *http.Request) bool {
	c, err := r.Cookie(authTokenCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.tokens[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(a.tokens, c.Value)
		return false
	}
	return true
}

// issueToken mints a fresh ephemeral session cookie. ttl is clamped to
// (0, maxTokenTTL] — a zero/negative argument falls back to defaultTokenTTL so
// a misconfigured setting can never produce an immediately-expired cookie or
// outrun the gate's threat model.
func (a *AuthManager) issueToken(w http.ResponseWriter, r *http.Request, ttl time.Duration) {
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
	a.tokens[tok] = exp
	// opportunistic GC of expired tokens
	for k, v := range a.tokens {
		if time.Now().After(v) {
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
	v := h.siteDefaults["settings_token_ttl"]
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
// the IP until the next TOTP rotation and returns true (caller must redirect
// to /fuckreddit).
func (a *AuthManager) registerFailure(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.tries[ip]
	if st == nil {
		st = &attempt{}
		a.tries[ip] = st
	}
	st.count++
	if st.count >= maxAttempts {
		st.lockedUntil = time.Now().Add(lockoutWindow)
		st.count = 0
		return true
	}
	return false
}

// locked reports whether the IP is currently in the cool-down window.
func (a *AuthManager) locked(ip string) (bool, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
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
		if h.auth == nil { // safety: tests / misconfig — fail closed
			http.Error(w, "auth unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost && !isSameOriginPost(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if h.auth.HasValidToken(r) {
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
	if c, _ := r.Cookie(safeEnvCookie); c == nil || c.Value != "ok" {
		return stageSafeEnv
	}
	// stageServerSecret is implicit — the gate only advances past it when the
	// secret has been submitted in the same request. Stateless on purpose:
	// the server secret must be re-entered on every fresh round (no cookie).
	secret, _ := h.auth.store.Secret()
	if secret == "" {
		return stageServerSecret // first enrollment needs server-secret first
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
	r.ParseForm()
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
			if locked := h.auth.registerFailure(ip); locked {
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
		if existing, _ := h.auth.store.Secret(); existing != "" {
			code := strings.TrimSpace(r.FormValue("current_code"))
			if code == "" {
				h.renderAuthPage(w, r, stageServerSecret, "TOTP is already enrolled — also enter the current 6-digit code to rotate it, or run `redmemo --reset-totp` first")
				return
			}
			ok, replay := h.auth.consumeCode(existing, code, time.Now())
			if !ok || replay {
				if locked := h.auth.registerFailure(ip); locked {
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
			if locked := h.auth.registerFailure(ip); locked {
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
		h.auth.issueToken(w, r, h.resolveTokenTTL())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)

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
			if locked := h.auth.registerFailure(ip); locked {
				http.Redirect(w, r, "/fuckreddit?reason=auth_locked", http.StatusSeeOther)
				return
			}
			h.renderAuthPage(w, r, stageTOTPCode, "incorrect code")
			return
		}
		if replay {
			http.Redirect(w, r, "/fuckreddit?reason=totp_replay", http.StatusSeeOther)
			return
		}
		h.auth.resetAttempts(ip)
		h.auth.issueToken(w, r, h.resolveTokenTTL())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)

	default:
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
	}
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
  body{display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;font-family:system-ui,sans-serif;background:var(--background);color:var(--text)}
  .card{max-width:480px;width:90%;padding:2rem;background:var(--post);border:var(--panel-border);border-radius:8px;box-shadow:var(--shadow)}
  h1{margin-top:0;font-size:1.2rem;color:var(--text)}
  input[type=text],input[type=password]{width:100%;padding:.6rem;margin:.5rem 0 1rem;background:var(--outside);color:var(--text);border:var(--panel-border);border-radius:4px;font-family:monospace;font-size:1rem;box-sizing:border-box}
  input[type=text]:focus,input[type=password]:focus{outline:2px solid var(--accent);outline-offset:1px}
  button{padding:.6rem 1.2rem;background:var(--accent);color:var(--foreground);border:0;border-radius:4px;cursor:pointer;font-weight:bold}
  button.alt{background:var(--highlighted);color:var(--text)}
  .err{color:var(--nsfw);margin:.5rem 0}
  .muted{color:var(--visited);font-size:.85rem}
  img.qr{display:block;margin:1rem auto;background:#fff;padding:.5rem;border-radius:4px}
  code{display:block;padding:.5rem;background:var(--outside);color:var(--text);border-radius:4px;font-family:monospace;word-break:break-all}
  p{color:var(--text)}
</style></head><body class="{{.BodyClass}}"><div class="card">
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
  <p class="muted">Enter the secret configured via <code>REDMEMO_SERVER_SECRET</code>.{{if .HasTOTP}} TOTP is already enrolled — to rotate it, also enter the current 6-digit code.{{end}}</p>
  <form method="post" action="/settings" autocomplete="off">
    <input type="hidden" name="stage" value="server_secret">
    <input type="password" name="secret" autofocus required>
    {{if .HasTOTP}}<input type="text" name="current_code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" required placeholder="current 6-digit code">{{end}}
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
    <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autofocus required placeholder="6-digit code">
    <button>Verify &amp; enter settings</button>
  </form>
{{else if eq .Stage "totp"}}
  <h1>Authenticate</h1>
  <p class="muted">Enter the current 6-digit code from your authenticator. Three wrong codes lock this round.</p>
  <form method="post" action="/settings" autocomplete="off">
    <input type="hidden" name="stage" value="totp">
    <input type="text" name="code" inputmode="numeric" pattern="[0-9]{6}" maxlength="6" autofocus required>
    <button>Unlock settings</button>
  </form>
{{end}}
</div></body></html>`))

type authPageView struct {
	Stage           string
	Err             string
	Secret          string
	QRDataURI       string
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
	v := authPageView{Stage: "enroll", Secret: secret, QRDataURI: dataURI, Err: errMsg}
	h.themeView(r, &v)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := authPageTpl.Execute(w, v); err != nil {
		log.Printf("[auth] template: %v", err)
	}
}
