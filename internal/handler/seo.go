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
	"time"

	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
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

// indexingAllowed returns true when the instance allows search engine indexing.
// ON by default (decentralized discovery) — only an operator who explicitly sets
// seo.allow_indexing=false flips the global robots meta to noindex and
// /robots.txt to "Disallow: /".
func (h *Handler) indexingAllowed() bool {
	return h.cfg != nil && h.cfg.SEO.AllowIndexing
}

// npConfiguredSubs returns the subreddits this instance has chosen for Natural
// Prefetch — its *intent*, what it set out to mirror — lowercased and deduped.
// Unlike the archived-subs list (which only contains subs that already have
// stored posts crossing the hub threshold), this reflects the operator's
// configuration the moment they save it, so a freshly stood-up instance is
// discoverable by its chosen subs immediately rather than after the first crawl
// cycle lands. This is the signal decentralized discovery hangs on: it answers
// "which instance mirrors r/X?" for every advertised X, populated or not.
func (h *Handler) npConfiguredSubs() []string {
	if h == nil {
		return nil
	}
	raw := h.siteDefault("prefetch_subs")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return searchquery.Parse(raw).WhiteSubs
}

// discoverySubs unions the instance's archived subs (already-stored, ranked by
// the caller) with its configured-but-not-yet-populated NP subs, preserving the
// archived ordering first and appending NP-only subs after. The result is the
// full set of subs this instance advertises to crawlers. De-dup is
// case-insensitive; NP names are emitted in their configured (lowercased) form.
func discoverySubs(archived, npSubs []string) []string {
	seen := make(map[string]struct{}, len(archived)+len(npSubs))
	out := make([]string, 0, len(archived)+len(npSubs))
	for _, s := range archived {
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	for _, s := range npSubs {
		k := strings.ToLower(s)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, s)
	}
	return out
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
	// /np.json is the machine-readable advert of this instance's Natural-Prefetch
	// sub list — the primitive decentralized aggregators crawl to map sub → mirror.
	b.WriteString("Allow: /np.json\n")
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
	b.WriteString("Disallow: /style/\n")
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
	// Advertise the full discovery set — what the instance has archived AND what
	// it has configured for Natural Prefetch — so search engines can match the
	// instance against every sub it mirrors, even ones still warming up.
	subs = discoverySubs(subs, h.npConfiguredSubs())
	d.Indexable = true
	d.MetaDescription = h.archiveHubDescription(d.BrandName, subs)
	d.HeadExtraHTML = h.archiveHubHeadExtra(d.BrandName, d.MetaDescription, subs)
}

