package archive

import "testing"

func TestParseControl(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		canonical string
		allow     map[string]bool
	}{
		{
			name:      "empty allows everything",
			raw:       "",
			canonical: "",
			allow: map[string]bool{
				"anything": true, "golang": true,
			},
		},
		{
			name:      "pure whitelist",
			raw:       "suba+subb",
			canonical: "suba+subb",
			allow: map[string]bool{
				"suba": true, "subb": true, "subc": false,
			},
		},
		{
			name:      "pure blacklist",
			raw:       "-suba-subb",
			canonical: "-suba-subb",
			allow: map[string]bool{
				"suba": false, "subb": false, "subc": true,
			},
		},
		{
			name:      "mixed: any include drops all excludes",
			raw:       "suba+subb-subc",
			canonical: "suba+subb",
			allow: map[string]bool{
				"suba": true, "subb": true,
				// '-subc' is discarded once a '+' is present, but the active
				// whitelist still excludes subc by virtue of not listing it.
				"subc": false, "subd": false,
			},
		},
		{
			name:      "duplicate in whitelist drops that name entirely",
			raw:       "suba+suba+subb",
			canonical: "subb",
			allow: map[string]bool{
				"suba": false, // dropped — whitelist mode still active
				"subb": true,
				"subc": false,
			},
		},
		{
			name:      "duplicate same name across signs drops both",
			raw:       "suba-suba",
			canonical: "",
			allow: map[string]bool{
				"suba": true, // both occurrences dropped → no filter
				"subc": true,
			},
		},
		{
			name:      "all-dup blacklist collapses to empty filter",
			raw:       "-suba-suba",
			canonical: "",
			allow: map[string]bool{
				"suba": true, "subc": true,
			},
		},
		{
			name:      "r/ prefix and case stripped",
			raw:       "r/Foo+R/Bar",
			canonical: "bar+foo",
			allow: map[string]bool{
				"foo": true, "BAR": true, "baz": false,
			},
		},
		{
			name:      "whitespace separated tokens work like + glue",
			raw:       "  suba   subb  -subc ",
			canonical: "suba+subb",
			allow: map[string]bool{
				"suba": true, "subb": true, "subc": false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ParseControl(tc.raw)
			if got := c.Canonical(); got != tc.canonical {
				t.Errorf("Canonical(%q) = %q, want %q", tc.raw, got, tc.canonical)
			}
			for sub, want := range tc.allow {
				if got := c.Allow(sub); got != want {
					t.Errorf("Allow(%q) on %q = %v, want %v", sub, tc.raw, got, want)
				}
			}
		})
	}
}

func TestControlNilAllowsAll(t *testing.T) {
	var c *Control
	if !c.Allow("anything") {
		t.Fatal("nil *Control should allow everything")
	}
}
