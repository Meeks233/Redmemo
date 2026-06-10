package handler

import (
	"reflect"
	"strings"
	"testing"
)

// TestCanonicalizeDepth pins the case/alias/whitespace contract. Operators may
// hand-type any of these; the normaliser must collapse them to the four
// canonical values (or refuse) before they reach the scheduler.
func TestCanonicalizeDepth(t *testing.T) {
	tests := []struct {
		in       string
		want     string
		wantOK   bool
	}{
		{"l2+l3", "l2+l3", true},
		{"L2+L3", "l2+l3", true},
		{"l3+l2", "l2+l3", true}, // reversed order accepted
		{" l2 + l3 ", "l2+l3", true},
		{"l2", "l2", true},
		{"L2", "l2", true},
		{"l3", "l3", true},
		{"none", "none", true},
		{"None", "none", true},
		{"off", "none", true},
		{"l1", "none", true}, // L1-only alias
		{"", "none", false},  // empty: collapses to canonical "none" but reports !ok so callers can choose to fall back
		{"l4", "", false},
		{"l2+l2", "", false},
		{"hot", "", false},
		{"l2+", "", false},
	}
	for _, tt := range tests {
		got, ok := CanonicalizeDepth(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("CanonicalizeDepth(%q) = (%q, %v); want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestDepthPredicates is a paranoia check that DepthHasL2/L3 agree with the
// canonical depth table from the docs — any future renaming of canonical
// values must update both predicates and this test together.
func TestDepthPredicates(t *testing.T) {
	cases := []struct {
		depth string
		l2    bool
		l3    bool
	}{
		{"none", false, false},
		{"l2", true, false},
		{"l3", false, true},
		{"l2+l3", true, true},
		{"", false, false},
		{"garbage", false, false},
	}
	for _, c := range cases {
		if got := DepthHasL2(c.depth); got != c.l2 {
			t.Errorf("DepthHasL2(%q) = %v, want %v", c.depth, got, c.l2)
		}
		if got := DepthHasL3(c.depth); got != c.l3 {
			t.Errorf("DepthHasL3(%q) = %v, want %v", c.depth, got, c.l3)
		}
	}
}

// TestParsePrefetchSubModes_Depth exercises the per-sub depth override path:
// valid values, aliases, mixed sort+time+depth clauses, multiple subs, depth
// override of "none" alongside a "l2+l3" default (the user's golang example),
// and bogus depth values that must drop only the depth fragment without
// nuking the clause.
func TestParsePrefetchSubModes_Depth(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      []PrefetchSubOverride
		wantRejN  int // number of rejected fragments (we don't pin the exact reason strings to keep the test ergonomic)
	}{
		{
			name: "single sub, depth only",
			in:   "golang=depth:l2+l3",
			want: []PrefetchSubOverride{{Sub: "golang", Depth: "l2+l3"}},
		},
		{
			name: "single sub, sort+time+depth",
			in:   "golang=sort:top&time:week&depth:l2+l3",
			want: []PrefetchSubOverride{{Sub: "golang", Sort: "top", Timeframe: "week", Depth: "l2+l3"}},
		},
		{
			name: "depth alias 'd'",
			in:   "golang=d:l3",
			want: []PrefetchSubOverride{{Sub: "golang", Depth: "l3"}},
		},
		{
			name: "depth:none overrides global default-on for a single sub",
			in:   "boring=depth:none",
			want: []PrefetchSubOverride{{Sub: "boring", Depth: "none"}},
		},
		{
			name: "depth canonical even when written reversed/uppercase",
			in:   "golang=depth:L3+L2",
			want: []PrefetchSubOverride{{Sub: "golang", Depth: "l2+l3"}},
		},
		{
			name: "multiple subs with mixed depth",
			in:   "golang=depth:l2+l3&sort:top+golang=depth:none+rust=sort:new",
			want: []PrefetchSubOverride{
				{Sub: "golang", Sort: "top", Depth: "l2+l3"},
				{Sub: "golang", Depth: "none"},
				{Sub: "rust", Sort: "new"},
			},
		},
		{
			name: "invalid depth value drops only the depth fragment",
			in:   "golang=sort:top&depth:l4",
			want: []PrefetchSubOverride{{Sub: "golang", Sort: "top"}},
			wantRejN: 1,
		},
		{
			name: "clause with ONLY a bogus depth has nothing usable -> entire clause dropped",
			in:   "golang=depth:l4",
			want: nil,
			wantRejN: 2, // one for depth:l4, one for "no usable sort/time overrides"
		},
		{
			name: "depth duplicated across clauses for same sub -> last write wins",
			in:   "golang=depth:l2+golang=depth:l3",
			want: []PrefetchSubOverride{{Sub: "golang", Depth: "l3"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bad := ParsePrefetchSubModes(tt.in)
			if len(got) == 0 && len(tt.want) == 0 {
				// Empty result; nil vs []T{} is just an allocator detail.
			} else if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
			if len(bad) != tt.wantRejN {
				t.Errorf("rejects=%d (%v), want %d", len(bad), bad, tt.wantRejN)
			}
		})
	}
}

// TestCanonicalPrefetchSubModes_Depth pins the echo-back order for depth: it
// follows sort and time in the canonical "k:v&k:v" form, so the textarea
// re-render is stable regardless of how the user keyed the original clause.
func TestCanonicalPrefetchSubModes_Depth(t *testing.T) {
	in := "golang=depth:l2+l3&time:week&sort:top"
	overrides, _ := ParsePrefetchSubModes(in)
	got := CanonicalPrefetchSubModes(overrides)
	want := "golang=sort:top&time:week&depth:l2+l3"
	if got != want {
		t.Errorf("CanonicalPrefetchSubModes = %q, want %q", got, want)
	}
}

// TestNormalizeSettings_PrefetchDefaultDepth pins the validation contract:
// canonical values pass through (possibly normalised), garbage is rejected
// AND reported, and the key is dropped on reject so the existing stored
// default stands. Mirrors the surrounding NormalizeSettings_* tests' style.
func TestNormalizeSettings_PrefetchDefaultDepth(t *testing.T) {
	tests := []struct {
		in          string
		want        string
		wantPresent bool
		wantReject  bool
	}{
		{"l2+l3", "l2+l3", true, false},
		{"L2+L3", "l2+l3", true, false},
		{"l3+l2", "l2+l3", true, false},
		{"l2", "l2", true, false},
		{"l3", "l3", true, false},
		{"none", "none", true, false},
		{"l1", "none", true, false}, // alias accepted
		{"off", "none", true, false},
		{"l4", "", false, true},
		{"hot", "", false, true},
	}
	for _, tt := range tests {
		updates := map[string]string{"prefetch_default_depth": tt.in}
		out, rej := NormalizeSettings(updates)
		got, present := out["prefetch_default_depth"]
		if present != tt.wantPresent {
			t.Errorf("input %q: present=%v want %v", tt.in, present, tt.wantPresent)
		}
		if got != tt.want {
			t.Errorf("input %q: got %q want %q", tt.in, got, tt.want)
		}
		hasRej := false
		for _, r := range rej {
			if r.Key == "prefetch_default_depth" {
				hasRej = true
				break
			}
		}
		if hasRej != tt.wantReject {
			t.Errorf("input %q: rejected=%v want %v", tt.in, hasRej, tt.wantReject)
		}
	}
}

// TestNormalizeSettings_PrefetchSubModes_Depth checks the integration path:
// the per-sub override raw string gets canonicalised, depth values are
// normalised, and the depth: token survives the round-trip. Anchors the
// public docs example (default=none + r/golang opt-in via depth:l2+l3).
func TestNormalizeSettings_PrefetchSubModes_Depth(t *testing.T) {
	updates := map[string]string{
		"prefetch_sub_modes": "golang=DEPTH:l3+L2&SORT:TOP+golang=depth:NoNe",
	}
	out, _ := NormalizeSettings(updates)
	got := out["prefetch_sub_modes"]
	if !strings.Contains(got, "golang=") || !strings.Contains(got, "depth:l2+l3") {
		t.Errorf("expected golang depth:l2+l3 in %q", got)
	}
	if !strings.Contains(got, "sort:top") {
		t.Errorf("expected sort:top in %q", got)
	}
	if !strings.Contains(got, "golang=depth:none") {
		t.Errorf("expected golang=depth:none in %q", got)
	}
}
