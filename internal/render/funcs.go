package render

import (
	"html/template"
	"strings"

	"github.com/redmemo/redmemo/internal/reddit"
)

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"capitalize": reddit.Capitalize,
		"contains":   strings.Contains,
		"hasPrefix":  strings.HasPrefix,
		"hasSuffix":  strings.HasSuffix,
		"join":       strings.Join,
		"toLower":    strings.ToLower,
		"toUpper":    strings.ToUpper,
		"trimPrefix": strings.TrimPrefix,
		"safe":       func(s string) template.HTML { return template.HTML(s) },
		"safeAttr":   func(s string) template.HTMLAttr { return template.HTMLAttr(s) },
		"safeURL":    func(s string) template.URL { return template.URL(s) },
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"mul":        func(a, b int) int { return a * b },
		"formatNum":  reddit.FormatNum,
		"slice": func(s string, start, end int) string {
			if start < 0 {
				start = 0
			}
			if end > len(s) {
				end = len(s)
			}
			if start >= end {
				return ""
			}
			return s[start:end]
		},
		"concat": func(parts ...string) string {
			return strings.Join(parts, "")
		},
		"commentsWord": func(raw string) string {
			if raw == "1" {
				return "comment"
			}
			return "comments"
		},
		"feedPath": func(name string) string {
			if strings.HasPrefix(name, "u_") {
				return "u/" + name[2:]
			}
			return "r/" + name
		},
		"communityPath": func(name string) string {
			if strings.HasPrefix(name, "u_") {
				return "u/" + name[2:]
			}
			return "r/" + name
		},
		"dict": func(pairs ...any) map[string]any {
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i+1 < len(pairs); i += 2 {
				if k, ok := pairs[i].(string); ok {
					m[k] = pairs[i+1]
				}
			}
			return m
		},
		"list": func(items ...string) []string {
			return items
		},
		"split": strings.Split,
		"gtf": func(a, b float64) bool { return a > b },
		"lef": func(a, b float64) bool { return a > 0 && a <= b },
		"inList": func(item, delimited string) bool {
			for _, s := range strings.Split(delimited, "+") {
				if strings.EqualFold(s, item) {
					return true
				}
			}
			return false
		},
	}
}
