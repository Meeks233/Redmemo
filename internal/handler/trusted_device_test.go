package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redmemo/redmemo/internal/store"
)

// fakeDeviceStore is an in-memory stand-in for *store.TrustedDeviceStore that
// mirrors the SQL semantics (every "active" check compares expires_at against
// time.Now(); Check touches a live row or deletes an expired one; Revoke deletes
// by id; DeleteExpired drops past-expiry rows). It is mutex-guarded so the
// background sweeper goroutine and the test goroutine can touch it concurrently.
type fakeDeviceStore struct {
	mu     sync.Mutex
	rows   []*fakeDeviceRow
	nextID int64
}

type fakeDeviceRow struct {
	id        int64
	hash      string
	prefix    string
	ip        string // primary (mint-time) binding
	ipAlt     string // complementary-family binding, lazily learned (see Check)
	createdAt time.Time
	lastUsed  *time.Time
	expiresAt time.Time
}

func (f *fakeDeviceStore) CountActive() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.rows {
		if r.expiresAt.After(time.Now()) {
			n++
		}
	}
	return n, nil
}

func (f *fakeDeviceStore) Create(hash, prefix, ip string, expiresAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.rows = append(f.rows, &fakeDeviceRow{
		id: f.nextID, hash: hash, prefix: prefix, ip: ip,
		createdAt: time.Now(), expiresAt: expiresAt,
	})
	return nil
}

// Check mirrors store.Check, including the dual-stack OR binding: a live row
// whose ip OR ip_alt matches clientIP is touched and reported TrustValid; a live
// row whose ip_alt slot is still empty learns clientIP into it IFF clientIP is of
// the OTHER family than ip (then validates); an expired row is deleted on the
// spot (IP-agnostic, pure hygiene) and reported TrustExpired; a missing row — or
// a live row presented from an unbound, non-learnable IP — is TrustAbsent.
func (f *fakeDeviceStore) Check(hash, clientIP string, newExpiry time.Time) (store.TrustVerdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	clientIsV6 := strings.Contains(clientIP, ":")
	for i, r := range f.rows {
		if r.hash != hash {
			continue
		}
		if r.expiresAt.After(time.Now()) {
			matches := r.ip == clientIP || (r.ipAlt != "" && r.ipAlt == clientIP)
			// Learn only into an empty alt slot, and only the complementary family
			// (mirrors `(position(':' in ip) > 0) <> $4` in the store CTE).
			canLearn := r.ipAlt == "" && r.ip != "" && (strings.Contains(r.ip, ":") != clientIsV6)
			if !matches && !canLearn {
				return store.TrustAbsent, nil
			}
			if !matches {
				r.ipAlt = clientIP
			}
			now := time.Now()
			r.lastUsed = &now
			r.expiresAt = newExpiry // sliding-window renewal, mirrors the store CTE
			return store.TrustValid, nil
		}
		f.rows = append(f.rows[:i], f.rows[i+1:]...)
		return store.TrustExpired, nil
	}
	return store.TrustAbsent, nil
}

func (f *fakeDeviceStore) ListActive() ([]store.TrustedDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.TrustedDevice
	for _, r := range f.rows {
		if r.expiresAt.After(time.Now()) {
			out = append(out, store.TrustedDevice{
				ID: r.id, TokenPrefix: r.prefix, IP: r.ip,
				CreatedAt: r.createdAt, LastUsed: r.lastUsed, ExpiresAt: r.expiresAt,
			})
		}
	}
	return out, nil
}

func (f *fakeDeviceStore) HashByID(id int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			return r.hash, nil
		}
	}
	return "", nil
}

func (f *fakeDeviceStore) Revoke(id int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows {
		if r.id == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			return 1, nil
		}
	}
	return 0, nil
}

func (f *fakeDeviceStore) DeleteExpired() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kept []*fakeDeviceRow
	var removed int64
	for _, r := range f.rows {
		if r.expiresAt.After(time.Now()) {
			kept = append(kept, r)
		} else {
			removed++
		}
	}
	f.rows = kept
	return removed, nil
}

func (f *fakeDeviceStore) DeleteAll() (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	removed := int64(len(f.rows))
	f.rows = nil
	return removed, nil
}

