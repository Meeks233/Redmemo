package versionintel

import (
	"testing"
	"time"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// --- app version parsing / comparison ---

func TestParseAppVersion(t *testing.T) {
	v, ok := parseAppVersion("2026.06.0")
	if !ok || v.year != 2026 || v.major != 6 || v.patch != 0 {
		t.Fatalf("parseAppVersion(2026.06.0) = %+v ok=%v", v, ok)
	}
	for _, bad := range []string{"", "2026.6", "v2026.06.0", "2026.06.0-beta", "abc"} {
		if _, ok := parseAppVersion(bad); ok {
			t.Errorf("parseAppVersion(%q) unexpectedly ok", bad)
		}
	}
}

func TestAppVersionNewer(t *testing.T) {
	a, _ := parseAppVersion("2026.20.0")
	b, _ := parseAppVersion("2026.06.0")
	if !a.newer(b) || b.newer(a) || a.newer(a) {
		t.Error("appVersion.newer ordering wrong")
	}
	c, _ := parseAppVersion("2027.01.0")
	if !c.newer(a) {
		t.Error("year should dominate the major number")
	}
}

func TestBuildUserAgent(t *testing.T) {
	ua := BuildUserAgent("2026.06.0", "2606110", 15)
	want := "Reddit/Version 2026.06.0/Build 2606110/Android 15"
	if ua != want {
		t.Errorf("BuildUserAgent = %q, want %q", ua, want)
	}
}

// --- Android popular-version lookup ---

func TestParseAndroidShares(t *testing.T) {
	csv := "Date,15.0,16.0,Other\n2026-03,40.0,5.0,55.0\n2026-04,38.5,8.2,53.3\n"
	shares, err := parseAndroidShares([]byte(csv))
	if err != nil {
		t.Fatalf("parseAndroidShares: %v", err)
	}
	if shares[15] != 38.5 || shares[16] != 8.2 {
		t.Errorf("shares = %+v, want latest row {15:38.5 16:8.2}", shares)
	}
	if _, ok := shares[0]; ok {
		t.Error("the Date/Other columns must not produce entries")
	}
}

func TestParseAndroidShares_Empty(t *testing.T) {
	if _, err := parseAndroidShares([]byte("Date,15.0\n")); err == nil {
		t.Error("expected error for header-only CSV")
	}
}

// TestParseAndroidShares_Series feeds a realistic multi-column, multi-month
// CSV and checks that (a) the latest month's row wins, and (b) mostPopular
// returns the genuine argmax — not a column-position artefact.
func TestParseAndroidShares_Series(t *testing.T) {
	csv := "Date,8.0,11.0,12.0,16.0,13.0,14.0,15.0,Other\n" +
		"2026-03,1.0,5.0,8.0,2.0,18.0,30.0,12.0,24.0\n" +
		"2026-04,0.8,4.0,7.0,6.0,15.0,28.0,22.0,17.2\n" +
		"2026-05,0.5,3.5,6.0,9.0,13.0,25.0,33.0,9.5\n"
	shares, err := parseAndroidShares([]byte(csv))
	if err != nil {
		t.Fatalf("parseAndroidShares: %v", err)
	}
	// Latest month is 2026-05.
	if shares[15] != 33.0 || shares[14] != 25.0 || shares[16] != 9.0 {
		t.Errorf("latest-month shares wrong: %+v", shares)
	}
	// 15 leads 2026-05 even though 16's column sits earlier in the header.
	if got := mostPopular(shares); got != 15 {
		t.Errorf("mostPopular = %d, want 15 (true install-base leader)", got)
	}
}

// TestParseAndroidShares_UnsortedCSV reproduces the real StatCounter export,
// which appends trailing all-zero rows for pre-launch months — the latest
// month must be found by Date, not by row position.
func TestParseAndroidShares_UnsortedCSV(t *testing.T) {
	csv := "Date,14.0,15.0,16.0\n" +
		"2026-04,28.0,22.0,6.0\n" +
		"2026-05,25.0,19.0,40.0\n" + // genuine latest month
		"2009-01,0,0,0\n" + // trailing pre-launch noise
		"2009-02,0,0,0\n"
	shares, err := parseAndroidShares([]byte(csv))
	if err != nil {
		t.Fatalf("parseAndroidShares: %v", err)
	}
	if shares[16] != 40.0 {
		t.Errorf("shares[16] = %v, want 40.0 (from 2026-05, not the last row)", shares[16])
	}
	if got := mostPopular(shares); got != 16 {
		t.Errorf("mostPopular = %d, want 16", got)
	}
}

// TestDeriveAPKVersion_DateSweep runs the derivation across six years of
// monthly dates and checks every result against the scheme's invariants.
func TestDeriveAPKVersion_DateSweep(t *testing.T) {
	for d := date(2026, 1, 1); d.Before(date(2032, 1, 1)); d = d.AddDate(0, 1, 0) {
		version, build := DeriveAPKVersion(d)
		v, ok := parseAppVersion(version)
		if !ok {
			t.Fatalf("date %s: unparseable version %q", d.Format("2006-01"), version)
		}
		target := d.AddDate(0, -apkLagMonths, 0)
		if v.year != target.Year() {
			t.Errorf("date %s: derived year %d, want %d (4 months behind)",
				d.Format("2006-01"), v.year, target.Year())
		}
		if v.major < 1 || v.major > apkMaxMajor {
			t.Errorf("date %s: major %d outside [1,%d]", d.Format("2006-01"), v.major, apkMaxMajor)
		}
		if v.patch != 0 {
			t.Errorf("date %s: patch %d, want 0", d.Format("2006-01"), v.patch)
		}
		if len(build) != 7 {
			t.Errorf("date %s: build %q not 7 digits", d.Format("2006-01"), build)
		}
	}
}

// TestDeriveMajorNumber_AllMonths checks the monthly accumulation lands in the
// expected band for every month of a year, across many random draws.
func TestDeriveMajorNumber_AllMonths(t *testing.T) {
	for month := 1; month <= 12; month++ {
		lo, hi := 3*month, 5*month
		if lo > 40 { // capped by the random [40,44] year ceiling
			lo = 40
		}
		if hi > apkMaxMajor {
			hi = apkMaxMajor
		}
		for i := 0; i < 300; i++ {
			n := deriveMajorNumber(month)
			if n < lo || n > hi {
				t.Fatalf("deriveMajorNumber(%d) = %d, want [%d,%d]", month, n, lo, hi)
			}
		}
	}
}

// TestDecideAPKVersion_MonotonicOverTime chains the decision month by month
// for five years, exactly as the live rotation does, and asserts the version
// only ever moves forward and meaningfully advances overall.
func TestDecideAPKVersion_MonotonicOverTime(t *testing.T) {
	current := "2026.01.0"
	start, _ := parseAppVersion(current)
	prev := start
	bumps := 0
	for d := date(2026, 1, 15); d.Before(date(2031, 1, 1)); d = d.AddDate(0, 1, 0) {
		version, build, changed := DecideAPKVersion(current, d)
		if !changed {
			continue
		}
		nv, ok := parseAppVersion(version)
		if !ok {
			t.Fatalf("date %s: unparseable %q", d.Format("2006-01"), version)
		}
		if !nv.newer(prev) {
			t.Fatalf("date %s: version regressed %s -> %s", d.Format("2006-01"), prev, version)
		}
		if want := d.AddDate(0, -apkLagMonths, 0).Year(); nv.year != want {
			t.Errorf("date %s: adopted year %d, want %d", d.Format("2006-01"), nv.year, want)
		}
		if len(build) != 7 {
			t.Fatalf("date %s: build %q not 7 digits", d.Format("2006-01"), build)
		}
		current, prev = version, nv
		bumps++
	}
	if !prev.newer(start) {
		t.Errorf("version did not advance over five years: %s -> %s", start, prev)
	}
	if bumps == 0 {
		t.Error("expected at least one version bump over five years")
	}
}

func TestMostPopular(t *testing.T) {
	if got := mostPopular(map[int]float64{14: 30, 15: 45, 16: 10}); got != 15 {
		t.Errorf("mostPopular = %d, want 15 (largest share)", got)
	}
	// Ties break toward the newer version.
	if got := mostPopular(map[int]float64{15: 40, 16: 40}); got != 16 {
		t.Errorf("mostPopular tie = %d, want 16", got)
	}
}

func TestFallbackAndroidVersion(t *testing.T) {
	if FallbackAndroidVersion != 15 {
		t.Errorf("FallbackAndroidVersion = %d, want 15", FallbackAndroidVersion)
	}
}

func TestRandomDeviceLifespanDays(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := RandomDeviceLifespanDays()
		if d < deviceLifespanMinDays || d > deviceLifespanMaxDays {
			t.Fatalf("RandomDeviceLifespanDays = %d, want [%d,%d]", d, deviceLifespanMinDays, deviceLifespanMaxDays)
		}
	}
}

