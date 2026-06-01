package handler

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/redmemo/redmemo/internal/render"
)

// archiveDescSubCap bounds how many sub names we list in the archive hub's
// <meta name=description>. Google truncates around 160 chars; 10 names keeps
// the snippet useful without overflowing.
const archiveDescSubCap = 10

// archiveJSONLDSubCap bounds the ItemList entries we emit on the archive hub.
// 50 covers a typical self-hosted instance comfortably and stays well under
// Google's 100-item ItemList soft limit.
const archiveJSONLDSubCap = 50

// sitemapEntryCap bounds how many archived-sub URLs we emit in sitemap.xml.
// 50k is the per-file URL cap in the sitemap protocol; our archive lists are
// far below that, but the cap protects against pathological databases.
const sitemapEntryCap = 50000

// canonicalHost returns the configured absolute origin for the instance with
// no trailing slash. Empty when SEO.CanonicalHost isn't set — in that case
// sitemap.xml emits root-relative <loc> URLs (legal per sitemap protocol when
// the sitemap is at the site root, which it is).
func (h *Handler) canonicalHost() string {
	host := strings.TrimSpace(h.cfg.SEO.CanonicalHost)
	host = strings.TrimRight(host, "/")
	return host
}

// indexingAllowed returns true when the instance owner has opted into search
// engine indexing. Off by default — the global robots meta stays noindex and
// /robots.txt is "Disallow: /" until the owner flips this.
func (h *Handler) indexingAllowed() bool {
	return h.cfg != nil && h.cfg.SEO.AllowIndexing
}

func (h *Handler) handleRobotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")

	if !h.indexingAllowed() {
		io.WriteString(w, "User-agent: *\nDisallow: /\n")
		return
	}

	var b strings.Builder
	b.WriteString("User-agent: *\n")
	// The archive surfaces are the only pages that describe THIS instance
	// (what subs it mirrors) — those go open to crawlers. Everything else is
	// either a Reddit duplicate (DMCA risk + thin content) or transient
	// machinery (settings, media proxies, JSON endpoints).
	b.WriteString("Allow: /archive\n")
	b.WriteString("Disallow: /r/\n")
	b.WriteString("Disallow: /user/\n")
	b.WriteString("Disallow: /search\n")
	b.WriteString("Disallow: /settings\n")
	b.WriteString("Disallow: /debug\n")
	b.WriteString("Disallow: /fuckreddit\n")
	b.WriteString("Disallow: /countdown\n")
	b.WriteString("Disallow: /api/\n")
	b.WriteString("Disallow: /proxy/\n")
	b.WriteString("Disallow: /img/\n")
	b.WriteString("Disallow: /preview/\n")
	b.WriteString("Disallow: /thumb/\n")
	b.WriteString("Disallow: /emoji/\n")
	b.WriteString("Disallow: /vid/\n")
	b.WriteString("Disallow: /hls/\n")
	b.WriteString("\n")
	host := h.canonicalHost()
	if host != "" {
		fmt.Fprintf(&b, "Sitemap: %s/sitemap.xml\n", host)
	} else {
		b.WriteString("Sitemap: /sitemap.xml\n")
	}
	io.WriteString(w, b.String())
}

// decorateArchiveHubSEO stamps the archive hub's BasePage with indexability +
// a meta description listing the top archived subs + a JSON-LD ItemList. No-op
// when SEO is off (BasePage stays at the global noindex,nofollow default) or
// when a local-archive search is active (search-result pages aren't indexable
// — the query is user-supplied and produces effectively infinite URL shapes).
func (h *Handler) decorateArchiveHubSEO(d *render.ArchiveHubPageData, subs []string) {
	if !h.indexingAllowed() || d.Search {
		return
	}
	d.Indexable = true
	d.MetaDescription = h.archiveHubDescription(d.BrandName, subs)
	d.HeadExtraHTML = h.archiveHubHeadExtra(d.BrandName, d.MetaDescription, subs)
}