func (f *fakeDeviceStore) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// altIPOf returns the learned complementary-family binding for the row carrying
// hash (or "" if there is none / no such row) — lets the dual-stack tests assert
// exactly which sibling address got learned.
func (f *fakeDeviceStore) altIPOf(hash string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.hash == hash {
			return r.ipAlt
		}
	}
	return ""
}

func newTestAuthWithDevices(f *fakeDeviceStore) *AuthManager {
	a := newTestAuth()
	a.devices = f
	return a
}

// boundIP is the client IP that trusted-device rows in these tests are minted
// for; HasValidTrustedDevice is called with the same IP so the binding matches.
const boundIP = "192.0.2.5"

func trustedReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(&http.Cookie{Name: trustedCookie, Value: token})
	return r
}

func issuePost() (*httptest.ResponseRecorder, *http.Request) {
	return httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/settings", nil)
}

// TestTrustedDeviceRevokedImmediatelyRejected proves a revoked long token stops
// authorising /settings on the very next request — the gate re-queries the table
// every call, so there is no caching or grace window to outlive a revoke.
func TestTrustedDeviceRevokedImmediatelyRejected(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "deadbeefcafef00d0123456789abcdef"
	if err := f.Create(hashToken(tok), tok[:8], "192.0.2.5", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	req := trustedReq(tok)
	if !a.HasValidTrustedDevice(req, boundIP) {
		t.Fatal("freshly created trusted device should validate")
	}

	list, err := a.ListTrustedDevices()
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 device listed, got %d", len(list))
	}

	if err := a.RevokeTrustedDevice(list[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if a.HasValidTrustedDevice(req, boundIP) {
		t.Fatal("revoked trusted device must be rejected immediately on the next request")
	}
}

// TestRevokeAllTrustedDevicesOnTOTPReset proves that wiping the second factor
// (the --reset-totp CLI path / a TOTP rotation) de-authorises EVERY trusted
// device. A 365-day trust cookie minted under the old secret must not survive a
// reset the operator performs precisely because they suspect compromise.
func TestRevokeAllTrustedDevicesOnTOTPReset(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	toks := []string{
		"deadbeefcafef00d0123456789abcdef",
		"feedface0123456789abcdeffeedface",
	}
	for _, tok := range toks {
		if err := f.Create(hashToken(tok), tok[:8], "192.0.2.5", time.Now().Add(trustedTokenTTL)); err != nil {
			t.Fatalf("seed device: %v", err)
		}
	}
	for _, tok := range toks {
		if !a.HasValidTrustedDevice(trustedReq(tok), boundIP) {
			t.Fatal("seeded trusted device should validate before reset")
		}
	}

	n, err := a.RevokeAllTrustedDevices()
	if err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if n != int64(len(toks)) {
		t.Fatalf("want %d devices revoked, got %d", len(toks), n)
	}
	for _, tok := range toks {
		if a.HasValidTrustedDevice(trustedReq(tok), boundIP) {
			t.Fatal("every trusted device must be rejected after a TOTP reset")
		}
	}
	if f.len() != 0 {
		t.Fatalf("store should be empty after revoke-all, has %d rows", f.len())
	}
}

// TestRevokeAllTrustedDevicesNilStore guards the no-store path (tests / a
// misconfig with no trusted-device backing): revoke-all must be a clean no-op,
// never a nil dereference.
func TestRevokeAllTrustedDevicesNilStore(t *testing.T) {
	a := newTestAuth() // no devices wired
	n, err := a.RevokeAllTrustedDevices()
	if err != nil || n != 0 {
		t.Fatalf("nil store revoke-all: want (0,nil), got (%d,%v)", n, err)
	}
}

// TestSessionTokenBoundToIP proves the ephemeral /settings session cookie only
// authorises requests from the IP it was minted for: the same cookie replayed
// from a different address is rejected (theft / exfiltration), while the
// original address keeps working until the token expires.
func TestSessionTokenBoundToIP(t *testing.T) {
	a := newTestAuth()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", nil)
	a.issueToken(w, r, 10*time.Minute, "192.0.2.10")

	var tok string
	for _, c := range w.Result().Cookies() {
		if c.Name == authTokenCookie {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("issueToken should set the session cookie")
	}

	sameIP := httptest.NewRequest(http.MethodGet, "/settings", nil)
	sameIP.AddCookie(&http.Cookie{Name: authTokenCookie, Value: tok})
	if !a.HasValidToken(sameIP, "192.0.2.10") {
		t.Fatal("token must validate from the IP it was issued to")
	}

	otherIP := httptest.NewRequest(http.MethodGet, "/settings", nil)
	otherIP.AddCookie(&http.Cookie{Name: authTokenCookie, Value: tok})
	if a.HasValidToken(otherIP, "203.0.113.7") {
		t.Fatal("token replayed from a different IP must be rejected")
	}
	// Rejection must NOT delete the row — the legitimate IP can still use it.
	if !a.HasValidToken(sameIP, "192.0.2.10") {
		t.Fatal("a cross-IP rejection must not invalidate the token for its own IP")
	}
}

// TestTrustedDeviceSlidingRenewal proves the trusted-device window slides: a
// near-expiry token that gets validated has its expiry pushed forward, so a
// browser in regular use never lapses.
func TestTrustedDeviceSlidingRenewal(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "0011223344556677889900aabbccddee"
	// Seed a row expiring in 1 minute — well inside its window but close to lapse.
	if err := f.Create(hashToken(tok), tok[:8], "192.0.2.5", time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	if !a.HasValidTrustedDevice(trustedReq(tok), boundIP) {
		t.Fatal("near-expiry trusted device should still validate")
	}

	// After one validation the stored expiry should have jumped to ~now+TTL.
	list, err := a.ListTrustedDevices()
	if err != nil || len(list) != 1 {
		t.Fatalf("list devices: err=%v len=%d", err, len(list))
	}
	if got := time.Until(list[0].ExpiresAt); got < trustedTokenTTL-time.Hour {
		t.Fatalf("validation should have slid expiry to ~%s out, got %s", trustedTokenTTL, got)
	}
}

// TestTrustedDeviceBoundToIP proves the 30-day trusted cookie only authorises
// requests from the IP it was minted for: a live, valid cookie replayed from a
// different address is rejected (theft / exfiltration), mirroring the IP binding
// on the ephemeral session token. The legitimate IP keeps working, and the
// cross-IP rejection counts toward the abuse tripwire.
func TestTrustedDeviceBoundToIP(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "cafef00dcafef00dcafef00dcafef00d"
	if err := f.Create(hashToken(tok), tok[:8], boundIP, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// Same IP it was minted for: honoured.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundIP) {
		t.Fatal("trusted cookie must validate from the IP it was minted for")
	}
	// A stolen cookie replayed from another network: rejected.
	if a.HasValidTrustedDevice(trustedReq(tok), "203.0.113.99") {
		t.Fatal("trusted cookie replayed from a different IP must be rejected")
	}
	// The cross-IP rejection must NOT have deleted the row — the legitimate IP
	// can still use it.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundIP) {
		t.Fatal("a cross-IP rejection must not invalidate the cookie for its own IP")
	}
}

// testHandlerWithAuth builds a minimal Handler wired to the given AuthManager
// for exercising the auth-gated HTTP handlers directly (bypassing the
// requireSettingsAuth middleware, which the unit under test runs behind).
func testHandlerWithAuth(a *AuthManager) *Handler {
	return &Handler{siteDefaults: make(map[string]string), auth: a}
}

// TestRedirectAfterUnlockTrustSkipsSessionToken proves goal 1: a successful
// unlock that trusts the device mints ONLY the long trusted cookie — no
// redundant ephemeral session token is issued (the trusted cookie already
// authorises every future request, so a second credential is wasted state).
func TestRedirectAfterUnlockTrustSkipsSessionToken(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)
	h := testHandlerWithAuth(a)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", nil)
	r.Form = map[string][]string{"trust_device": {"on"}}
	h.redirectAfterUnlock(w, r, "192.0.2.9")

	var sawTrusted, sawSession bool
	for _, c := range w.Result().Cookies() {
		switch c.Name {
		case trustedCookie:
			if c.Value != "" {
				sawTrusted = true
			}
		case authTokenCookie:
			if c.Value != "" {
				sawSession = true
			}
		}
	}
	if !sawTrusted {
		t.Fatal("trusting the device should set the trusted cookie")
	}
	if sawSession {
		t.Fatal("a trusted device must NOT also receive an ephemeral session token")
	}
	if n := len(a.tokens); n != 0 {
		t.Fatalf("no session token should be tracked for a trusted device, got %d", n)
	}
}

// TestRedirectAfterUnlockNoTrustIssuesSession proves the fallback: when trust is
// not requested, the short-lived session token is still minted as before.
func TestRedirectAfterUnlockNoTrustIssuesSession(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)
	h := testHandlerWithAuth(a)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", nil)
	r.Form = map[string][]string{}
	h.redirectAfterUnlock(w, r, "192.0.2.9")

	var sawSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == authTokenCookie && c.Value != "" {
			sawSession = true
		}
	}
	if !sawSession {
		t.Fatal("an untrusted unlock should still mint a session token")
	}
	if f.len() != 0 {
		t.Fatal("no trusted device should be created without trust_device=on")
	}
}

