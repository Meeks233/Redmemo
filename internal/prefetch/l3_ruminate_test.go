package prefetch

import (
	"context"
	"testing"

	"github.com/redmemo/redmemo/internal/store"
)

// TestShouldRuminate exhaustively pins the rumination decision — the single
// rule both the SQL prefilter and the in-wave gate obey: re-fetch a post's
// comments iff the thread grew past the count we recorded at the last L3 fetch
// and still clears the min-comments waterline, and only when a baseline exists.
func TestShouldRuminate(t *testing.T) {
	cases := []struct {
		name        string
		current     int
		last        int
		minComments int
		want        bool
	}{
		// The motivating scenario: L1 saw the post at 10 comments, archived L3,
		// then L1 re-scanned at 13. Someone replied — ruminate.
		{"grew 10 to 13", 13, 10, 0, true},
		{"grew by one", 11, 10, 0, true},
		{"grew by one over threshold", 11, 10, 5, true},

		// No growth: the snapshot is current, nothing to do.
		{"unchanged", 13, 13, 0, false},
		{"unchanged at threshold", 5, 5, 5, false},

		// Negative growth: comments were deleted/removed upstream. Never treat a
		// shrinking thread as new activity.
		{"shrank", 8, 10, 0, false},

		// Min-comments waterline still applies to growth: a thread crawling
		// below the threshold stays frozen even though it gained a reply.
		{"grew but below threshold", 4, 2, 5, false},
		{"grew and exactly meets threshold", 5, 2, 5, true},
		{"grew one under threshold", 4, 3, 5, false},

		// No baseline (last < 0 ⇒ never L3-fetched, or a pre-num_comments run):
		// rumination measures growth against a prior fetch, so without one there
		// is nothing to compare and we must not ruminate.
		{"no baseline", 100, -1, 0, false},
		{"no baseline even over threshold", 100, -1, 5, false},

		// Zero-comment edge: a baseline of 0 (we fetched when empty) that now has
		// replies is legitimate growth.
		{"from zero baseline", 3, 0, 0, true},
		{"from zero baseline below threshold", 3, 0, 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRuminate(tc.current, tc.last, tc.minComments); got != tc.want {
				t.Errorf("shouldRuminate(current=%d, last=%d, min=%d) = %v, want %v",
					tc.current, tc.last, tc.minComments, got, tc.want)
			}
		})
	}
}

// ruminateCandidate is a tiny helper to build a store.L3RuminateCandidate the
// wave's seam will hand to ruminateL3.
func ruminateCandidate(id string, current, last int) store.L3RuminateCandidate {
	return store.L3RuminateCandidate{
		Post: &store.StoredPost{
			URLPath:   "/r/selfhosted/comments/" + id + "/title/",
			PostID:    id,
			Subreddit: "selfhosted",
		},
		CurrentComments: current,
		LastComments:    last,
	}
}

// withRuminate attaches a fixed ruminate batch to a wave scheduler built by
// newL3WaveScheduler, so a test can drive the post-media rumination sweep
// without a DB.
func withRuminate(s *Scheduler, cands []store.L3RuminateCandidate) {
	s.l3RuminateFn = func(_ /*sub*/, _ /*cycleID*/ string, _ /*limit*/, _ /*min*/ int) ([]store.L3RuminateCandidate, error) {
		return cands, nil
	}
}

// TestRunL2Wave_RuminatesGrownPost is the core of the feature and the direct
// answer to "相同L2跳过L3会不会跳过": a post whose media is already done is
// invisible to the L2 media loop (L2 skips it), yet because its comment count
// grew since the last archive the rumination sweep re-admits it to L3.
func TestRunL2Wave_RuminatesGrownPost(t *testing.T) {
	// No pending media at all — the steady state where the sub's backlog is
	// fully media-done and only rumination can drive new L3 work.
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "", nil)
	withRuminate(s, []store.L3RuminateCandidate{ruminateCandidate("grown1", 13, 10)})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	if len(got) != 1 || got[0] != "grown1" {
		t.Errorf("L3 fetched %v, want [grown1] (grown thread must be ruminated even when L2 skips it)", got)
	}
}