// --- APK version derivation (minor rotation) ---

func TestDeriveMajorNumber_MonthlyBand(t *testing.T) {
	for i := 0; i < 200; i++ {
		if n := deriveMajorNumber(1); n < 3 || n > 5 {
			t.Fatalf("deriveMajorNumber(1) = %d, want [3,5]", n)
		}
	}
}

func TestDeriveMajorNumber_Capped(t *testing.T) {
	for i := 0; i < 300; i++ {
		if n := deriveMajorNumber(12); n < 1 || n > apkMaxMajor {
			t.Fatalf("deriveMajorNumber(12) = %d, want [1,%d]", n, apkMaxMajor)
		}
	}
}

func TestDeriveAPKVersion_LagsRealTime(t *testing.T) {
	version, build := DeriveAPKVersion(date(2026, 12, 1))
	v, ok := parseAppVersion(version)
	if !ok {
		t.Fatalf("derived version %q unparseable", version)
	}
	if v.year != 2026 { // 4 months behind 2026-12 is August 2026
		t.Errorf("derived %s not in 2026", version)
	}
	if v.major < 1 || v.major > apkMaxMajor {
		t.Errorf("derived major %d outside [1,%d]", v.major, apkMaxMajor)
	}
	if len(build) != 7 {
		t.Errorf("build %q should be 7 digits", build)
	}
}