// TestSelfRevokeDropsAccessAndRedirectsHome proves goal 2: when a trusted
// browser revokes its OWN device row, handleTrustedRevoke tears down that
// browser's credentials (session token + both cookies) and redirects to the
// home page rather than the settings gate.
func TestSelfRevokeDropsAccessAndRedirectsHome(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)
	h := testHandlerWithAuth(a)

	const tok = "abc123abc123abc123abc123abc12345"
	if err := f.Create(hashToken(tok), tok[:8], "192.0.2.7", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	list, _ := a.ListTrustedDevices()
	id := list[0].ID

	// The revoking request carries this device's own trusted cookie plus a stray
	// live session token — both must be cleared on self-revoke.
	a.tokens["sess123"] = sessionToken{exp: time.Now().Add(time.Hour), ip: "192.0.2.7"}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/trusted/revoke", nil)
	r.AddCookie(&http.Cookie{Name: trustedCookie, Value: tok})
	r.AddCookie(&http.Cookie{Name: authTokenCookie, Value: "sess123"})
	r.Form = map[string][]string{"id": {strconv.FormatInt(id, 10)}}

	h.handleTrustedRevoke(w, r)

	if got := w.Result().StatusCode; got != http.StatusSeeOther {
		t.Fatalf("want 303 redirect, got %d", got)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("self-revoke should redirect to home, got %q", loc)
	}
	if _, ok := a.tokens["sess123"]; ok {
		t.Fatal("self-revoke must drop the in-memory session token")
	}
	var clearedTrust, clearedSession bool
	for _, c := range w.Result().Cookies() {
		if c.Name == trustedCookie && c.MaxAge < 0 {
			clearedTrust = true
		}
		if c.Name == authTokenCookie && c.MaxAge < 0 {
			clearedSession = true
		}
	}
	if !clearedTrust || !clearedSession {
		t.Fatalf("both cookies should be cleared (trust=%v session=%v)", clearedTrust, clearedSession)
	}
	if f.len() != 0 {
		t.Fatal("the revoked row should be gone")
	}
}

