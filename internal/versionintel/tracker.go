package versionintel

import (
	"context"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redmemo/redmemo/internal/store"
)

const (
	// osCheckInterval is the polite StatCounter polling cadence — once a month.
	osCheckInterval = 30 * 24 * time.Hour
	// deviceLifespanMinDays / deviceLifespanMaxDays bound a device's lifespan.
	// A real user replaces their phone roughly every 3 years; the exact span
	// is randomized per device.
	deviceLifespanMinDays = 900  // ~2.5 years
	deviceLifespanMaxDays = 1280 // ~3.5 years
	// fetchTimeout caps the StatCounter request so a slow public site can
	// never stall a token refresh.
	fetchTimeout = 10 * time.Second
)

func randRange(lo, hi int) int { return lo + rand.IntN(hi-lo+1) }

// RandomDeviceLifespanDays returns a fresh randomized device lifespan (~3
// years), in days.
func RandomDeviceLifespanDays() int {
	return randRange(deviceLifespanMinDays, deviceLifespanMaxDays)
}

// Tracker advances the device version identity. It is stateless beyond its
// HTTP client and clock; all carry-over state lives on the DeviceProfile.
type Tracker struct {
	httpClient *http.Client
	now        func() time.Time
}

// NewTracker builds a Tracker. httpClient is used only for the monthly
// StatCounter poll; each request is additionally wrapped in fetchTimeout.
func NewTracker(httpClient *http.Client) *Tracker {
	return &Tracker{httpClient: httpClient, now: time.Now}
}

// Rotate advances the device's version identity for a freshly-minted token.
// It performs, in order:
//
//   - the minor rotation — re-deriving the Reddit app version (~4 months
//     behind now), bound to this token, downgrade-guarded;
//   - a monthly StatCounter poll that records the most popular Android version
//     as the OS scheduled for the next major rotation;
//   - the major rotation — roughly every 3 years, modelling the user
//     replacing their phone: a brand-new device_id plus the scheduled Android
//     version, valid for the next ~3 years.
//
// identityChanged reports whether a field affecting the spoofed identity (and
// thus the User-Agent) moved. The poll degrades gracefully — a failed fetch
// leaves the schedule untouched.
func (t *Tracker) Rotate(ctx context.Context, p store.DeviceProfile) (updated store.DeviceProfile, identityChanged bool) {
	now := t.now()
	updated = p

	// Minor rotation — app version, every token.
	if version, build, changed := DecideAPKVersion(p.AppVersion, now); changed {
		log.Printf("versionintel: app version %s -> %s (build %s)", p.AppVersion, version, build)
		updated.AppVersion, updated.Build = version, build
		identityChanged = true
	}

	// Monthly poll — schedule the most popular Android version for the next
	// major rotation. Runs before the major-rotation check so a freshly
	// observed version can be consumed in the same call if a rotation is due.
	if !now.Before(updated.OSNextCheckAt) {
		t.pollPopularAndroid(ctx, &updated)
		updated.OSNextCheckAt = now.Add(osCheckInterval)
	}

	// Major rotation — a new phone every ~3 years.
	if now.After(updated.DeviceBornAt.Add(time.Duration(updated.DeviceLifespanDays) * 24 * time.Hour)) {
		majorRotation(&updated, now)
		identityChanged = true
	}

	if identityChanged {
		updated.UserAgent = BuildUserAgent(updated.AppVersion, updated.Build, updated.AndroidVersion)
	}
	return updated, identityChanged
}

func (t *Tracker) pollPopularAndroid(ctx context.Context, p *store.DeviceProfile) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	popular, err := FetchPopularAndroidVersion(fctx, t.httpClient, t.now())
	if err != nil {
		log.Printf("versionintel: popular-Android lookup failed (%v) — keeping schedule", err)
		return
	}
	// Only ever schedule forward: above the current OS and any pending one.
	if popular > p.AndroidVersion && popular > p.NextAndroidVersion {
		log.Printf("versionintel: android %d scheduled for the next major rotation", popular)
		p.NextAndroidVersion = popular
	}
}

// majorRotation regenerates the device identity: a brand-new device_id and the
// Android version scheduled by the popularity poll, with a fresh ~3-year
// lifespan — modelling the user buying a new phone. The app version is left to
// the minor rotation. When nothing newer was scheduled, the OS version is kept.
func majorRotation(p *store.DeviceProfile, now time.Time) {
	android := p.AndroidVersion
	if p.NextAndroidVersion > android {
		android = p.NextAndroidVersion
	}
	log.Printf("versionintel: major rotation — new device (android %d -> %d)", p.AndroidVersion, android)
	p.DeviceID = uuid.New().String()
	p.AndroidVersion = android
	p.NextAndroidVersion = 0
	p.DeviceBornAt = now
	p.DeviceLifespanDays = RandomDeviceLifespanDays()
}
