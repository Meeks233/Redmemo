package prefetch

import (
	"testing"

	"github.com/redmemo/redmemo/internal/reddit"
)

// TestResolveL3MinComments_DefaultsToZero pins the safety contract: a missing
// or unparseable setting collapses to 0 (= filter disabled) so a corrupt DB
// row cannot accidentally freeze every post out of L3.
func TestResolveL3MinComments_DefaultsToZero(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want int
	}{
		{"nil settings", "", 0},
		{"blank value", "", 0},
		{"whitespace", "   ", 0},
		{"non-numeric", "lots", 0},
		{"negative (DB tampered)", "-3", 0},
		{"zero (canonical disable)", "0", 0},
		{"small positive", "5", 5},
		{"large positive", "12345", 12345},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{}
			if tc.set != "" || tc.name != "nil settings" {
				s.settings = &mockSettings{data: map[string]string{"prefetch_l3_min_comments": tc.set}}
			}
			if got := s.resolveL3MinComments(); got != tc.want {
				t.Errorf("resolveL3MinComments() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestNumCommentsOf_Parses pins how the comment count is extracted from the
// archived reddit.Post — Comments[1] is the raw decimal string per
// FormatNum's contract; parse failure must collapse to 0 so an unparseable
// post can never bypass a positive threshold.
func TestNumCommentsOf_Parses(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"nil-safe blank", "", 0},
		{"zero", "0", 0},
		{"small", "5", 5},
		{"large", "12345", 12345},
		{"whitespace-padded", "  42  ", 42},
		{"non-numeric", "many", 0},
		{"negative (corrupt)", "-3", 0},
		{"float (corrupt)", "5.5", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &reddit.Post{Comments: [2]string{tc.raw, tc.raw}}
			if got := numCommentsOf(p); got != tc.want {
				t.Errorf("numCommentsOf(%q) = %d, want %d", tc.raw, got, tc.want)
			}
			if got := numCommentsOf(nil); got != 0 {
				t.Errorf("numCommentsOf(nil) = %d, want 0", got)
			}
		})
	}
}

// TestL3MeetsThreshold_GateLogic pins the three branches of l3MeetsThreshold:
// disabled (threshold ≤ 0) always passes, at/over threshold passes, under
// threshold is frozen. Uses a Scheduler without runStore so the side-effect
// recording branch is exercised in its nil-safe form (the test would crash on
// nil dereference if Record were unguarded).
func TestL3MeetsThreshold_GateLogic(t *testing.T) {
	cases := []struct {
		name        string
		threshold   string
		numComments int
		want        bool
	}{
		{"disabled (zero) lets any post through", "0", 0, true},
		{"disabled (blank) lets any post through", "", 100, true},
		{"at threshold passes", "5", 5, true},
		{"over threshold passes", "5", 10, true},
		{"under threshold frozen", "5", 4, false},
		{"zero comments vs threshold 1 frozen", "1", 0, false},
		{"negative DB value treated as disabled", "-3", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Scheduler{
				settings: &mockSettings{data: map[string]string{"prefetch_l3_min_comments": tc.threshold}},
				Events:   NewEventLog(10),
			}
			got := s.l3MeetsThreshold(tc.numComments, "golang", "abc123")
			if got != tc.want {
				t.Errorf("l3MeetsThreshold(%d) with threshold=%q = %v, want %v", tc.numComments, tc.threshold, got, tc.want)
			}
		})
	}
}
