package versionintel

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	// apkLagMonths is how far behind the current date the spoofed Reddit app
	// version is held — pessimistically, so the build is never bleeding-edge.
	apkLagMonths = 4
	// apkMaxMajor is the hard ceiling for the major number: it must never
	// exceed this regardless of how the random accumulation lands.
	apkMaxMajor = 44
)

// monthlyIncrements is the per-month bump applied to the major number.
// Observed on APKMirror as ~3-5, clustered on 4 — hence 4 is over-weighted.
var monthlyIncrements = []int{3, 4, 4, 4, 5}

// deriveMajorNumber models Reddit's "YYYY.N.P" numbering: N restarts near 1
// each January and accumulates a random ~3-5 per month. It is capped at a
// random per-year ceiling in [40,44] — Reddit rolls the year over somewhere
// in that band — and never exceeds apkMaxMajor. Each call is independently
// randomized, so callers must guard against downgrade.
func deriveMajorNumber(month int) int {
	n := 0
	for m := 1; m <= month; m++ {
		n += monthlyIncrements[rand.IntN(len(monthlyIncrements))]
	}
	yearCap := 40 + rand.IntN(5) // 40-44 inclusive
	if n > yearCap {
		n = yearCap
	}
	if n > apkMaxMajor {
		n = apkMaxMajor
	}
	return n
}

// deriveAppVersion synthesizes a Reddit app version/build for a calendar date.
// The patch (".0") is fixed — the true minor bumps are unpredictable, so we
// pessimistically skip them. The build's 3-digit serial is an internal Reddit
// CI counter that cannot be derived; Reddit is not observed to validate it.
func deriveAppVersion(t time.Time) (appVersion, string) {
	year := t.Year()
	major := deriveMajorNumber(int(t.Month()))
	v := appVersion{year: year, major: major, patch: 0}
	build := fmt.Sprintf("%02d%02d%03d", year%100, major, rand.IntN(1000))
	return v, build
}

// DeriveAPKVersion returns a conservatively-derived app version/build for the
// current date, held apkLagMonths behind real time. Fully offline; each call
// is randomized, so callers must guard against downgrade (see DecideAPKVersion).
func DeriveAPKVersion(now time.Time) (version, build string) {
	v, b := deriveAppVersion(now.AddDate(0, -apkLagMonths, 0))
	return v.String(), b
}

// DecideAPKVersion picks the app version the device should adopt: the
// conservatively-derived version ~4 months behind now. Because each
// derivation is randomized it never downgrades — if the freshly-derived
// version is not strictly newer than the current one, nothing changes.
func DecideAPKVersion(current string, now time.Time) (version, build string, changed bool) {
	cur, curOK := parseAppVersion(current)
	target, build := deriveAppVersion(now.AddDate(0, -apkLagMonths, 0))
	if curOK && !target.newer(cur) {
		return current, "", false // already at or ahead of target — never downgrade
	}
	return target.String(), build, true
}