// decorateArchiveSubSEO stamps a per-sub archive page with indexability + a
// sub-specific meta description + a CollectionPage JSON-LD block. offset is the
// current pagination offset (0 on the canonical landing) and pageSize is the
// per-page batch size; together they drive rel="prev"/"next" link emission.
func (h *Handler) decorateArchiveSubSEO(d *render.ArchivePageData, offset, pageSize int) {
	if !h.indexingAllowed() {
		return
	}
	d.Indexable = true
	d.MetaDescription = fmt.Sprintf(
		"r/%s archive on %s — %d posts mirrored locally for offline browsing.",
		d.Sub, d.BrandName, d.TotalPosts,
	)
	d.HeadExtraHTML = h.archiveSubHeadExtra(d.BrandName, d.Sub, d.MetaDescription, d.TotalPosts, offset, pageSize)
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

	homeURL := "/"
	if host != "" {
		homeURL = host + "/"
	}
	breadcrumbs := map[string]any{
		"@context": "https://schema.org",
		"@type":    "BreadcrumbList",
		"itemListElement": []map[string]any{
			{"@type": "ListItem", "position": 1, "name": brand, "item": homeURL},
			{"@type": "ListItem", "position": 2, "name": "Archive", "item": pageURL},
		},
	}
	writeJSONLD(&b, breadcrumbs)

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

func (h *Handler) archiveSubHeadExtra(brand, sub, desc string, total int64, offset, pageSize int) string {
	host := h.canonicalHost()
	subPath := "/archive/r/" + sub
	pageURL := host + subPath
	if host == "" {
		pageURL = subPath
	}

	var b strings.Builder
	// The canonical always points at the offset=0 landing — paginated views
	// are duplicates of the same collection and should consolidate signals
	// onto the root.
	if host != "" {
		fmt.Fprintf(&b, "<link rel=\"canonical\" href=\"%s\"/>", html.EscapeString(pageURL))
	}

	// rel="prev"/"next" hint crawlers to walk pagination as a single series.
	abs := func(path string) string {
		if host == "" {
			return path
		}
		return host + path
	}
	if pageSize > 0 {
		if offset >= pageSize {
			prevOff := offset - pageSize
			prev := subPath
			if prevOff > 0 {
				prev = fmt.Sprintf("%s?offset=%d", subPath, prevOff)
			}
			fmt.Fprintf(&b, "<link rel=\"prev\" href=\"%s\"/>", html.EscapeString(abs(prev)))
		}
		if int64(offset+pageSize) < total {
			next := fmt.Sprintf("%s?offset=%d", subPath, offset+pageSize)
			fmt.Fprintf(&b, "<link rel=\"next\" href=\"%s\"/>", html.EscapeString(abs(next)))
		}
	}

	writeOpenGraph(&b, "r/"+sub+" — "+brand+" archive", desc, pageURL, host, "website")

	hubURL := "/archive"
	if host != "" {
		hubURL = host + "/archive"
	}

	// BreadcrumbList renders the Home › Archive › r/sub trail in Google's
	// SERP snippet — high-impact for click-through on long-tail queries.
	homeURL := "/"
	if host != "" {
		homeURL = host + "/"
	}
	breadcrumbs := map[string]any{
		"@context": "https://schema.org",
		"@type":    "BreadcrumbList",
		"itemListElement": []map[string]any{
			{"@type": "ListItem", "position": 1, "name": brand, "item": homeURL},
			{"@type": "ListItem", "position": 2, "name": "Archive", "item": hubURL},
			{"@type": "ListItem", "position": 3, "name": "r/" + sub, "item": pageURL},
		},
	}
	writeJSONLD(&b, breadcrumbs)

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
			"@type":         "ItemList",
			"name":          "Archived posts from r/" + sub,
			"numberOfItems": total,
			"itemListOrder": "https://schema.org/ItemListOrderDescending",
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
	Loc        string `xml:"loc"`
	LastMod    string `xml:"lastmod,omitempty"`
	ChangeFreq string `xml:"changefreq,omitempty"`
	Priority   string `xml:"priority,omitempty"`
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

	var hubLastMod string
	urls := []sitemapURL{
		{Loc: abs("/archive"), ChangeFreq: "hourly", Priority: "1.0"},
		{Loc: abs("/archive?sort=top"), ChangeFreq: "daily", Priority: "0.8"},
		{Loc: abs("/archive?sort=all"), ChangeFreq: "daily", Priority: "0.7"},
	}

	seen := make(map[string]struct{})
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
		var maxLU time.Time
		for i, s := range subs {
			seen[strings.ToLower(s.Name)] = struct{}{}
			pri := 0.9 - float64(i)*0.01
			if pri < 0.3 {
				pri = 0.3
			}
			lm := ""
			if !s.LastUpdated.IsZero() {
				lm = s.LastUpdated.UTC().Format("2006-01-02")
				if s.LastUpdated.After(maxLU) {
					maxLU = s.LastUpdated
				}
			}
			urls = append(urls, sitemapURL{
				Loc:        abs("/archive/r/" + s.Name),
				LastMod:    lm,
				ChangeFreq: "daily",
				Priority:   fmt.Sprintf("%.1f", pri),
			})
		}
		if !maxLU.IsZero() {
			hubLastMod = maxLU.UTC().Format("2006-01-02")
		}
	}

	// Configured-but-not-yet-archived NP subs still get a sitemap entry so the
	// instance is crawlable on its advertised intent from minute one. They sit
	// at the 0.3 floor (no posts yet) and carry no lastmod.
	for _, name := range h.npConfiguredSubs() {
		if len(urls) >= sitemapEntryCap {
			break
		}
		if _, ok := seen[strings.ToLower(name)]; ok {
			continue
		}
		seen[strings.ToLower(name)] = struct{}{}
		urls = append(urls, sitemapURL{
			Loc:        abs("/archive/r/" + name),
			ChangeFreq: "daily",
			Priority:   "0.3",
		})
	}
	if hubLastMod != "" {
		for i := range urls[:3] {
			urls[i].LastMod = hubLastMod
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

// npDiscovery is the stable, machine-readable shape served at /np.json. It is
// the decentralized-discovery primitive: aggregators and crawlers fetch it to
// learn which subreddits this instance mirrors, without scraping HTML or
// depending on a central index. The schema is intentionally flat and additive —
// new fields may be appended, existing ones never change meaning.
type npDiscovery struct {
	Brand    string               `json:"brand"`
	Host     string               `json:"host,omitempty"`
	Archive  string               `json:"archive_url"`
	Count    int                  `json:"count"`
	Subs     []string             `json:"subs"`
	SubLinks []npDiscoverySubLink `json:"sub_links"`
}

type npDiscoverySubLink struct {
	Sub      string `json:"sub"`
	URL      string `json:"url"`
	Archived bool   `json:"archived"`
}

// handleNPDiscovery serves GET /np.json — the instance's Natural-Prefetch sub
// list as JSON. 404s when indexing is off (a private instance advertises
// nothing). Unions configured NP subs with already-archived subs so the feed
// reflects both intent and contents.
func (h *Handler) handleNPDiscovery(w http.ResponseWriter, r *http.Request) {
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

	archivedSet := make(map[string]struct{})
	var archived []string
	if h.postStore != nil {
		if subs, err := h.postStore.ArchivedSubsByTop(0); err != nil {
			log.Printf("handler: np.json archived subs: %v", err)
		} else {
			for _, s := range subs {
				archivedSet[strings.ToLower(s.Name)] = struct{}{}
				archived = append(archived, s.Name)
			}
		}
	}

	subs := discoverySubs(archived, h.npConfiguredSubs())
	links := make([]npDiscoverySubLink, len(subs))
	for i, s := range subs {
		_, isArchived := archivedSet[strings.ToLower(s)]
		links[i] = npDiscoverySubLink{
			Sub:      s,
			URL:      abs("/archive/r/" + s),
			Archived: isArchived,
		}
	}

	payload := npDiscovery{
		Brand:    h.cfg.Render.BrandName,
		Host:     host,
		Archive:  abs("/archive"),
		Count:    len(subs),
		Subs:     subs,
		SubLinks: links,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	// Cross-origin readable so browser-based aggregators / directory sites can
	// fetch the advert directly. The payload is already-public config, no secrets.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		log.Printf("handler: np.json encode: %v", err)
	}
}