// decorateArchiveSubSEO stamps a per-sub archive page with indexability + a
// sub-specific meta description + a CollectionPage JSON-LD block.
func (h *Handler) decorateArchiveSubSEO(d *render.ArchivePageData) {
	if !h.indexingAllowed() {
		return
	}
	d.Indexable = true
	d.MetaDescription = fmt.Sprintf(
		"r/%s archive on %s — %d posts mirrored locally for offline browsing.",
		d.Sub, d.BrandName, d.TotalPosts,
	)
	d.HeadExtraHTML = h.archiveSubHeadExtra(d.BrandName, d.Sub, d.MetaDescription, d.TotalPosts)
}

func (h *Handler) archiveHubDescription(brand string, subs []string) string {
	cap := archiveDescSubCap
	if cap > len(subs) {
		cap = len(subs)
	}
	if cap == 0 {
		return fmt.Sprintf("%s — local Reddit archive.", brand)
	}
	list := make([]string, cap)
	for i := 0; i < cap; i++ {
		list[i] = "r/" + subs[i]
	}
	more := ""
	if len(subs) > cap {
		more = fmt.Sprintf(" and %d more", len(subs)-cap)
	}
	return fmt.Sprintf("%s — local Reddit archive mirroring %s%s.", brand, strings.Join(list, ", "), more)
}

func (h *Handler) archiveHubHeadExtra(brand, desc string, subs []string) string {
	host := h.canonicalHost()
	pageURL := host + "/archive"
	if host == "" {
		pageURL = "/archive"
	}

	var b strings.Builder
	if host != "" {
		fmt.Fprintf(&b, "<link rel=\"canonical\" href=\"%s\"/>", html.EscapeString(pageURL))
	}
	writeOpenGraph(&b, brand+" — archived subreddits", desc, pageURL, host, "website")

	// CollectionPage + ItemList — tells Google "this URL collects these
	// subreddit pages", and each ListItem points back at the per-sub archive
	// URL so the knowledge graph maps instance → subs cleanly.
	cap := archiveJSONLDSubCap
	if cap > len(subs) {
		cap = len(subs)
	}
	items := make([]map[string]any, cap)
	for i := 0; i < cap; i++ {
		subURL := "/archive/r/" + subs[i]
		if host != "" {
			subURL = host + subURL
		}
		items[i] = map[string]any{
			"@type":    "ListItem",
			"position": i + 1,
			"url":      subURL,
			"name":     "r/" + subs[i],
		}
	}
	jsonld := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CollectionPage",
		"name":        brand + " — archived subreddits",
		"description": desc,
		"url":         pageURL,
		"mainEntity": map[string]any{
			"@type":           "ItemList",
			"numberOfItems":   len(subs),
			"itemListElement": items,
		},
	}
	writeJSONLD(&b, jsonld)
	return b.String()
}

func (h *Handler) archiveSubHeadExtra(brand, sub, desc string, total int64) string {
	host := h.canonicalHost()
	pageURL := host + "/archive/r/" + sub
	if host == "" {
		pageURL = "/archive/r/" + sub
	}

	var b strings.Builder
	if host != "" {
		fmt.Fprintf(&b, "<link rel=\"canonical\" href=\"%s\"/>", html.EscapeString(pageURL))
	}
	writeOpenGraph(&b, "r/"+sub+" — "+brand+" archive", desc, pageURL, host, "website")

	hubURL := "/archive"
	if host != "" {
		hubURL = host + "/archive"
	}
	jsonld := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CollectionPage",
		"name":        "r/" + sub + " — " + brand + " archive",
		"description": desc,
		"url":         pageURL,
		"isPartOf": map[string]any{
			"@type": "CollectionPage",
			"name":  brand + " — archived subreddits",
			"url":   hubURL,
		},
		"mainEntity": map[string]any{
			"@type":            "ItemList",
			"name":             "Archived posts from r/" + sub,
			"numberOfItems":    total,
			"itemListOrder":    "https://schema.org/ItemListOrderDescending",
		},
	}
	writeJSONLD(&b, jsonld)
	return b.String()
}