// TestRevokeOtherDeviceKeepsAccess proves the converse: revoking a DIFFERENT
// device leaves the caller's own session intact and returns to /settings.
func TestRevokeOtherDeviceKeepsAccess(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)
	h := testHandlerWithAuth(a)

	const mine = "1111111111111111aaaaaaaaaaaaaaaa"
	const other = "2222222222222222bbbbbbbbbbbbbbbb"
	_ = f.Create(hashToken(mine), mine[:8], boundIP, time.Now().Add(trustedTokenTTL))
	_ = f.Create(hashToken(other), other[:8], boundIP, time.Now().Add(trustedTokenTTL))
	list, _ := a.ListTrustedDevices()
	var otherID int64
	for _, d := range list {
		if d.TokenPrefix == other[:8] {
			otherID = d.ID
		}
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/trusted/revoke", nil)
	r.AddCookie(&http.Cookie{Name: trustedCookie, Value: mine})
	r.Form = map[string][]string{"id": {strconv.FormatInt(otherID, 10)}}

	h.handleTrustedRevoke(w, r)

	if loc := w.Header().Get("Location"); loc != "/settings" {
		t.Fatalf("revoking another device should stay in settings, got %q", loc)
	}
	if !a.HasValidTrustedDevice(trustedReq(mine), boundIP) {
		t.Fatal("the caller's own trusted device must still validate")
	}
}

