package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/searchquery"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) notifyUserRequest() {
	if h.prefetcher != nil {
		h.prefetcher.NotifyUserRequest()
	}
}

func (h *Handler) fetchSubreddit(ctx context.Context, sub, sort, after string, limit int) ([]reddit.Post, string, string, error) {
	if h.oauthHolder.HasAvailableTokens() {
		posts, before, after, err := h.redditCli.FetchSubreddit(ctx, sub, sort, after, limit)
		h.recordUpstream(ctx)
		if err == nil {
			h.notifyUserRequest()
			return posts, before, after, nil
		}
	}
	posts, before, afterCur, err := h.publicCli.FetchSubreddit(ctx, sub, sort, after, limit)
	h.recordUpstream(ctx)
	return posts, before, afterCur, err
}

func (h *Handler) fetchPost(ctx context.Context, sub, id, commentSort string) (reddit.Post, []reddit.Comment, error) {
	if h.oauthHolder.HasAvailableTokens() {
		post, comments, err := h.redditCli.FetchPost(ctx, sub, id, commentSort)
		h.recordUpstream(ctx)
		if err == nil {
			h.notifyUserRequest()
			return post, comments, nil
		}
	}
	post, comments, err := h.publicCli.FetchPost(ctx, sub, id, commentSort)
	h.recordUpstream(ctx)
	return post, comments, err
}

const archivePageSize = 25
const archiveHubMinPosts = 10

// parseArchiveSearch reads the /archive local-search box from the request and
// converts it into a PostgreSQL query via the shared e621-style query parser.
// The returned bool reports whether a search is active (an empty box is not).
func parseArchiveSearch(r *http.Request) (render.ArchiveSearchView, store.ArchiveSearchOpts, searchquery.Parsed, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	view := render.ArchiveSearchView{Query: raw}
	parsed := searchquery.Parse(raw)
	opts := parsedToArchiveOpts(parsed)
	return view, opts, parsed, raw != ""
}

// archiveSearchQS builds the URL-encoded query string for the archive search
// (the raw box text, without the page cursor) used by pagination links.
func archiveSearchQS(v render.ArchiveSearchView) string {
	qs := url.Values{}
	if v.Query != "" {
		qs.Set("q", v.Query)
	}
	return qs.Encode()
}

// archiveCacheScoreScanCap bounds how many SQL-matched rows the cache-score
// archive search pulls into memory before Go-filtering. The cache_score: constraint
// can't be expressed in SQL (the eviction score lives behind canonical-key +
// muxed resolution), so this path scans up to this many matches of the *other*
// constraints and filters them by resident eviction score in Go. It is sized to
// comfortably cover a self-hosted archive's filtered set; a result set larger
// than the cap simply isn't fully counted (the total reflects what was scanned).
const archiveCacheScoreScanCap = 2000

// archiveCacheScoreSearch runs the archive local search for a query carrying a
// cache_score: (media cache eviction score) constraint. It pulls the SQL matches of
// every other constraint, keeps only posts whose primary media is resident and
// whose eviction score satisfies nc, then slices the survivors by offset/limit
// in Go. It returns the requested window of posts and the total survivor count.
func (h *Handler) archiveCacheScoreSearch(opts store.ArchiveSearchOpts, nc *searchquery.NumConstraint, offset int) (posts []reddit.Post, total int64) {
	opts.Limit = archiveCacheScoreScanCap
	opts.Offset = 0
	stored, _, err := h.postStore.ArchiveSearch(opts)
	if err != nil {
		log.Printf("handler: archive cache-score search: %v", err)
	}

	matched := make([]reddit.Post, 0, len(stored))
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err != nil {
			continue
		}
		if !h.matchCacheScore(nc, &p) {
			continue
		}
		p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
		matched = append(matched, p)
	}

	total = int64(len(matched))
	if offset < 0 {
		offset = 0
	}
	if offset > len(matched) {
		offset = len(matched)
	}
	end := offset + archivePageSize
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total
}

