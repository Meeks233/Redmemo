package oauth

import (
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/versionintel"
)

// DefaultDeviceProfile builds the first-boot device profile. The device_id is
// minted with a fresh UUID; it and the Android version stay fixed until the
// first major rotation (~3 years out). The Android version seeds from the
// fallback constant — the monthly StatCounter poll later schedules whatever
// is most popular for the next rotation. The app version derives ~4 months
// behind now (see internal/versionintel).
func DefaultDeviceProfile() *store.DeviceProfile {
	now := time.Now()
	appVersion, build := versionintel.DeriveAPKVersion(now)
	androidVer := versionintel.FallbackAndroidVersion
	return &store.DeviceProfile{
		DeviceID:           uuid.New().String(),
		UserAgent:          versionintel.BuildUserAgent(appVersion, build, androidVer),
		AndroidVersion:     androidVer,
		AppVersion:         appVersion,
		Build:              build,
		DeviceBornAt:       now,
		DeviceLifespanDays: versionintel.RandomDeviceLifespanDays(),
		OSNextCheckAt:      now,
	}
}

// ResolveDeviceProfile returns the pinned device profile, creating it on first
// boot. Once a row exists it is never overwritten — the device identity stays
// stable for the life of the deployment.
func ResolveDeviceProfile(s *store.DeviceProfileStore) (*store.DeviceProfile, error) {
	existing, err := s.Get()
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	if err := s.Insert(DefaultDeviceProfile()); err != nil {
		return nil, err
	}
	// Re-read so the row that actually persisted is the one returned.
	profile, err := s.Get()
	if err != nil {
		return nil, err
	}
	if profile == nil {
		return nil, fmt.Errorf("device profile missing after insert")
	}
	return profile, nil
}

// IdentityFromProfile builds a SpoofIdentity from the pinned device profile.
// device_id and User-Agent are fixed; the per-request-varying fields (qos,
// media-codecs) stay randomized, mirroring the real app.
func IdentityFromProfile(p *store.DeviceProfile) SpoofIdentity {
	qos := float64(rand.IntN(99001)+1000) / 1000.0

	codecs := "video/avc, video/hevc"
	if rand.IntN(2) == 0 {
		codecs += ", video/x-vnd.on2.vp9"
	}

	headers := map[string]string{
		"User-Agent":            p.UserAgent,
		"x-reddit-retry":        "algo=no-retries",
		"x-reddit-compression":  "1",
		"x-reddit-qos":          fmt.Sprintf("%.3f", qos),
		"x-reddit-media-codecs": codecs,
		"Content-Type":          "application/json; charset=UTF-8",
		"client-vendor-id":      p.DeviceID,
		"X-Reddit-Device-Id":    p.DeviceID,
		// Set explicitly so the transport does not inject its own default
		// ("gzip, deflate, br"); real Reddit Android (OkHttp) advertises gzip only.
		"Accept-Encoding": "gzip",
	}

	return SpoofIdentity{
		UserAgent: p.UserAgent,
		DeviceID:  p.DeviceID,
		Headers:   headers,
	}
}
