// Package versionintel keeps the spoofed Android device's version identity
// tracking the real world over a long-running deployment, so the device
// never looks suspiciously stale (an old OS / old app build is itself a
// fingerprint) nor suspiciously bleeding-edge.
//
// It has two independent tracks:
//
//   - Android OS major version — driven by a hardcoded prediction of Google's
//     ~yearly Q2 release cadence, gated by a conservative random 2-6 month
//     adoption delay, and confirmed against StatCounter market-share data.
//   - Reddit app version/build — conservatively derived ~4 months behind the
//     current date, optionally snapped to a real version scraped best-effort
//     from APKMirror.
//
// All decisions are pure functions over the current DeviceProfile; the
// Tracker wires them to the network and the clock. Nothing here ever
// downgrades a version, and external fetches degrade gracefully — a failed
// fetch keeps the current version and retries on the next cycle.
package versionintel

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// BuildUserAgent renders the Reddit Android User-Agent for a version identity.
// The shape matches a real captured client: Reddit/Version <v>/Build <b>/Android <n>.
func BuildUserAgent(appVersion, build string, androidVersion int) string {
	return fmt.Sprintf("Reddit/Version %s/Build %s/Android %d", appVersion, build, androidVersion)
}

// appVersion is a parsed Reddit app version, "YYYY.N.P": year, the major
// number N (restarts each January, accumulates ~4/month), and the patch P.
type appVersion struct {
	year, major, patch int
}

var appVersionRe = regexp.MustCompile(`^(\d{4})\.(\d{1,2})\.(\d+)$`)

// parseAppVersion parses "2026.06.0"; ok is false for any other shape.
func parseAppVersion(s string) (appVersion, bool) {
	m := appVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return appVersion{}, false
	}
	y, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	p, _ := strconv.Atoi(m[3])
	return appVersion{year: y, major: n, patch: p}, true
}

// newer reports whether a is strictly newer than b.
func (a appVersion) newer(b appVersion) bool {
	if a.year != b.year {
		return a.year > b.year
	}
	if a.major != b.major {
		return a.major > b.major
	}
	return a.patch > b.patch
}

func (a appVersion) String() string {
	return fmt.Sprintf("%04d.%02d.%d", a.year, a.major, a.patch)
}
