package oauth

import (
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type SpoofIdentity struct {
	UserAgent string
	DeviceID  string
	Headers   map[string]string
}

// androidUA holds the resolved Android User-Agent configuration. It is built
// once at package init from REDMEMO_ANDROID_* environment variables, falling
// back to a small curated list of recent (verified) Reddit Android releases.
//
// Operators are STRONGLY encouraged to override these via env vars so the
// spoofed User-Agent tracks a current, real Reddit Android client — a stale
// hardcoded version is itself a fingerprint. See config.example.yaml.
var androidUA = resolveAndroidUA()

type androidUASettings struct {
	fullUA      string   // verbatim override; when set, takes full precedence
	appVersions []string // "Version YYYY.WW.X/Build NNNNNNN" entries
	osMin       int      // inclusive Android major-version lower bound
	osMax       int      // inclusive Android major-version upper bound
}

// defaultAndroidAppVersions are recent Reddit Android releases verified on
// APKMirror (build numbers included). Deliberately "recent but not latest"
// (~Feb 2026 builds) to balance authenticity against server-side version
// checks. Override with REDMEMO_ANDROID_APP_VERSION to keep this current.
var defaultAndroidAppVersions = []string{
	"Version 2026.05.0/Build 2605040",
	"Version 2026.05.1/Build 2605051",
	"Version 2026.06.0/Build 2606110",
	"Version 2026.07.0/Build 2607141",
}

const (
	defaultAndroidOSMin = 12
	defaultAndroidOSMax = 15
)

// resolveAndroidUA reads the REDMEMO_ANDROID_* environment variables once.
//
//	REDMEMO_ANDROID_USER_AGENT  — full UA string, used verbatim (highest priority)
//	REDMEMO_ANDROID_APP_VERSION — comma-separated "Version .../Build ..." entries
//	REDMEMO_ANDROID_APP_DATE    — a date (YYYY-MM-DD) auto-translated to a version
//	REDMEMO_ANDROID_OS_VERSION  — "13" (fixed) or "12-15" (inclusive range)
//
// Precedence for the app version: USER_AGENT > APP_VERSION > APP_DATE > default.
func resolveAndroidUA() androidUASettings {
	s := androidUASettings{
		appVersions: defaultAndroidAppVersions,
		osMin:       defaultAndroidOSMin,
		osMax:       defaultAndroidOSMax,
	}

	if ua := strings.TrimSpace(os.Getenv("REDMEMO_ANDROID_USER_AGENT")); ua != "" {
		s.fullUA = ua
		log.Printf("oauth: using fixed Android User-Agent from REDMEMO_ANDROID_USER_AGENT")
		return s
	}

	versionsSet := false
	if raw := os.Getenv("REDMEMO_ANDROID_APP_VERSION"); raw != "" {
		var versions []string
		for _, part := range strings.Split(raw, ",") {
			if v := strings.TrimSpace(part); v != "" {
				versions = append(versions, v)
			}
		}
		if len(versions) > 0 {
			s.appVersions = versions
			versionsSet = true
			log.Printf("oauth: using %d Android app version(s) from REDMEMO_ANDROID_APP_VERSION", len(versions))
		}
	}

	if raw := strings.TrimSpace(os.Getenv("REDMEMO_ANDROID_APP_DATE")); raw != "" {
		switch {
		case versionsSet:
			log.Printf("oauth: REDMEMO_ANDROID_APP_DATE ignored — REDMEMO_ANDROID_APP_VERSION takes precedence")
		default:
			if v, ok := deriveAppVersionFromDate(raw); ok {
				s.appVersions = []string{v}
				log.Printf("oauth: derived Android app version %q from REDMEMO_ANDROID_APP_DATE=%s", v, raw)
			} else {
				log.Printf("oauth: invalid REDMEMO_ANDROID_APP_DATE %q (want YYYY-MM-DD), using default versions", raw)
			}
		}
	}

	if raw := strings.TrimSpace(os.Getenv("REDMEMO_ANDROID_OS_VERSION")); raw != "" {
		if lo, hi, ok := parseOSRange(raw); ok {
			s.osMin, s.osMax = lo, hi
		} else {
			log.Printf("oauth: invalid REDMEMO_ANDROID_OS_VERSION %q, using default %d-%d", raw, s.osMin, s.osMax)
		}
	}

	return s
}

// parseOSRange parses "13" or "12-15" into an inclusive [lo,hi] range.
func parseOSRange(raw string) (lo, hi int, ok bool) {
	if loStr, hiStr, found := strings.Cut(raw, "-"); found {
		l, err1 := strconv.Atoi(strings.TrimSpace(loStr))
		h, err2 := strconv.Atoi(strings.TrimSpace(hiStr))
		if err1 != nil || err2 != nil || l < 1 || h < l {
			return 0, 0, false
		}
		return l, h, true
	}
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 1 {
		return 0, 0, false
	}
	return v, v, true
}

// deriveAppVersionFromDate translates a calendar date (YYYY-MM-DD) into a
// plausible Reddit Android "Version .../Build ..." string, following Reddit's
// year.week scheme and the 2026-era build-number encoding: a 7-digit build of
// YY (2) + WW (2) + serial (3).
//
// IMPORTANT: the 3-digit build serial is an internal Reddit CI counter that
// cannot be computed from a date — it is synthesized here to be structurally
// plausible (correct length and YYWW prefix). Reddit does not appear to
// validate the build number, but for a guaranteed-authentic version/build
// pair, set REDMEMO_ANDROID_APP_VERSION to a value verified on APKMirror.
//
// The week is the ISO-8601 week of the given date; Reddit's own numbering can
// be off by a week or so, so the derived version is approximate by design.
func deriveAppVersionFromDate(raw string) (string, bool) {
	t, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return "", false
	}
	year, week := t.ISOWeek()
	serial := (week * 20) % 1000
	return fmt.Sprintf("Version %d.%02d.0/Build %02d%02d%03d",
		year, week, year%100, week, serial), true
}

// buildUA renders a single Android User-Agent from the resolved settings.
func (s androidUASettings) buildUA() string {
	if s.fullUA != "" {
		return s.fullUA
	}
	version := s.appVersions[rand.IntN(len(s.appVersions))]
	androidVer := s.osMin
	if s.osMax > s.osMin {
		androidVer += rand.IntN(s.osMax - s.osMin + 1)
	}
	return fmt.Sprintf("Reddit/%s/Android %d", version, androidVer)
}

func GenerateIdentity() SpoofIdentity {
	deviceID := uuid.New().String()
	ua := androidUA.buildUA()

	qos := float64(rand.IntN(99001)+1000) / 1000.0

	codecs := "video/avc, video/hevc"
	if rand.IntN(2) == 0 {
		codecs += ", video/x-vnd.on2.vp9"
	}

	headers := map[string]string{
		"User-Agent":            ua,
		"x-reddit-retry":        "algo=no-retries",
		"x-reddit-compression":  "1",
		"x-reddit-qos":          fmt.Sprintf("%.3f", qos),
		"x-reddit-media-codecs": codecs,
		"Content-Type":          "application/json; charset=UTF-8",
		"client-vendor-id":      deviceID,
		"X-Reddit-Device-Id":    deviceID,
		// Set explicitly so the transport does not inject its own default
		// ("gzip, deflate, br"); real Reddit Android (OkHttp) advertises gzip only.
		"Accept-Encoding": "gzip",
	}

	return SpoofIdentity{
		UserAgent: ua,
		DeviceID:  deviceID,
		Headers:   headers,
	}
}