func (h *Handler) handleArchiveHub(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	sort := r.URL.Query().Get("sort")
	switch sort {
	case "new", "top", "all":
	default:
		sort = "new"
	}

	searchView, searchOpts, parsed, searching := parseArchiveSearch(r)

	// Local archive search — purely PostgreSQL-backed, never touches Reddit.
	if searching && h.postStore != nil {
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				offset = n
			}
		}
		searchOpts.Sort = sort

		// Honor the global show_nsfw preference. The archive search form has
		// its own NSFW dropdown for explicit narrowing, but show_nsfw=off must
		// dominate: a user who turned NSFW off should never see NSFW results
		// regardless of what the archive form says.
		if prefs.ShowNSFW != "on" {
			searchOpts.NSFW = "sfw"
		}

		var posts []reddit.Post
		var total int64
		if parsed.CacheScore != nil && h.mediaProxy != nil {
			// The cache_score: constraint (media cache eviction score) cannot be pushed
			// into SQL, so this path scans the other constraints' matches and
			// filters/paginates by resident eviction score in Go.
			posts, total = h.archiveCacheScoreSearch(searchOpts, parsed.CacheScore, offset)
		} else {
			searchOpts.Limit = archivePageSize
			searchOpts.Offset = offset
			stored, t, err := h.postStore.ArchiveSearch(searchOpts)
			if err != nil {
				log.Printf("handler: archive search: %v", err)
			}
			total = t
			for _, sp := range stored {
				var p reddit.Post
				if err := json.Unmarshal(sp.JSONData, &p); err == nil {
					p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
					posts = append(posts, p)
				}
			}
		}

		if r.URL.Query().Get("partial") == "1" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if len(posts) == 0 {
				return
			}
			if err := h.renderer.RenderPostList(w, posts, prefs); err != nil {
				log.Printf("handler: render archive search partial: %v", err)
			}
			return
		}

		data := render.ArchiveHubPageData{
			BasePage: render.BasePage{
				URL:       r.URL.Path,
				Prefs:     prefs,
				BrandName: h.cfg.Render.BrandName,
				Version:   "0.1.0",
			},
			Sort:         sort,
			MinPosts:     archiveHubMinPosts,
			SearchParams: searchView,
			Search:       true,
			SearchPosts:  posts,
			SearchTotal:  total,
			SearchPageSize: archivePageSize,
			SearchQS:     archiveSearchQS(searchView),
			Interval:     prefs.ScrollInterval,
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := h.renderer.RenderArchiveHub(w, data); err != nil {
			log.Printf("handler: render archive search: %v", err)
		}
		return
	}

	var raw []store.ArchivedSub
	if h.postStore != nil {
		var err error
		switch sort {
		case "top":
			raw, err = h.postStore.ArchivedSubsByTop(archiveHubMinPosts)
		case "all":
			raw, err = h.postStore.ArchivedSubsAlphabetical()
		default:
			raw, err = h.postStore.ArchivedSubsByNew(archiveHubMinPosts)
		}
		if err != nil {
			log.Printf("handler: archive hub (%s): %v", sort, err)
		}
	}

	names := make([]string, 0, len(raw))
	for _, rs := range raw {
		names = append(names, rs.Name)
	}
	var iconMap map[string]*store.SubIcon
	if h.subIconStore != nil && len(names) > 0 {
		iconMap, _ = h.subIconStore.GetIconMap(names)
	}

	nsfwSet, deadSet := h.resolveArchiveTags(names)

	subs := make([]render.ArchiveHubEntry, 0, len(raw))
	for _, rs := range raw {
		entry := render.ArchiveHubEntry{
			Name:      rs.Name,
			PostCount: rs.PostCount,
		}
		if icon, ok := iconMap[rs.Name]; ok {
			entry.IconURL = icon.IconURL
		}
		key := strings.ToLower(rs.Name)
		if nsfwSet[key] {
			entry.NSFW = true
		}
		if deadSet[key] {
			entry.Dead = true
		}
		subs = append(subs, entry)
	}

	// Trigger passive L4 icon check
	if h.prefetcher != nil {
		go h.prefetcher.CheckIconsPassive()
	}

	data := render.ArchiveHubPageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sort:         sort,
		Subs:         subs,
		MinPosts:     archiveHubMinPosts,
		SearchParams: searchView,
	}
	if sort == "all" {
		data.AlphaGroups, data.AlphaIndex = groupSubsAlphabetical(subs)
	}

	h.decorateArchiveHubSEO(&data, names)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderArchiveHub(w, data); err != nil {
		log.Printf("handler: render archive hub: %v", err)
	}
}

