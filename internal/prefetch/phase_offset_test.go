package prefetch

import (
	"testing"
	"time"
)

// TestPrefetchPhaseOffset_DefaultFiveWhenUnset pins the safe fallback when no
// setting is stored. The pre-S35 hardcoded behavior was 5% (base/20), so an
// empty / missing setting must collapse to that exact value — otherwise users
// who never touched the threshold UI would silently see their L1 cadence shift
// when this code rolled out.
func TestPrefetchPhaseOffset_DefaultFiveWhenUnset(t *testing.T) {
	base := 100 * time.Minute
	want := 5 * time.Minute

	cases := []struct {
		name string
		s    SettingsProvider
	}{
		{"nil provider", nil},
		{"empty map", &mockSettings{data: map[string]string{}}},
		{"blank string", &mockSettings{data: map[string]string{"prefetch_threshold": ""}}},
		{"whitespace only", &mockSettings{data: map[string]string{"prefetch_threshold": "   "}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{settings: tc.s}
			got := s.prefetchPhaseOffset(base)
			if got != want {
				t.Errorf("prefetchPhaseOffset(%s) = %s, want %s", base, got, want)
			}
		})
	}
}

// TestPrefetchPhaseOffset_RejectsInvalid pins the [1, 99] envelope. Anything
// outside the band (0, 100, negative, non-integer, garbage) falls back to 5%
// rather than letting a misconfigured setting silently push the offset to
// zero or beyond the cycle period.
func TestPrefetchPhaseOffset_RejectsInvalid(t *testing.T) {
	base := 100 * time.Minute
	want := 5 * time.Minute

	for _, v := range []string{"0", "100", "-1", "-50", "9999", "abc", "1.5", "50%", "  "} {
		t.Run("value="+v, func(t *testing.T) {
			s := &Scheduler{settings: &mockSettings{data: map[string]string{"prefetch_threshold": v}}}
			got := s.prefetchPhaseOffset(base)
			if got != want {
				t.Errorf("prefetchPhaseOffset with %q = %s, want %s (5%% fallback)", v, got, want)
			}
		})
	}
}

// TestPrefetchPhaseOffset_HonoursValid sweeps the legal range and pins the
// linear pct/100 relationship. 20 (the new docker default) must produce
// exactly 20% of the base period, otherwise the operator's "I want the first
// fetch inside the first 20% of the window" intent is broken.
func TestPrefetchPhaseOffset_HonoursValid(t *testing.T) {
	base := 100 * time.Minute

	cases := []struct {
		pct  string
		want time.Duration
	}{
		{"1", 1 * time.Minute},
		{"5", 5 * time.Minute},
		{"20", 20 * time.Minute}, // docker default
		{"50", 50 * time.Minute}, // global middleware default
		{"75", 75 * time.Minute},
		{"99", 99 * time.Minute},
	}
	for _, tc := range cases {
		t.Run("pct="+tc.pct, func(t *testing.T) {
			s := &Scheduler{settings: &mockSettings{data: map[string]string{"prefetch_threshold": tc.pct}}}
			got := s.prefetchPhaseOffset(base)
			if got != tc.want {
				t.Errorf("pct=%s: got %s, want %s", tc.pct, got, tc.want)
			}
		})
	}
}

// TestPrefetchPhaseOffset_NonZero guards the rand.Int63n(spread+1) caller in
// bucketLoop: a spread of zero is fine (rand.Int63n(1)==0, degenerate but
// safe) but a negative one panics. The helper must always return a strictly
// positive duration even when integer division would truncate to zero on a
// pathologically small base (e.g. test-shrunk overrides) or a zero base.
func TestPrefetchPhaseOffset_NonZero(t *testing.T) {
	cases := []struct {
		name string
		base time.Duration
		pct  string
	}{
		{"tiny base default pct", 10 * time.Nanosecond, ""},
		{"tiny base pct=1", 10 * time.Nanosecond, "1"},
		{"zero base default pct", 0, ""},
		{"zero base pct=20", 0, "20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := map[string]string{}
			if tc.pct != "" {
				data["prefetch_threshold"] = tc.pct
			}
			s := &Scheduler{settings: &mockSettings{data: data}}
			got := s.prefetchPhaseOffset(tc.base)
			if got <= 0 {
				t.Errorf("got non-positive %s — rand.Int63n caller would panic or degenerate", got)
			}
		})
	}
}

// TestPrefetchPhaseOffset_LiveSettingChange pins the late-binding contract:
// the helper reads s.settings.Get() at every invocation, so a user toggling
// the threshold in the UI sees the new value reflected on the very next
// bucket-cycle restart, not pinned to whatever was set at Scheduler.New time.
func TestPrefetchPhaseOffset_LiveSettingChange(t *testing.T) {
	base := 100 * time.Minute
	ms := &mockSettings{data: map[string]string{"prefetch_threshold": "10"}}
	s := &Scheduler{settings: ms}

	if got := s.prefetchPhaseOffset(base); got != 10*time.Minute {
		t.Fatalf("initial: got %s, want 10m", got)
	}
	_ = ms.Set("prefetch_threshold", "40")
	if got := s.prefetchPhaseOffset(base); got != 40*time.Minute {
		t.Errorf("after Set: got %s, want 40m", got)
	}
	_ = ms.Set("prefetch_threshold", "bogus")
	if got := s.prefetchPhaseOffset(base); got != 5*time.Minute {
		t.Errorf("after garbage Set: got %s, want 5m fallback", got)
	}
}

// TestPrefetchPhaseOffset_NoOverflow guards the int64 multiplication path
// against silent overflow at the upper end of plausible bucket periods (the
// `all` bucket can be days). 7 days × 99 still fits comfortably in int64,
// but a regression to base.Nanoseconds() * pct on int (not int64) would
// silently truncate on 32-bit builds.
func TestPrefetchPhaseOffset_NoOverflow(t *testing.T) {
	base := 7 * 24 * time.Hour
	s := &Scheduler{settings: &mockSettings{data: map[string]string{"prefetch_threshold": "99"}}}
	got := s.prefetchPhaseOffset(base)
	want := time.Duration(int64(base) * 99 / 100)
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got <= 0 {
		t.Errorf("got non-positive %s — overflow regression", got)
	}
}