// TestTrustedDeviceExpiredCleanedOnAccess proves the passive lazy-cleanup path:
// presenting an expired cookie is rejected AND the dead row is reaped in the
// same request (TrustExpired), so an invalid-but-uncleaned token does not linger.
func TestTrustedDeviceExpiredCleanedOnAccess(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const dead = "11111111ddddddddeeeeeeeeffffffff"
	if err := f.Create(hashToken(dead), dead[:8], "", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("seed dead: %v", err)
	}

	if a.HasValidTrustedDevice(trustedReq(dead), boundIP) {
		t.Fatal("expired token must be rejected")
	}
	if got := f.len(); got != 0 {
		t.Fatalf("expired row should be cleaned on access, %d row(s) remain", got)
	}
}

// TestTrustedDeviceCapEnforced verifies issueTrustedDevice mints up to
// maxTrustedDevices long tokens while every slot is held by a live token, then
// drops further requests without adding a row (the grace-warning path).
func TestTrustedDeviceCapEnforced(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	for i := 0; i < maxTrustedDevices; i++ {
		rec, req := issuePost()
		if !a.issueTrustedDevice(rec, req, "192.0.2.9") {
			t.Fatalf("issue #%d should succeed under the cap", i+1)
		}
	}

	rec, req := issuePost()
	if a.issueTrustedDevice(rec, req, "192.0.2.9") {
		t.Fatal("issuing a device while all slots hold live tokens must be refused")
	}
	if n := f.len(); n != maxTrustedDevices {
		t.Fatalf("want exactly %d rows at the cap, got %d", maxTrustedDevices, n)
	}
}

// TestTrustedDeviceIssueReclaimsExpiredSlot covers the slot-reclaim path: with
// the cap reached but some slots expired, a new request batch-reaps the expired
// rows and takes a freed slot — net device count stays at the cap.
func TestTrustedDeviceIssueReclaimsExpiredSlot(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	// Two expired + one live = 3 rows, but only 1 LIVE slot occupied.
	if err := f.Create(hashToken("dead-a"), "dead-a00", "", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("seed dead-a: %v", err)
	}
	if err := f.Create(hashToken("dead-b"), "dead-b00", "", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("seed dead-b: %v", err)
	}
	if err := f.Create(hashToken("live-c"), "live-c00", "", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed live-c: %v", err)
	}

	rec, req := issuePost()
	if !a.issueTrustedDevice(rec, req, "192.0.2.20") {
		t.Fatal("a free/expired slot should accept a new trusted device")
	}
	// Expired rows reaped, the live one kept, the new one inserted → 2 live rows.
	active, err := f.CountActive()
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 2 {
		t.Fatalf("want 2 live devices after reclaim, got %d", active)
	}
	if total := f.len(); total != 2 {
		t.Fatalf("expired rows should be gone; want 2 total rows, got %d", total)
	}
}

// TestTrustTripwireSealsAfterThreeFailures exercises the 30-min/3-strike global
// tripwire: three rejected cookie checks seal trust instance-wide, after which
// even a genuinely valid cookie is refused and no new trust can be issued.
func TestTrustTripwireSealsAfterThreeFailures(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const good = "900dbeef0123456789abcdef0a1b2c3d"
	if err := f.Create(hashToken(good), good[:8], boundIP, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed good: %v", err)
	}

	// trustMaxFailures rejected checks (an absent/forged cookie) trip the wire.
	for i := 0; i < trustMaxFailures; i++ {
		if a.HasValidTrustedDevice(trustedReq("forged-cookie-value"), boundIP) {
			t.Fatal("a forged cookie must never validate")
		}
	}

	if !a.trustBanned() {
		t.Fatal("trust should be globally sealed after the failure ceiling")
	}
	if a.HasValidTrustedDevice(trustedReq(good), boundIP) {
		t.Fatal("once the tripwire trips, even a valid trust cookie must be refused")
	}
	rec, req := issuePost()
	if a.issueTrustedDevice(rec, req, "192.0.2.30") {
		t.Fatal("once the tripwire trips, new trust requests must be refused")
	}
}