func writeOpenGraph(b *strings.Builder, title, desc, url, host, ogType string) {
	fmt.Fprintf(b, "<meta property=\"og:title\" content=\"%s\"/>", html.EscapeString(title))
	fmt.Fprintf(b, "<meta property=\"og:description\" content=\"%s\"/>", html.EscapeString(desc))
	fmt.Fprintf(b, "<meta property=\"og:type\" content=\"%s\"/>", html.EscapeString(ogType))
	if url != "" {
		fmt.Fprintf(b, "<meta property=\"og:url\" content=\"%s\"/>", html.EscapeString(url))
	}
	if host != "" {
		fmt.Fprintf(b, "<meta property=\"og:image\" content=\"%s/logo.png\"/>", html.EscapeString(host))
	}
	fmt.Fprintf(b, "<meta name=\"twitter:card\" content=\"summary\"/>")
	fmt.Fprintf(b, "<meta name=\"twitter:title\" content=\"%s\"/>", html.EscapeString(title))
	fmt.Fprintf(b, "<meta name=\"twitter:description\" content=\"%s\"/>", html.EscapeString(desc))
}

func writeJSONLD(b *strings.Builder, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("handler: json-ld marshal: %v", err)
		return
	}
	// JSON-LD lives in <script type="application/ld+json">. The only sequence
	// that can break out is "</" in JSON string values; escape "<" defensively
	// so a future sub name with "<" never closes the script tag.
	safe := strings.ReplaceAll(string(data), "<", "\\u003c")
	b.WriteString("<script type=\"application/ld+json\">")
	b.WriteString(safe)
	b.WriteString("</script>")
}

// sitemapURL is the standard sitemaps.org <url> shape; rendered via
// encoding/xml so loc/changefreq/priority get properly escaped.
type sitemapURL struct {
	Loc        string  `xml:"loc"`
	ChangeFreq string  `xml:"changefreq,omitempty"`
	Priority   string  `xml:"priority,omitempty"`
}

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

func (h *Handler) handleSitemapXML(w http.ResponseWriter, r *http.Request) {
	// 404 when SEO is off: that way we don't accidentally feed a sitemap to a
	// crawler that's also reading "Disallow: /" — a contradiction Google logs
	// as an error.
	if !h.indexingAllowed() {
		http.NotFound(w, r)
		return
	}

	host := h.canonicalHost()
	abs := func(path string) string {
		if host == "" {
			return path
		}
		return host + path
	}

	urls := []sitemapURL{
		{Loc: abs("/archive"), ChangeFreq: "hourly", Priority: "1.0"},
		{Loc: abs("/archive?sort=top"), ChangeFreq: "daily", Priority: "0.8"},
		{Loc: abs("/archive?sort=all"), ChangeFreq: "daily", Priority: "0.7"},
	}

	if h.postStore != nil {
		subs, err := h.postStore.ArchivedSubsByTop(0)
		if err != nil {
			log.Printf("handler: sitemap archived subs: %v", err)
		}
		if len(subs) > sitemapEntryCap-len(urls) {
			subs = subs[:sitemapEntryCap-len(urls)]
		}
		// Priority decays with rank so Google has a useful signal about
		// which subs are this instance's bulk vs long-tail. Top sub = 0.9,
		// floors at 0.3.
		for i, s := range subs {
			pri := 0.9 - float64(i)*0.01
			if pri < 0.3 {
				pri = 0.3
			}
			urls = append(urls, sitemapURL{
				Loc:        abs("/archive/r/" + s.Name),
				ChangeFreq: "daily",
				Priority:   fmt.Sprintf("%.1f", pri),
			})
		}
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(sitemapURLSet{
		XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs:  urls,
	}); err != nil {
		log.Printf("handler: sitemap encode: %v", err)
	}
	io.WriteString(w, "\n")
}
