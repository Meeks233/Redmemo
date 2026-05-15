package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

func (h *Handler) notifyUserRequest() {
	if h.prefetcher != nil {
		h.prefetcher.NotifyUserRequest()
	}
}

func (h *Handler) fetchSubreddit(ctx context.Context, sub, sort, after string, limit int) ([]reddit.Post, string, string, error) {
	if h.oauthPool.HasAvailableTokens() {
		posts, before, after, err := h.redditCli.FetchSubreddit(ctx, sub, sort, after, limit)
		if err == nil {
			h.notifyUserRequest()
			return posts, before, after, nil
		}
	}
	return h.publicCli.FetchSubreddit(ctx, sub, sort, after, limit)
}

func (h *Handler) fetchPost(ctx context.Context, sub, id, commentSort string) (reddit.Post, []reddit.Comment, error) {
	if h.oauthPool.HasAvailableTokens() {
		post, comments, err := h.redditCli.FetchPost(ctx, sub, id, commentSort)
		if err == nil {
			h.notifyUserRequest()
			return post, comments, nil
		}
	}
	return h.publicCli.FetchPost(ctx, sub, id, commentSort)
}

const archivePageSize = 25
const archiveHubMinPosts = 10

func (h *Handler) handleArchiveHub(w http.ResponseWriter, r *http.Request) {
	prefs := h.readPreferences(r)

	sort := r.URL.Query().Get("sort")
	switch sort {
	case "new", "top", "all":
	default:
		sort = "new"
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
		Sort:     sort,
		Subs:     subs,
		MinPosts: archiveHubMinPosts,
	}
	if sort == "all" {
		data.AlphaGroups, data.AlphaIndex = groupSubsAlphabetical(subs)
	}

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

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}

	total, _ := h.postStore.CountBySubreddit(sub)
	totalPages := int((total + archivePageSize - 1) / archivePageSize)
	if totalPages < 1 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * archivePageSize
	stored, err := h.postStore.ListBySubreddit(sub, archivePageSize, offset)
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

	data := render.ArchivePageData{
		BasePage: render.BasePage{
			URL:       r.URL.Path,
			Prefs:     prefs,
			BrandName: h.cfg.Render.BrandName,
			Version:   "0.1.0",
		},
		Sub:                sub,
		Posts:              posts,
		TotalPosts:         total,
		Page:               page,
		TotalPages:         totalPages,
		AllPostsHiddenNSFW: allPostsNSFW(posts, prefs),
		HasPrev:            page > 1,
		HasNext:            page < totalPages,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.renderer.RenderArchive(w, data); err != nil {
		log.Printf("handler: render archive %s: %v", sub, err)
	}
}

func (h *Handler) isArchivableSub(sub string) bool {
	if h.postStore != nil {
		if count, err := h.postStore.CountBySubreddit(sub); err == nil && count > 0 {
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

func (h *Handler) fetchSubredditAbout(ctx context.Context, sub string) (reddit.Subreddit, error) {
	if h.oauthPool.HasAvailableTokens() {
		info, err := h.redditCli.FetchSubredditAbout(ctx, sub)
		if err == nil {
			h.notifyUserRequest()
			return info, nil
		}
	}
	return h.publicCli.FetchSubredditAbout(ctx, sub)
}