// TestTrustTripwireSelfClears confirms the global seal is not permanent: once
// the ban window has elapsed, a valid cookie is honoured again.
func TestTrustTripwireSelfClears(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const good = "abadcafe0123456789abcdef0a1b2c3d"
	if err := f.Create(hashToken(good), good[:8], boundIP, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	for i := 0; i < trustMaxFailures; i++ {
		a.HasValidTrustedDevice(trustedReq("forged-cookie-value"), boundIP)
	}
	// Force the ban window into the past, as if 30 minutes had elapsed.
	a.mu.Lock()
	a.trustBanUntil = time.Now().Add(-time.Second)
	a.trustFailWindowStart = time.Now().Add(-2 * trustFailWindow)
	a.mu.Unlock()

	if a.trustBanned() {
		t.Fatal("trust ban should have self-cleared once its window elapsed")
	}
	if !a.HasValidTrustedDevice(trustedReq(good), boundIP) {
		t.Fatal("a valid cookie must be honoured again after the ban window clears")
	}
}

// TestShouldShowTrustLimit pins the grace-banner decision and, in particular,
// the reported contradiction bug: the sticky ?trusted=limit marker must NOT show
// the "you already have 3 trusted devices" banner when the list is empty or
// below the cap. The banner may appear only when the live device count is
// genuinely at/above maxTrustedDevices — exactly when issuance would be refused.
func TestShouldShowTrustLimit(t *testing.T) {
	cases := []struct {
		name   string
		marker string
		count  int
		want   bool
	}{
		// The exact reported bug: marker sticky, zero devices → must NOT show.
		{"marker but no devices (the bug)", "limit", 0, false},
		{"marker but one device", "limit", 1, false},
		{"marker just below cap", "limit", maxTrustedDevices - 1, false},
		{"marker exactly at cap", "limit", maxTrustedDevices, true},
		{"marker above cap (defensive)", "limit", maxTrustedDevices + 1, true},
		{"no marker, at cap", "", maxTrustedDevices, false},
		{"empty marker, zero devices", "", 0, false},
		{"unrelated marker, at cap", "ok", maxTrustedDevices, false},
		{"unrelated marker, below cap", "revoked", 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldShowTrustLimit(c.marker, c.count); got != c.want {
				t.Errorf("shouldShowTrustLimit(%q, %d) = %v, want %v", c.marker, c.count, got, c.want)
			}
		})
	}
}

