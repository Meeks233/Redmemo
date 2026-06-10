package prefetch

import "testing"

// TestResolveSubDepth pins the resolution priority and the canonicalisation
// behaviour of the new NP depth knob:
//   per-sub override `depth:` > global prefetch_default_depth > fallback "l2+l3"
//
// All four canonical values must round-trip, depth:l2+l3 must survive the
// shared `+`-splitting clause grammar via splitTopLevelClauses, and the
// `depth:none` override on top of a global `l2+l3` default must opt the
// single sub out (the inverse of the docs' golang opt-in example).
func TestResolveSubDepth(t *testing.T) {
	tests := []struct {
		name string
		ms   map[string]string
		sub  string
		want string
	}{
		{"nil-style empty settings → fallback l2+l3", map[string]string{}, "golang", "l2+l3"},
		{"global default none", map[string]string{"prefetch_default_depth": "none"}, "golang", "none"},
		{"global default l2", map[string]string{"prefetch_default_depth": "l2"}, "golang", "l2"},
		{"global default l3", map[string]string{"prefetch_default_depth": "l3"}, "golang", "l3"},
		{"global default l2+l3", map[string]string{"prefetch_default_depth": "l2+l3"}, "golang", "l2+l3"},
		{"global default invalid → fallback l2+l3", map[string]string{"prefetch_default_depth": "garbage"}, "golang", "l2+l3"},
		{"global default L1 alias → none", map[string]string{"prefetch_default_depth": "l1"}, "golang", "none"},
		{"per-sub depth:none on default=l2+l3 opts out", map[string]string{
			"prefetch_default_depth": "l2+l3",
			"prefetch_sub_modes":     "golang=depth:none",
		}, "golang", "none"},
		{"per-sub depth:l2+l3 on default=none opts in (the user's golang example)", map[string]string{
			"prefetch_default_depth": "none",
			"prefetch_sub_modes":     "golang=depth:l2+l3&sort:top",
		}, "golang", "l2+l3"},
		{"unmatched sub falls back to global default", map[string]string{
			"prefetch_default_depth": "l2",
			"prefetch_sub_modes":     "golang=depth:l3",
		}, "rust", "l2"},
		{"per-sub depth survives mixed clauses with other subs", map[string]string{
			"prefetch_default_depth": "none",
			"prefetch_sub_modes":     "golang=depth:l2+l3+golang=sort:top+rust=depth:l3",
		}, "rust", "l3"},
		{"per-sub depth alias 'd'", map[string]string{
			"prefetch_default_depth": "none",
			"prefetch_sub_modes":     "golang=d:l2+l3",
		}, "golang", "l2+l3"},
		{"per-sub depth invalid falls back to global", map[string]string{
			"prefetch_default_depth": "l2",
			"prefetch_sub_modes":     "golang=depth:l4",
		}, "golang", "l2"},
		{"per-sub depth case/order normalised", map[string]string{
			"prefetch_sub_modes": "golang=depth:L3+L2",
		}, "golang", "l2+l3"},
		{"sub name case-insensitive match", map[string]string{
			"prefetch_sub_modes": "Golang=depth:l3",
		}, "GOLANG", "l3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Scheduler{settings: &mockSettings{data: tt.ms}}
			if got := s.resolveSubDepth(tt.sub); got != tt.want {
				t.Errorf("resolveSubDepth(%q)=%q, want %q", tt.sub, got, tt.want)
			}
		})
	}
}

// TestSplitTopLevelClauses pins the `+`-inside-depth-value protection. A
// `+` between `depth:l2` and `l3` must be treated as part of the depth
// value, not a clause separator; every other `+` still splits as before.
func TestSplitTopLevelClauses(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"golang", []string{"golang"}},
		{"golang+rust", []string{"golang", "rust"}},
		{"golang=depth:l2+l3", []string{"golang=depth:l2+l3"}},
		{"golang=depth:l3+l2", []string{"golang=depth:l3+l2"}},
		{"golang=depth:l2+l3+rust=sort:top", []string{"golang=depth:l2+l3", "rust=sort:top"}},
		{"golang=sort:top&depth:l2+l3+rust=depth:none", []string{"golang=sort:top&depth:l2+l3", "rust=depth:none"}},
		{"golang=depth:l2+l3&sort:top+rust", []string{"golang=depth:l2+l3&sort:top", "rust"}},
		// upper/lowercase + `d:` alias variants
		{"golang=d:L2+L3+rust", []string{"golang=d:L2+L3", "rust"}},
		// a depth value without a sibling l2/l3 partner stays split (not depth syntax)
		{"golang=depth:l2+sort:top", []string{"golang=depth:l2", "sort:top"}},
	}
	for _, c := range cases {
		got := splitTopLevelClauses(c.in)
		if len(got) != len(c.want) {
			t.Errorf("split(%q)=%v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("split(%q)[%d]=%q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
