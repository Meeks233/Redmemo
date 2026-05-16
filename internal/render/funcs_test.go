package render

import (
	"html/template"
	"testing"
)

func TestSafe(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	safeFn := fns["safe"].(func(string) template.HTML)
	got := safeFn("<b>bold</b>")
	if got != "<b>bold</b>" {
		t.Errorf("safe() = %q, want %q", got, "<b>bold</b>")
	}
}

func TestSafeAttr(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["safeAttr"].(func(string) template.HTMLAttr)
	got := fn(`class="active"`)
	if got != `class="active"` {
		t.Errorf("safeAttr() = %q, want %q", got, `class="active"`)
	}
}

func TestSafeURL(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["safeURL"].(func(string) template.URL)
	got := fn("https://example.com")
	if got != "https://example.com" {
		t.Errorf("safeURL() = %q, want %q", got, "https://example.com")
	}
}

func TestAdd(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["add"].(func(int, int) int)
	tests := []struct{ a, b, want int }{
		{1, 2, 3},
		{0, 0, 0},
		{-1, 1, 0},
		{100, 200, 300},
	}
	for _, tc := range tests {
		if got := fn(tc.a, tc.b); got != tc.want {
			t.Errorf("add(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSub(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["sub"].(func(int, int) int)
	if got := fn(10, 3); got != 7 {
		t.Errorf("sub(10, 3) = %d, want 7", got)
	}
}

func TestMul(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["mul"].(func(int, int) int)
	if got := fn(4, 5); got != 20 {
		t.Errorf("mul(4, 5) = %d, want 20", got)
	}
}

func TestSlice(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["slice"].(func(string, int, int) string)

	tests := []struct {
		s          string
		start, end int
		want       string
	}{
		{"hello", 0, 5, "hello"},
		{"hello", 1, 3, "el"},
		{"hello", 0, 0, ""},
		{"hello", -1, 3, "hel"},
		{"hello", 0, 100, "hello"},
		{"hello", 5, 3, ""},
	}
	for _, tc := range tests {
		got := fn(tc.s, tc.start, tc.end)
		if got != tc.want {
			t.Errorf("slice(%q, %d, %d) = %q, want %q", tc.s, tc.start, tc.end, got, tc.want)
		}
	}
}

func TestConcat(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["concat"].(func(...string) string)

	if got := fn("a", "b", "c"); got != "abc" {
		t.Errorf("concat(a,b,c) = %q, want %q", got, "abc")
	}
	if got := fn(); got != "" {
		t.Errorf("concat() = %q, want empty", got)
	}
}

func TestCommentsWord(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["commentsWord"].(func(string) string)

	if got := fn("1"); got != "comment" {
		t.Errorf("commentsWord(1) = %q, want %q", got, "comment")
	}
	if got := fn("5"); got != "comments" {
		t.Errorf("commentsWord(5) = %q, want %q", got, "comments")
	}
	if got := fn("0"); got != "comments" {
		t.Errorf("commentsWord(0) = %q, want %q", got, "comments")
	}
}

func TestFeedPath(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["feedPath"].(func(string) string)

	if got := fn("golang"); got != "r/golang" {
		t.Errorf("feedPath(golang) = %q, want %q", got, "r/golang")
	}
	if got := fn("u_someuser"); got != "u/someuser" {
		t.Errorf("feedPath(u_someuser) = %q, want %q", got, "u/someuser")
	}
}

func TestCommunityPath(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["communityPath"].(func(string) string)

	if got := fn("pics"); got != "r/pics" {
		t.Errorf("communityPath(pics) = %q, want %q", got, "r/pics")
	}
	if got := fn("u_user123"); got != "u/user123" {
		t.Errorf("communityPath(u_user123) = %q, want %q", got, "u/user123")
	}
}

func TestDict(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["dict"].(func(...any) map[string]any)

	m := fn("key1", "val1", "key2", 42)
	if m["key1"] != "val1" {
		t.Errorf("dict key1 = %v, want val1", m["key1"])
	}
	if m["key2"] != 42 {
		t.Errorf("dict key2 = %v, want 42", m["key2"])
	}

	empty := fn()
	if len(empty) != 0 {
		t.Errorf("dict() should be empty, got %d entries", len(empty))
	}

	// Odd number of args — last value ignored
	odd := fn("k1", "v1", "k2")
	if len(odd) != 1 {
		t.Errorf("dict with odd args should have 1 entry, got %d", len(odd))
	}
}

func TestList(t *testing.T) {
	fns := templateFuncs(Locale{}, Locale{}, "en")
	fn := fns["list"].(func(...string) []string)

	got := fn("a", "b", "c")
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("list(a,b,c) = %v", got)
	}

	empty := fn()
	if len(empty) != 0 {
		t.Errorf("list() should be empty, got %v", empty)
	}
}
