package reddit

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// FormatNum formats a number for human display.
// Returns [formatted, raw]: 1234 → ["1.2k", "1234"], 1234567 → ["1.2m", "1234567"].
func FormatNum(n int64) [2]string {
	raw := strconv.FormatInt(n, 10)
	var display string
	switch {
	case n >= 1_000_000 || n <= -1_000_000:
		display = fmt.Sprintf("%.1fm", float64(n)/1_000_000.0)
	case n >= 1000 || n <= -1000:
		display = fmt.Sprintf("%.1fk", float64(n)/1_000.0)
	default:
		display = raw
	}
	return [2]string{display, raw}
}

// FormatTime converts a Unix timestamp (float64) into a relative time string
// and an absolute UTC time string.
func FormatTime(created float64) (string, string) {
	t := time.Unix(int64(math.Round(created)), 0).UTC()
	now := time.Now().UTC()
	delta := now.Sub(t)
	future := delta < 0
	if future {
		delta = -delta
	}

	var rel string
	switch {
	case delta > 30*24*time.Hour:
		rel = t.Format("Jan 02 '06")
	case delta >= 24*time.Hour:
		rel = fmt.Sprintf("%dd", int(delta.Hours()/24))
	case delta >= time.Hour:
		rel = fmt.Sprintf("%dh", int(delta.Hours()))
	default:
		rel = fmt.Sprintf("%dm", int(delta.Minutes()))
	}

	if delta <= 30*24*time.Hour {
		if future {
			rel += " left"
		} else {
			rel += " ago"
		}
	}

	abs := t.Format("Jan 02 2006, 15:04:05 UTC")
	return rel, abs
}

// Capitalize returns s with its first Unicode letter uppercased.
func Capitalize(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// FilterPosts removes posts whose community or author (u_name) is in filters.
// It modifies the slice in place and returns (filtered count, all filtered).
func FilterPosts(posts *[]Post, filters map[string]bool) (uint64, bool) {
	before := len(*posts)
	result := (*posts)[:0]
	for _, p := range *posts {
		if !filters[p.Community] && !filters["u_"+p.Author.Name] {
			result = append(result, p)
		}
	}
	*posts = result
	return uint64(before - len(result)), len(result) == 0
}

// ParseParentID splits a Reddit parent_id like "t1_xxx" into (kind, id).
func ParseParentID(parentID string) (kind, id string) {
	parts := strings.SplitN(parentID, "_", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", parentID
}