// TestTrustedSweeperRunsCleanup exercises the wiring: StartTrustedSweeper fires
// the periodic cleanup once synchronously on start, so a pre-seeded expired row
// is gone shortly after the goroutine launches while the live row remains.
func TestTrustedSweeperRunsCleanup(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)
	if err := f.Create(hashToken("expiredtok"), "expired0", "", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("seed expired: %v", err)
	}
	if err := f.Create(hashToken("livetok000"), "livetok0", "", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed live: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartTrustedSweeper(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for f.len() > 1 {
		if time.Now().After(deadline) {
			t.Fatalf("sweeper did not reap the expired row in time, len=%d", f.len())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.len() != 1 {
		t.Fatalf("want 1 live row after the sweep, got %d", f.len())
	}
}

// boundV4 / boundV6 are the IPv4 and IPv6 addresses of one notional dual-stack
// client used by the OR-binding tests. boundV4 == boundIP so the existing
// single-stack tests keep their meaning.
const (
	boundV4 = boundIP        // "192.0.2.5"
	boundV6 = "2001:db8::5"  // same physical client, sibling family
)

// TestTrustedDeviceDualStackOR is the headline fix: a trusted cookie minted on a
// dual-stack client's IPv4 address must keep validating when the SAME cookie is
// later presented over the client's IPv6 address (and vice-versa), without ever
// minting a second trusted-device row. The sibling-family address is learned
// once into ip_alt, after which BOTH families validate (OR).
func TestTrustedDeviceDualStackOR(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "0a0a0a0a0b0b0b0b0c0c0c0c0d0d0d0d"
	// Minted over IPv4 — ip set, ip_alt still empty.
	if err := f.Create(hashToken(tok), tok[:8], boundV4, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// Same family it was minted for: honoured, nothing learned yet.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV4) {
		t.Fatal("trusted cookie must validate from its mint-time IPv4 address")
	}
	if alt := f.altIPOf(hashToken(tok)); alt != "" {
		t.Fatalf("a same-family presentation must not learn an alt IP, got %q", alt)
	}

	// First IPv6 presentation of the SAME cookie: recognised as the same device,
	// learns the sibling address rather than rejecting.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV6) {
		t.Fatal("trusted cookie must validate from the sibling IPv6 address (OR binding)")
	}
	if alt := f.altIPOf(hashToken(tok)); alt != boundV6 {
		t.Fatalf("the IPv6 sibling should be learned into ip_alt, got %q", alt)
	}

	// Both families now validate, and the OR binding never created a second row —
	// the whole point: one physical device, one slot.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV4) {
		t.Fatal("IPv4 must still validate after the IPv6 sibling is learned")
	}
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV6) {
		t.Fatal("IPv6 must keep validating once learned")
	}
	if n := f.len(); n != 1 {
		t.Fatalf("dual-stack OR binding must use exactly ONE slot, got %d rows", n)
	}
}

// TestTrustedDeviceSameFamilyRotationNotLearned proves the learn is family-scoped:
// a DIFFERENT same-family address (e.g. an IPv6 prefix rotation, or a same-network
// thief) is NOT absorbed into ip_alt — it is rejected, exactly as before, so the
// single sibling slot is reserved for the genuine other-family address.
func TestTrustedDeviceSameFamilyRotationNotLearned(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "1212121234343434565656567878787a"
	// Minted over IPv6.
	if err := f.Create(hashToken(tok), tok[:8], boundV6, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	// A different IPv6 (same family) must be refused and must NOT be learned.
	if a.HasValidTrustedDevice(trustedReq(tok), "2001:db8::99") {
		t.Fatal("a different same-family address must be rejected, not learned")
	}
	if alt := f.altIPOf(hashToken(tok)); alt != "" {
		t.Fatalf("a same-family mismatch must leave ip_alt empty, got %q", alt)
	}
	// The genuine IPv4 sibling can still take the (still-empty) alt slot.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV4) {
		t.Fatal("the IPv4 sibling should still be learnable into the reserved alt slot")
	}
	if alt := f.altIPOf(hashToken(tok)); alt != boundV4 {
		t.Fatalf("the IPv4 sibling should occupy ip_alt, got %q", alt)
	}
}

