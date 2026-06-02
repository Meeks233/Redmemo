package archive

import (
	"strings"
	"sync/atomic"
)

// Control is the parsed Archive Control filter. It decides whether a given
// subreddit's posts are accepted into the archive based on a small grammar:
//
//   suba+subb        — include-only (whitelist): archive ONLY suba and subb
//   -suba-subb       — exclude-only (blacklist): archive everything except these
//   suba+subb-subc   — mixed: any '+' wins the round, so all '-' entries are
//                      discarded; the result is whitelist {suba, subb}
//   suba+suba        — a name appearing more than once is dropped entirely
//                      (not deduped), regardless of sign
//
// An empty parsed result means "no filter" — Allow returns true for every sub.
type Control struct {
	whites map[string]bool
	blacks map[string]bool
}

// ParseControl turns a raw query string into a Control. Whitespace, '+' and '-'
// separate names; a leading '-' marks the name as an exclude, anything else is
// an include. Names are lowercased and an `r/` prefix is stripped.
func ParseControl(raw string) *Control {
	c := &Control{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return c
	}

	type entry struct {
		name    string
		include bool
	}
	var entries []entry
	counts := make(map[string]int)

	// Tokens are split on whitespace; within each token, '+' and '-' bytes both
	// terminate a name AND signal the sign of the next name.
	for _, tok := range strings.Fields(raw) {
		for i, n := 0, len(tok); i < n; {
			include := true
			if tok[i] == '+' || tok[i] == '-' {
				include = tok[i] == '+'
				i++
			}
			start := i
			for i < n && tok[i] != '+' && tok[i] != '-' {
				i++
			}
			name := normalizeName(tok[start:i])
			if name == "" {
				continue
			}
			entries = append(entries, entry{name: name, include: include})
			counts[name]++
		}
	}

	whites := make(map[string]bool)
	blacks := make(map[string]bool)
	seenInclude := false
	for _, e := range entries {
		// 重名自动丢弃: a name appearing more than once is dropped entirely so
		// an ambiguous user input ("suba+suba", "suba-suba") yields neither
		// include nor exclude.
		if counts[e.name] > 1 {
			continue
		}
		if e.include {
			whites[e.name] = true
			seenInclude = true
		} else {
			blacks[e.name] = true
		}
	}

	// 只要有一个 '+'，所有 '-' 就全部失效丢弃
	if seenInclude {
		blacks = nil
	} else if len(blacks) == 0 {
		blacks = nil
	}
	if len(whites) == 0 {
		whites = nil
	}

	c.whites = whites
	c.blacks = blacks
	return c
}

// Allow reports whether the named sub passes this filter.
//
//   - whitelist active: only names in the whitelist pass
//   - blacklist active: every name passes except those in the blacklist
//   - neither active (empty filter): everything passes
func (c *Control) Allow(sub string) bool {
	if c == nil {
		return true
	}
	name := normalizeName(sub)
	if name == "" {
		return true
	}
	if len(c.whites) > 0 {
		return c.whites[name]
	}
	if len(c.blacks) > 0 {
		return !c.blacks[name]
	}
	return true
}

// IsEmpty reports whether the control has no effective rule (allows everything).
func (c *Control) IsEmpty() bool {
	return c == nil || (len(c.whites) == 0 && len(c.blacks) == 0)
}

// Canonical re-serializes the control into the storage form: includes first
// joined by '+', then any excludes each prefixed with '-'. Returns "" when the
// control has no effective rule.
func (c *Control) Canonical() string {
	if c.IsEmpty() {
		return ""
	}
	whites := sortedKeys(c.whites)
	blacks := sortedKeys(c.blacks)
	var b strings.Builder
	for i, s := range whites {
		if i > 0 {
			b.WriteByte('+')
		}
		b.WriteString(s)
	}
	for _, s := range blacks {
		b.WriteByte('-')
		b.WriteString(s)
	}
	return b.String()
}

func normalizeName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, "/r/")
	s = strings.TrimPrefix(s, "r/")
	return s
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Tiny insertion sort — no `sort` import needed for a handful of names.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// controlPtr is a tiny atomic holder so a running archive Service can hot-swap
// its Control whenever settings change, without any locking on the archive
// hot path.
type controlPtr struct {
	v atomic.Pointer[Control]
}

func (p *controlPtr) load() *Control { return p.v.Load() }
func (p *controlPtr) store(c *Control) { p.v.Store(c) }
