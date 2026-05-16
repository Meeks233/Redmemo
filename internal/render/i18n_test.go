package render

import "testing"

func TestResolveLang(t *testing.T) {
	tests := []struct {
		name           string
		cookie, accept string
		want           string
	}{
		{"cookie wins", "zh", "en-US,en", "zh"},
		{"unsupported cookie falls through", "fr", "en-US", "en"},
		{"empty cookie uses accept-language", "", "zh-CN,zh;q=0.9,en;q=0.8", "zh"},
		{"accept-language primary subtag", "", "en-GB", "en"},
		{"no signal falls back to default", "", "", DefaultLang},
		{"unsupported accept-language falls back", "", "fr-FR,de;q=0.8", DefaultLang},
		{"whitespace tolerated", "", " zh ", "zh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveLang(tc.cookie, tc.accept); got != tc.want {
				t.Errorf("ResolveLang(%q, %q) = %q, want %q", tc.cookie, tc.accept, got, tc.want)
			}
		})
	}
}

func TestTranslatorFallback(t *testing.T) {
	def := Locale{"greeting": "Hello", "count": "%d items"}
	loc := Locale{"greeting": "你好"}
	tr := translator(loc, def)

	if got := tr("greeting"); got != "你好" {
		t.Errorf("present key = %q, want 你好", got)
	}
	// Missing in loc, present in default — fall back to default.
	if got := tr("count", 3); got != "3 items" {
		t.Errorf("default fallback = %q, want \"3 items\"", got)
	}
	// Missing everywhere — fall back to the key itself.
	if got := tr("nonexistent"); got != "nonexistent" {
		t.Errorf("missing key = %q, want \"nonexistent\"", got)
	}
}

func TestLoadLocales(t *testing.T) {
	locales, err := loadLocales()
	if err != nil {
		t.Fatalf("loadLocales() error: %v", err)
	}
	for _, lang := range SupportedLangs {
		if len(locales[lang]) == 0 {
			t.Errorf("locale %q is empty", lang)
		}
	}
	// Every key in the default locale should also exist in every other
	// locale, so no string silently falls back at runtime.
	def := locales[DefaultLang]
	for _, lang := range SupportedLangs {
		if lang == DefaultLang {
			continue
		}
		for key := range def {
			if _, ok := locales[lang][key]; !ok {
				t.Errorf("locale %q missing key %q", lang, key)
			}
		}
	}
}