func TestDeriveAPKVersion_YearCrossover(t *testing.T) {
	prior, _ := DeriveAPKVersion(date(2027, 2, 15)) // 4mo back = Oct 2026
	if v, _ := parseAppVersion(prior); v.year != 2026 {
		t.Errorf("DeriveAPKVersion(2027-02) = %q, want a 2026.x.0 version", prior)
	}
	fresh, _ := DeriveAPKVersion(date(2027, 5, 15)) // 4mo back = Jan 2027
	if v, _ := parseAppVersion(fresh); v.year != 2027 || v.major > 5 {
		t.Errorf("DeriveAPKVersion(2027-05) = %q, want 2027 with major<=5", fresh)
	}
}

func TestDecideAPKVersion_NeverDowngrade(t *testing.T) {
	for i := 0; i < 50; i++ {
		if _, _, changed := DecideAPKVersion("2026.06.0", date(2026, 5, 19)); changed {
			t.Fatal("must not downgrade below the current app version")
		}
	}
}

func TestDecideAPKVersion_AdvancesOverTime(t *testing.T) {
	version, build, changed := DecideAPKVersion("2026.06.0", date(2026, 12, 1))
	if !changed {
		t.Fatal("expected an app-version advance by late 2026")
	}
	cur, _ := parseAppVersion("2026.06.0")
	got, _ := parseAppVersion(version)
	if !got.newer(cur) {
		t.Errorf("decided %s is not newer than current 2026.06.0", version)
	}
	if len(build) != 7 {
		t.Errorf("build %q should be 7 digits", build)
	}
}