// TestTrustedDeviceAltSlotBounded proves the device is bound to at most TWO
// addresses ever: once ip + ip_alt are both filled (one per family), a third,
// distinct address — even of a not-yet-bound family pattern — is rejected. This
// caps the blast radius of the OR relaxation.
func TestTrustedDeviceAltSlotBounded(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "9898989876767676545454543232321a"
	if err := f.Create(hashToken(tok), tok[:8], boundV4, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	// Fill the alt slot with the IPv6 sibling.
	if !a.HasValidTrustedDevice(trustedReq(tok), boundV6) {
		t.Fatal("IPv6 sibling should be learned")
	}
	// A THIRD distinct IPv6 address now finds the alt slot occupied → rejected.
	if a.HasValidTrustedDevice(trustedReq(tok), "2001:db8::dead") {
		t.Fatal("a third distinct address must be rejected once both slots are bound")
	}
	if alt := f.altIPOf(hashToken(tok)); alt != boundV6 {
		t.Fatalf("the alt slot must stay pinned to the first learned sibling, got %q", alt)
	}
}

// TestTrustedDeviceConcurrentDualStackSingleLearn is the race-condition test for
// the OR-binding learn. A burst of concurrent requests for the SAME cookie — a
// mix of the IPv4 (already bound) and IPv6 (to-be-learned) addresses, exactly
// what a browser firing parallel requests over a dual-stack link produces — must:
//   - never panic / corrupt state (run under `go test -race`);
//   - all succeed (every request is from a bound or learnable address);
//   - converge on EXACTLY ONE learned sibling (no double-learn), and never spawn
//     a second device row.
// The store's single-statement CTE makes the real DB path atomic; the fake mirror
// is mutex-guarded, so this exercises the AuthManager-level concurrency and pins
// the invariant.
func TestTrustedDeviceConcurrentDualStackSingleLearn(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "feedfacefeedfacefeedfacefeedface"
	if err := f.Create(hashToken(tok), tok[:8], boundV4, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	const goroutines = 64
	var wg sync.WaitGroup
	var okCount int64
	var okMu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		ip := boundV4
		if i%2 == 0 {
			ip = boundV6 // half present the sibling family that must be learned
		}
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			<-start // line everyone up so the presentations actually overlap
			if a.HasValidTrustedDevice(trustedReq(tok), ip) {
				okMu.Lock()
				okCount++
				okMu.Unlock()
			}
		}(ip)
	}
	close(start)
	wg.Wait()

	if okCount != goroutines {
		t.Fatalf("every concurrent presentation from a bound/learnable address must validate, got %d/%d", okCount, goroutines)
	}
	if alt := f.altIPOf(hashToken(tok)); alt != boundV6 {
		t.Fatalf("concurrent learns must converge on the single sibling, got ip_alt=%q", alt)
	}
	if n := f.len(); n != 1 {
		t.Fatalf("concurrent dual-stack validation must never create a second slot, got %d rows", n)
	}
}

// TestTrustedDeviceConcurrentDistinctIPsBounded hardens the bound under a race:
// many goroutines present the same valid cookie from DISTINCT addresses of the
// sibling family at once. At most ONE may win the empty alt slot; the device must
// still be capped at two bindings and one row, and the winner's address must be
// the one pinned in ip_alt for everyone who then matches it.
func TestTrustedDeviceConcurrentDistinctIPsBounded(t *testing.T) {
	f := &fakeDeviceStore{}
	a := newTestAuthWithDevices(f)

	const tok = "0123456789abcdef0123456789abcdef"
	if err := f.Create(hashToken(tok), tok[:8], boundV4, time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	const goroutines = 48
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		// Each goroutine uses a DISTINCT IPv6 address (the sibling family).
		ip := "2001:db8::" + strconv.Itoa(100+i)
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			<-start
			a.HasValidTrustedDevice(trustedReq(tok), ip)
		}(ip)
	}
	close(start)
	wg.Wait()

	// Whatever raced through, the device is bound to at most two addresses (ip +
	// one learned alt) and still a single row — the alt slot can never hold more
	// than one sibling.
	if alt := f.altIPOf(hashToken(tok)); !strings.HasPrefix(alt, "2001:db8::") {
		t.Fatalf("exactly one distinct sibling should win the alt slot, got %q", alt)
	}
	if n := f.len(); n != 1 {
		t.Fatalf("racing distinct siblings must not spawn extra rows, got %d", n)
	}
	winner := f.altIPOf(hashToken(tok))

	// The ~47 losing distinct-IP presentations are genuine rejections, so they
	// legitimately tripped the 30-min/3-strike abuse tripwire and sealed trust
	// instance-wide (correct behaviour — a burst of distinct cookie sources reads
	// as an attack). Clear that seal so the final binding-cap assertions below
	// observe the row state, not the ban.
	a.mu.Lock()
	a.trustBanUntil = time.Time{}
	a.trustFailCount = 0
	a.trustFailWindowStart = time.Time{}
	a.mu.Unlock()

	// The pinned winner keeps validating; a DIFFERENT sibling is now rejected
	// (alt slot full) — proving the two-address cap held through the race.
	if !a.HasValidTrustedDevice(trustedReq(tok), winner) {
		t.Fatalf("the learned sibling %q must keep validating", winner)
	}
	loser := "2001:db8::ffff"
	if a.HasValidTrustedDevice(trustedReq(tok), loser) {
		t.Fatal("a non-winning distinct sibling must be rejected once the alt slot is taken")
	}
}