// TestRunL2Wave_RuminateSkipsUnchanged guards the gate: a candidate whose count
// did not actually grow (e.g. a stale or over-broad query row) is dropped by
// the shouldRuminate re-check and never burns a fetch.
func TestRunL2Wave_RuminateSkipsUnchanged(t *testing.T) {
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "", nil)
	withRuminate(s, []store.L3RuminateCandidate{
		ruminateCandidate("same1", 13, 13),  // no growth
		ruminateCandidate("shrank1", 8, 10), // negative growth
		ruminateCandidate("grown1", 14, 10), // real growth → only this one
	})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	if len(got) != 1 || got[0] != "grown1" {
		t.Errorf("L3 fetched %v, want [grown1] (unchanged/shrank candidates must be skipped)", got)
	}
}

// TestRunL2Wave_RuminateRespectsMinComments confirms the waterline still gates
// rumination: a grown-but-tiny thread is skipped, a grown thread over the line
// is fetched.
func TestRunL2Wave_RuminateRespectsMinComments(t *testing.T) {
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "5", nil)
	withRuminate(s, []store.L3RuminateCandidate{
		ruminateCandidate("tiny", 4, 2),  // grew but < 5 → skip
		ruminateCandidate("big", 12, 8),  // grew and ≥ 5 → fetch
		ruminateCandidate("exact", 5, 3), // grew to exactly 5 → fetch
	})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	want := []string{"big", "exact"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("L3 fetched %v, want %v (min-comments waterline must gate rumination)", got, want)
	}
}

// TestRunL2Wave_RuminateAfterMediaDrain confirms the sweep runs alongside, and
// after, the normal media wave: a pending media post is bound-fetched AND a
// separate grown post is ruminated in the same wave.
func TestRunL2Wave_RuminateAfterMediaDrain(t *testing.T) {
	pending := []*store.StoredPost{l3TestPost(t, "media1", true, 8)}
	s, rec, dl := newL3WaveScheduler(t, "l2+l3", "", pending)
	withRuminate(s, []store.L3RuminateCandidate{ruminateCandidate("grown1", 13, 10)})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	got := rec.got()
	want := []string{"grown1", "media1"} // sorted
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("L3 fetched %v, want %v (both the bind media post and the ruminated post)", got, want)
	}
	if calls := dl.getCalls(); len(calls) != 1 {
		t.Errorf("media downloads = %v, want exactly 1 (the media post only)", calls)
	}
}

// TestRunL2Wave_NonBindNeverRuminates pins that rumination is a bind-mode
// (depth=l2+l3) behaviour only: under depth=l2 even a grown candidate is left
// untouched, matching the contract that depth=l2 never fetches comments.
func TestRunL2Wave_NonBindNeverRuminates(t *testing.T) {
	s, rec, _ := newL3WaveScheduler(t, "l2", "", nil)
	withRuminate(s, []store.L3RuminateCandidate{ruminateCandidate("grown1", 13, 10)})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 0 {
		t.Errorf("depth=l2 ruminated %v, want none (rumination is bind-only)", got)
	}
}

// TestRunL2Wave_RuminateNoBaselineSkipped confirms a candidate that slipped
// through without a baseline (last < 0) is dropped by the gate even though its
// current count is large.
func TestRunL2Wave_RuminateNoBaselineSkipped(t *testing.T) {
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "", nil)
	withRuminate(s, []store.L3RuminateCandidate{ruminateCandidate("nobase", 99, -1)})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 0 {
		t.Errorf("baseline-less candidate ruminated %v, want none", got)
	}
}

// TestRuminateL3_NilPostSkipped guards the defensive nil-Post branch: a
// malformed candidate must not panic the sweep.
func TestRuminateL3_NilPostSkipped(t *testing.T) {
	s, rec, _ := newL3WaveScheduler(t, "l2+l3", "", nil)
	withRuminate(s, []store.L3RuminateCandidate{
		{Post: nil, CurrentComments: 13, LastComments: 10},
		ruminateCandidate("grown1", 13, 10),
	})

	if err := s.runL2Wave(context.Background(), "day", "selfhosted", 25, "cycle:1", 1); err != nil {
		t.Fatalf("runL2Wave returned error: %v", err)
	}

	if got := rec.got(); len(got) != 1 || got[0] != "grown1" {
		t.Errorf("L3 fetched %v, want [grown1] (nil-Post candidate must be skipped, not panic)", got)
	}
}