// resolveArchiveTags returns lowercase-keyed sets of NSFW and Dead subs for
// the given names. NSFW is looked up in subreddit_status first; for any name
// missing a recorded flag, we lazily compute it from the posts table and
// upsert the result so subsequent calls are O(1).
func (h *Handler) resolveArchiveTags(names []string) (nsfw map[string]bool, dead map[string]bool) {
	nsfw = make(map[string]bool)
	dead = make(map[string]bool)
	if len(names) == 0 {
		return
	}

	var recorded map[string]bool
	if h.subStatusStore != nil {
		m, err := h.subStatusStore.GetNSFWMap(names)
		if err != nil {
			log.Printf("handler: archive hub get nsfw map: %v", err)
		} else {
			recorded = make(map[string]bool, len(m))
			for k, v := range m {
				recorded[strings.ToLower(k)] = v
			}
		}
	}

	var missing []string
	for _, n := range names {
		if _, ok := recorded[strings.ToLower(n)]; !ok {
			missing = append(missing, n)
		}
	}

	if len(missing) > 0 && h.postStore != nil {
		detected, err := h.postStore.DetectNSFWForSubs(missing)
		if err != nil {
			log.Printf("handler: archive hub detect nsfw: %v", err)
		} else if h.subStatusStore != nil {
			for _, n := range missing {
				v := detected[n]
				if recorded == nil {
					recorded = make(map[string]bool)
				}
				recorded[strings.ToLower(n)] = v
				if err := h.subStatusStore.SetNSFW(n, v); err != nil {
					log.Printf("handler: archive hub upsert nsfw %s: %v", n, err)
				}
			}
		}
	}
	for k, v := range recorded {
		if v {
			nsfw[k] = true
		}
	}

	if h.subStatusStore != nil {
		stMap, err := h.subStatusStore.GetStatusMap(names)
		if err != nil {
			log.Printf("handler: archive hub get status map: %v", err)
		} else {
			for n, st := range stMap {
				if st == "dead" || st == "private" || st == "quarantined" {
					dead[strings.ToLower(n)] = true
				}
			}
		}
	}
	return
}

// groupSubsAlphabetical buckets subs by their initial letter (A-Z); anything
// starting with a non-letter goes to the "#" group. Input is assumed sorted
// case-insensitively by Name.
func groupSubsAlphabetical(subs []render.ArchiveHubEntry) ([]render.ArchiveAlphaGroup, []string) {
	if len(subs) == 0 {
		return nil, nil
	}
	groupMap := make(map[string]*render.ArchiveAlphaGroup)
	var order []string
	for _, s := range subs {
		letter := "#"
		if s.Name != "" {
			c := s.Name[0]
			if c >= 'a' && c <= 'z' {
				letter = strings.ToUpper(string(c))
			} else if c >= 'A' && c <= 'Z' {
				letter = strings.ToUpper(string(c))
			}
		}
		g, ok := groupMap[letter]
		if !ok {
			g = &render.ArchiveAlphaGroup{Letter: letter}
			groupMap[letter] = g
			order = append(order, letter)
		}
		g.Subs = append(g.Subs, s)
	}
	groups := make([]render.ArchiveAlphaGroup, 0, len(order))
	for _, l := range order {
		g := groupMap[l]
		sort.SliceStable(g.Subs, func(i, j int) bool {
			if g.Subs[i].PostCount != g.Subs[j].PostCount {
				return g.Subs[i].PostCount > g.Subs[j].PostCount
			}
			return strings.ToLower(g.Subs[i].Name) < strings.ToLower(g.Subs[j].Name)
		})
		groups = append(groups, *g)
	}
	return groups, order
}

