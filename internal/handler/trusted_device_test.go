package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	ip        string
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

// Check mirrors store.Check: a live row is touched and reported TrustValid; an
// expired row is deleted on the spot and reported TrustExpired; a missing row
// is TrustAbsent.
func (f *fakeDeviceStore) Check(hash string) (store.TrustVerdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.rows {
		if r.hash != hash {
			continue
		}
		if r.expiresAt.After(time.Now()) {
			now := time.Now()
			r.lastUsed = &now
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

func (f *fakeDeviceStore) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

func newTestAuthWithDevices(f *fakeDeviceStore) *AuthManager {
	a := newTestAuth()
	a.devices = f
	return a
}

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
	if !a.HasValidTrustedDevice(req) {
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
	if a.HasValidTrustedDevice(req) {
		t.Fatal("revoked trusted device must be rejected immediately on the next request")
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
	a.tokens["sess123"] = time.Now().Add(time.Hour)
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
	_ = f.Create(hashToken(mine), mine[:8], "", time.Now().Add(trustedTokenTTL))
	_ = f.Create(hashToken(other), other[:8], "", time.Now().Add(trustedTokenTTL))
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
	if !a.HasValidTrustedDevice(trustedReq(mine)) {
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

	if a.HasValidTrustedDevice(trustedReq(dead)) {
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
	if err := f.Create(hashToken(good), good[:8], "", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed good: %v", err)
	}

	// trustMaxFailures rejected checks (an absent/forged cookie) trip the wire.
	for i := 0; i < trustMaxFailures; i++ {
		if a.HasValidTrustedDevice(trustedReq("forged-cookie-value")) {
			t.Fatal("a forged cookie must never validate")
		}
	}

	if !a.trustBanned() {
		t.Fatal("trust should be globally sealed after the failure ceiling")
	}
	if a.HasValidTrustedDevice(trustedReq(good)) {
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
	if err := f.Create(hashToken(good), good[:8], "", time.Now().Add(trustedTokenTTL)); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	for i := 0; i < trustMaxFailures; i++ {
		a.HasValidTrustedDevice(trustedReq("forged-cookie-value"))
	}
	// Force the ban window into the past, as if 30 minutes had elapsed.
	a.mu.Lock()
	a.trustBanUntil = time.Now().Add(-time.Second)
	a.trustFailWindowStart = time.Now().Add(-2 * trustFailWindow)
	a.mu.Unlock()

	if a.trustBanned() {
		t.Fatal("trust ban should have self-cleared once its window elapsed")
	}
	if !a.HasValidTrustedDevice(trustedReq(good)) {
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
