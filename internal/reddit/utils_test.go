package reddit

import (
	"strings"
	"testing"
	"time"
)

func TestFormatNum(t *testing.T) {
	tests := []struct {
		n           int64
		wantDisplay string
		wantRaw     string
	}{
		{0, "0", "0"},
		{42, "42", "42"},
		{999, "999", "999"},
		{1000, "1.0k", "1000"},
		{1234, "1.2k", "1234"},
		{10000, "10.0k", "10000"},
		{999999, "1000.0k", "999999"},
		{1000000, "1.0m", "1000000"},
		{1234567, "1.2m", "1234567"},
		{-1234, "-1.2k", "-1234"},
		{-1000000, "-1.0m", "-1000000"},
	}
	for _, tt := range tests {
		result := FormatNum(tt.n)
		if result[0] != tt.wantDisplay {
			t.Errorf("FormatNum(%d)[0] = %q, want %q", tt.n, result[0], tt.wantDisplay)
		}
		if result[1] != tt.wantRaw {
			t.Errorf("FormatNum(%d)[1] = %q, want %q", tt.n, result[1], tt.wantRaw)
		}
	}
}

func TestFormatTime(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name        string
		created     float64
		wantContain string
	}{
		{"minutes ago", float64(now.Add(-5 * time.Minute).Unix()), "m ago"},
		{"hours ago", float64(now.Add(-3 * time.Hour).Unix()), "h ago"},
		{"days ago", float64(now.Add(-2 * 24 * time.Hour).Unix()), "d ago"},
		{"old date", float64(now.Add(-365 * 24 * time.Hour).Unix()), "'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rel, abs := FormatTime(tt.created)
			if !strings.Contains(rel, tt.wantContain) {
				t.Errorf("FormatTime rel = %q, want to contain %q", rel, tt.wantContain)
			}
			if abs == "" {
				t.Error("FormatTime abs should not be empty")
			}
			if !strings.Contains(abs, "UTC") {
				t.Errorf("FormatTime abs = %q, should contain UTC", abs)
			}
		})
	}
}

func TestCapitalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"a", "A"},
		{"hello", "Hello"},
		{"Hello", "Hello"},
		{"hello world", "Hello world"},
		{"über", "Über"},
	}
	for _, tt := range tests {
		got := Capitalize(tt.input)
		if got != tt.want {
			t.Errorf("Capitalize(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseParentID(t *testing.T) {
	tests := []struct {
		input    string
		wantKind string
		wantID   string
	}{
		{"t1_abc123", "t1", "abc123"},
		{"t3_xyz789", "t3", "xyz789"},
		{"nounderscore", "", "nounderscore"},
		{"", "", ""},
	}
	for _, tt := range tests {
		kind, id := ParseParentID(tt.input)
		if kind != tt.wantKind || id != tt.wantID {
			t.Errorf("ParseParentID(%q) = (%q, %q), want (%q, %q)", tt.input, kind, id, tt.wantKind, tt.wantID)
		}
	}
}

func TestFilterPosts(t *testing.T) {
	posts := []Post{
		{Community: "golang", Author: Author{Name: "alice"}},
		{Community: "rust", Author: Author{Name: "bob"}},
		{Community: "python", Author: Author{Name: "charlie"}},
	}
	filters := map[string]bool{"rust": true}
	count, allFiltered := FilterPosts(&posts, filters)
	if count != 1 {
		t.Errorf("filtered count = %d, want 1", count)
	}
	if allFiltered {
		t.Error("allFiltered should be false")
	}
	if len(posts) != 2 {
		t.Errorf("remaining posts = %d, want 2", len(posts))
	}
}