func (h *Handler) handleArchiveSub(w http.ResponseWriter, r *http.Request) {
	sub := r.PathValue("sub")
	if sub == "" || !validSubName.MatchString(sub) {
		http.NotFound(w, r)
		return
	}

	if !h.isArchivableSub(sub) {
		http.NotFound(w, r)
		return
	}

	prefs := h.readPreferences(r)

	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}

	excludeNSFW := prefs.ShowNSFW != "on"
	stored, err := h.postStore.ListBySubreddit(sub, archivePageSize, offset, excludeNSFW)
	if err != nil {
		log.Printf("handler: archive list %s: %v", sub, err)
	}

	var posts []reddit.Post
	for _, sp := range stored {
		var p reddit.Post
		if err := json.Unmarshal(sp.JSONData, &p); err == nil {
			p.ArchivedRelTime, p.ArchivedTime = reddit.FormatTime(float64(sp.FirstSeen.Unix()))
			posts = append(posts, p)
		}
	}

	if r.URL.Query().Get("partial") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if len(posts) == 0 {
			return
		}
		if err := h.renderer.RenderPostList(w, posts, prefs); err != nil {
			log.Printf("handler: render archive partial %s: %v", sub, err)
		}
		return
	}

	total, _ := h.postStore.CountBySubreddit(sub, excludeNSFW)

	data := render.ArchivePageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:        sub,
		Posts:      posts,
		TotalPosts: total,
		PageSize:   archivePageSize,
		Interval:   prefs.ScrollInterval,
	}

	h.decorateArchiveSubSEO(&data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderArchive(w, data); err != nil {
		log.Printf("handler: render archive %s: %v", sub, err)
	}
}

func (h *Handler) isArchivableSub(sub string) bool {
	if h.postStore != nil {
		if count, err := h.postStore.CountBySubreddit(sub, false); err == nil && count > 0 {
			return true
		}
	}
	if h.settingsStore != nil {
		if v, ok, _ := h.settingsStore.Get("prefetch_subs"); ok && v != "" {
			for _, s := range strings.Split(v, "+") {
				if strings.EqualFold(strings.TrimSpace(s), sub) {
					return true
				}
			}
		}
	}
	return false
}

// fetchSubredditAbout returns the /r/<sub>/about data. It ALWAYS prefers the
// DB cache (60-day TTL via sub_icons.about_*). Upstream is consulted only
// when:
//   1. active=true (the request was initiated by a user actively viewing
//      the subreddit page — never by background prefetch or archive jobs), AND
//   2. The cache is missing OR expired.
//
// When active=false and the cache is missing/expired, an empty Subreddit
// is returned without any upstream call. This enforces the "no auto-fetch
// of about" policy.
func (h *Handler) fetchSubredditAbout(ctx context.Context, sub string, active bool) (reddit.Subreddit, error) {
	// 1. DB cache lookup.
	if h.subIconStore != nil {
		if cached, err := h.subIconStore.Get(sub); err == nil && cached != nil && len(cached.AboutJSON) > 0 {
			fresh := cached.AboutExpiresAt != nil && time.Now().Before(*cached.AboutExpiresAt)
			if fresh {
				var info reddit.Subreddit
				if jerr := json.Unmarshal(cached.AboutJSON, &info); jerr == nil {
					return info, nil
				}
			}
			// Stale or unparseable — fall through. If we're not in active
			// mode, return the stale data rather than nothing (better than
			// an empty page) but do not refresh.
			if !active {
				var info reddit.Subreddit
				_ = json.Unmarshal(cached.AboutJSON, &info)
				return info, nil
			}
		}
	}

	// 2. Active visit + cache miss/expired → upstream + persist.
	if !active {
		return reddit.Subreddit{}, nil
	}

	var info reddit.Subreddit
	var err error
	if h.oauthHolder.HasAvailableTokens() {
		info, err = h.redditCli.FetchSubredditAbout(ctx, sub)
		h.recordUpstream(ctx)
		if err == nil {
			h.notifyUserRequest()
		}
	}
	if err != nil || info.Name == "" {
		info, err = h.publicCli.FetchSubredditAbout(ctx, sub)
		h.recordUpstream(ctx)
	}
	if err == nil && h.subIconStore != nil && info.Name != "" {
		if data, jerr := json.Marshal(info); jerr == nil {
			if serr := h.subIconStore.SaveAbout(sub, data); serr != nil {
				log.Printf("handler: save about r/%s: %v", sub, serr)
			}
		}
	}
	return info, err
}
