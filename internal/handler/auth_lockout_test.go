package handler

import (
	"fmt"
	"testing"
	"time"
)

// newTestAuth builds an AuthManager with just the in-memory lockout maps wired
// up. registerFailure/locked never touch the store, so a nil store is fine here.
func newTestAuth() *AuthManager {
	return &AuthManager{
		tokens:    make(map[string]time.Time),
		tries:     make(map[string]*attempt),
		usedCodes: make(map[string]time.Time),
	}
}

// TestGlobalCeilingLocksAcrossDistinctIPs verifies the instance-wide backstop:
// failures spread across many distinct source IPs (none of which individually
// reach maxAttempts) still trip the global ceiling and lock an unrelated IP.
// This is the misconfig case — behind a proxy with no TrustedProxyCIDRs every
// client would actually share one IP, but spreading across IPs here proves the
// global counter is genuinely independent of the per-IP buckets.
func TestGlobalCeilingLocksAcrossDistinctIPs(t *testing.T) {
	a := newTestAuth()
	// Drive globalMaxAttempts failures, each from a unique IP so no per-IP
	// bucket ever reaches maxAttempts (3). Only the global tally accumulates.
	for i := 0; i < globalMaxAttempts; i++ {
		ip := fmt.Sprintf("198.51.100.%d", i)
		a.registerFailure(ip)
	}
	// A never-seen IP must now be locked purely by the global backstop.
	if locked, d := a.locked("203.0.113.7"); !locked || d <= 0 {
		t.Fatalf("expected global lockout for unrelated IP, got locked=%v d=%v", locked, d)
	}
}

// TestGlobalCeilingSelfClears confirms the global window is rolling and does not
// produce a permanent lockout: once lockoutWindow has elapsed since the window
// opened, the tally resets and a single fresh failure cannot re-trip it.
func TestGlobalCeilingSelfClears(t *testing.T) {
	a := newTestAuth()
	for i := 0; i < globalMaxAttempts; i++ {
		a.registerFailure(fmt.Sprintf("198.51.100.%d", i))
	}
	// Force the lockout and the rolling window into the past so the next failure
	// starts a clean window instead of accumulating on the tripped one.
	a.mu.Lock()
	a.globalUntil = time.Now().Add(-time.Second)
	a.globalWindowStart = time.Now().Add(-2 * lockoutWindow)
	a.mu.Unlock()

	if locked, _ := a.locked("203.0.113.7"); locked {
		t.Fatal("global lockout should have self-cleared")
	}
	// One isolated failure in the fresh window must not re-trip the ceiling.
	a.registerFailure("203.0.113.8")
	if locked, _ := a.locked("203.0.113.9"); locked {
		t.Fatal("a single failure after self-clear must not re-trip the global ceiling")
	}
}

// TestPerIPLockoutStillTrips guards against the global change masking the
// original per-IP behaviour: maxAttempts misses from one IP lock that IP.
func TestPerIPLockoutStillTrips(t *testing.T) {
	a := newTestAuth()
	var locked bool
	for i := 0; i < maxAttempts; i++ {
		locked, _ = a.registerFailure("192.0.2.1")
	}
	if !locked {
		t.Fatalf("expected per-IP lockout after %d misses", maxAttempts)
	}
	if l, _ := a.locked("192.0.2.1"); !l {
		t.Fatal("locked(ip) should report the per-IP lockout")
	}
}
