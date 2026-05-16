package render

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed locales/*.json
var localeFS embed.FS

// SupportedLangs lists the UI languages RedMemo ships, in preference order.
// The first entry is also the DefaultLang. Cookie/Accept-Language resolution
// always collapses to one of these values so HTML cache keys stay discrete.
var SupportedLangs = []string{"en", "zh"}

// DefaultLang is the fallback language used when no preference is known and
// when a translation key is missing from a non-default locale.
const DefaultLang = "en"

// htmlLangCodes maps an internal lang code to the value emitted in
// <html lang="...">. Defaults to the lang code itself when absent.
var htmlLangCodes = map[string]string{
	"en": "en",
	"zh": "zh-CN",
}

// Locale is a flat key -> translated-string map for one language.
type Locale map[string]string

// loadLocales parses every embedded locales/*.json file. It fails fast: a
// malformed file or a missing default locale aborts startup rather than
// silently degrading the UI.
func loadLocales() (map[string]Locale, error) {
	out := make(map[string]Locale, len(SupportedLangs))
	for _, lang := range SupportedLangs {
		data, err := localeFS.ReadFile("locales/" + lang + ".json")
		if err != nil {
			return nil, fmt.Errorf("read locale %s: %w", lang, err)
		}
		var loc Locale
		if err := json.Unmarshal(data, &loc); err != nil {
			return nil, fmt.Errorf("parse locale %s: %w", lang, err)
		}
		out[lang] = loc
	}
	if _, ok := out[DefaultLang]; !ok {
		return nil, fmt.Errorf("default locale %q not loaded", DefaultLang)
	}
	return out, nil
}

// translator returns the template `T` function bound to loc. Lookups fall back
// to the default locale and finally to the key itself, so a half-translated
// locale never breaks a page. When args are supplied the stored string is
// treated as an fmt format string.
func translator(loc, defaultLoc Locale) func(key string, args ...any) string {
	return func(key string, args ...any) string {
		s, ok := loc[key]
		if !ok || s == "" {
			if s, ok = defaultLoc[key]; !ok || s == "" {
				s = key
			}
		}
		if len(args) > 0 {
			return fmt.Sprintf(s, args...)
		}
		return s
	}
}

// htmlLang returns the <html lang="..."> value for an internal lang code.
func htmlLang(lang string) string {
	if v, ok := htmlLangCodes[lang]; ok {
		return v
	}
	return lang
}

// isSupportedLang reports whether lang is one of SupportedLangs.
func isSupportedLang(lang string) bool {
	for _, l := range SupportedLangs {
		if l == lang {
			return true
		}
	}
	return false
}

// ResolveLang picks a UI language. A non-empty, supported cookie value wins.
// Otherwise the Accept-Language header is scanned for the first tag whose
// primary subtag is supported. Failing both, DefaultLang is returned. The
// result is always one of SupportedLangs.
func ResolveLang(cookieVal, acceptLanguage string) string {
	if cookieVal != "" && isSupportedLang(cookieVal) {
		return cookieVal
	}
	for _, part := range strings.Split(acceptLanguage, ",") {
		tag := strings.TrimSpace(part)
		if i := strings.IndexByte(tag, ';'); i >= 0 {
			tag = tag[:i]
		}
		if i := strings.IndexByte(tag, '-'); i >= 0 {
			tag = tag[:i]
		}
		tag = strings.ToLower(strings.TrimSpace(tag))
		if isSupportedLang(tag) {
			return tag
		}
	}
	return DefaultLang
}
