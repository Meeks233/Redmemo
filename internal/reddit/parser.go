package reddit

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"strings"
)

// ParseSubredditListing parses a subreddit listing JSON response.
// Returns posts, before cursor, after cursor, error.
func ParseSubredditListing(data []byte) ([]Post, string, string, error) {
	var raw struct {
		Kind string `json:"kind"`
		Data struct {
			Before   string            `json:"before"`
			After    string            `json:"after"`
			Children []json.RawMessage `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, "", "", fmt.Errorf("parse listing: %w", err)
	}

	posts := make([]Post, 0, len(raw.Data.Children))
	for _, child := range raw.Data.Children {
		p, err := ParsePost(child)
		if err != nil {
			continue
		}
		posts = append(posts, p)
	}
	return posts, raw.Data.Before, raw.Data.After, nil
}

// ParsePost parses a single post from a listing child JSON object.
func ParsePost(data json.RawMessage) (Post, error) {
	var wrapper struct {
		Kind string                 `json:"kind"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Post{}, fmt.Errorf("parse post wrapper: %w", err)
	}
	if wrapper.Kind != "t3" {
		return Post{}, fmt.Errorf("unexpected kind: %s", wrapper.Kind)
	}
	return buildPost(wrapper.Data), nil
}

func buildPost(d map[string]interface{}) Post {
	p := Post{
		ID:        getString(d, "id"),
		Title:     getString(d, "title"),
		Community: getString(d, "subreddit"),
		Permalink: getString(d, "permalink"),
		Domain:    getString(d, "domain"),
		NSFW:      getBool(d, "over_18"),
		WSURL:     getString(d, "websocket_url"),
	}

	// Author
	p.Author = Author{
		Name:          getString(d, "author"),
		Distinguished: getString(d, "distinguished"),
	}
	p.Author.Flair = parseFlair(d, "author_flair")

	// Post flair
	p.Flair = parseFlair(d, "link_flair")

	// Score
	score := getInt64(d, "score")
	hideScore := getBool(d, "hide_score")
	if hideScore {
		p.Score = [2]string{"•", "Hidden"}
	} else {
		p.Score = FormatNum(score)
	}

	// Upvote ratio
	if ratio, ok := d["upvote_ratio"].(float64); ok {
		p.UpvoteRatio = int64(ratio * 100)
	}

	// Comments count
	p.Comments = FormatNum(getInt64(d, "num_comments"))

	// Flags
	p.Flags = Flags{
		Spoiler:  getBool(d, "spoiler"),
		NSFW:     getBool(d, "over_18"),
		Stickied: getBool(d, "stickied") || getBool(d, "pinned"),
	}

	// Time
	createdUTC := getFloat64(d, "created_utc")
	p.CreatedTS = uint64(math.Round(createdUTC))
	p.RelTime, p.Created = FormatTime(createdUTC)

	// Duplicates
	p.NumDuplicates = uint64(getInt64(d, "num_duplicates"))

	// OutURL
	if outURL := getString(d, "url_overridden_by_dest"); outURL != "" {
		p.OutURL = &outURL
	}

	// Body (selftext_html)
	bodyHTML := getString(d, "selftext_html")
	if bodyHTML != "" {
		bodyHTML = RewriteURLs(bodyHTML)
		// An image pasted into a self post's selftext (e.g. a footer
		// screenshot) arrives as an auto-link whose text equals its href. Inline
		// it as an <img> — same treatment comment bodies get — so the picture
		// actually shows instead of a bare /preview/pre/... link the reader has
		// to click. EmbedCommentImages only touches text==href auto-links, so
		// user-written labelled links are left alone.
		bodyHTML = EmbedCommentImages(bodyHTML)
	}

	// Removed/deleted upstream — set p.Removed so the archive layer skips
	// overwriting the local copy and the renderer shows the Time Machine
	// badge. removed_by_category covers explicit mod/admin/spam removals; the
	// selftext / author == "[deleted]" branches catch user-deleted posts and
	// some legacy moderator removals where removed_by_category is missing.
	if cat := getString(d, "removed_by_category"); cat != "" {
		p.Removed = true
		if cat == "moderator" {
			bodyHTML = `<p>[Removed by Reddit]</p>`
		}
	} else {
		selftext := getString(d, "selftext")
		if selftext == "[removed]" || selftext == "[deleted]" {
			p.Removed = true
		} else if p.Author.Name == "[deleted]" && getBool(d, "is_self") && selftext == "" {
			p.Removed = true
		}
	}
	p.Body = template.HTML(bodyHTML)

	// Media/PostType detection
	p.PostType, p.Media, p.Thumbnail = parseMedia(d)

	// Gallery
	p.Gallery = parseGallery(d)
	if len(p.Gallery) > 0 && p.PostType == "" {
		p.PostType = "gallery"
	}

	if p.PostType == "" {
		if getBool(d, "is_self") {
			p.PostType = "self"
		} else {
			p.PostType = "link"
		}
	}

	// Awards
	p.Awards = parseAwards(d)

	// Poll
	p.Poll = parsePoll(d)

	return p
}

// parseMedia detects post type and extracts media info following redlib's priority chain.
func parseMedia(d map[string]interface{}) (postType string, media Media, thumb Media) {
	// Thumbnail
	thumbURL := FormatURL(getString(d, "thumbnail"))
	if thumbURL != "" {
		thumb.URL = thumbURL
		thumb.Width = getNestedInt64(d, "thumbnail_width")
		thumb.Height = getNestedInt64(d, "thumbnail_height")
	}

	// 1. preview.reddit_video_preview.fallback_url
	if preview := getMap(d, "preview"); preview != nil {
		if rvp := getMap(preview, "reddit_video_preview"); rvp != nil {
			if fallback := getString(rvp, "fallback_url"); fallback != "" {
				isGif := getBool(rvp, "is_gif")
				if isGif {
					postType = "gif"
				} else {
					postType = "video"
				}
				media.URL = FormatURL(fallback)
				media.AltURL = FormatURL(getString(rvp, "hls_url"))
				media.Width = getInt64(rvp, "width")
				media.Height = getInt64(rvp, "height")
				media.Duration = getFloat64(rvp, "duration")
				setPreviewDimensions(d, &media)
				setPoster(d, &media)
				return
			}
		}
	}

	// 2. secure_media.reddit_video.fallback_url
	if sm := getMap(d, "secure_media"); sm != nil {
		if rv := getMap(sm, "reddit_video"); rv != nil {
			if fallback := getString(rv, "fallback_url"); fallback != "" {
				isGif := getBool(rv, "is_gif")
				if isGif {
					postType = "gif"
				} else {
					postType = "video"
				}
				media.URL = FormatURL(fallback)
				media.AltURL = FormatURL(getString(rv, "hls_url"))
				media.Width = getInt64(rv, "width")
				media.Height = getInt64(rv, "height")
				media.Duration = getFloat64(rv, "duration")
				setPoster(d, &media)
				return
			}
		}
	}

	// 3. crosspost_parent_list[0].secure_media.reddit_video
	if cpList, ok := d["crosspost_parent_list"].([]interface{}); ok && len(cpList) > 0 {
		if cp, ok := cpList[0].(map[string]interface{}); ok {
			if sm := getMap(cp, "secure_media"); sm != nil {
				if rv := getMap(sm, "reddit_video"); rv != nil {
					if fallback := getString(rv, "fallback_url"); fallback != "" {
						isGif := getBool(rv, "is_gif")
						if isGif {
							postType = "gif"
						} else {
							postType = "video"
						}
						media.URL = FormatURL(fallback)
						media.AltURL = FormatURL(getString(rv, "hls_url"))
						media.Width = getInt64(rv, "width")
						media.Height = getInt64(rv, "height")
						media.Duration = getFloat64(rv, "duration")
						setPoster(cp, &media)
						return
					}
				}
			}
		}
	}

	// 4. post_hint == "image"
	if getString(d, "post_hint") == "image" {
		postType = "image"

		// Check for mp4 variant (animated image/gif)
		if preview := getMap(d, "preview"); preview != nil {
			if images := getArray(preview, "images"); len(images) > 0 {
				if img, ok := images[0].(map[string]interface{}); ok {
					if variants := getMap(img, "variants"); variants != nil {
						if mp4 := getMap(variants, "mp4"); mp4 != nil {
							if source := getMap(mp4, "source"); source != nil {
								if mp4URL := getString(source, "url"); mp4URL != "" {
									postType = "gif"
									media.URL = FormatURL(mp4URL)
									media.Width = getInt64(source, "width")
									media.Height = getInt64(source, "height")
									return
								}
							}
						}
					}
				}
			}
		}

		domain := getString(d, "domain")
		if domain == "i.redd.it" {
			media.URL = FormatURL(getString(d, "url"))
		} else if preview := getMap(d, "preview"); preview != nil {
			if images := getArray(preview, "images"); len(images) > 0 {
				if img, ok := images[0].(map[string]interface{}); ok {
					if source := getMap(img, "source"); source != nil {
						media.URL = FormatURL(getString(source, "url"))
						media.Width = getInt64(source, "width")
						media.Height = getInt64(source, "height")
					}
				}
			}
		}
		setPreviewDimensions(d, &media)
		return
	}

	// 5. is_self
	if getBool(d, "is_self") {
		postType = "self"
		return
	}

	// 6. is_gallery
	if getBool(d, "is_gallery") {
		postType = "gallery"
		return
	}

	// 7. crosspost_parent_list[0].is_gallery
	if cpList, ok := d["crosspost_parent_list"].([]interface{}); ok && len(cpList) > 0 {
		if cp, ok := cpList[0].(map[string]interface{}); ok {
			if getBool(cp, "is_gallery") {
				postType = "gallery"
				return
			}
		}
	}

	// 8. is_reddit_media_domain && domain == "i.redd.it"
	if getBool(d, "is_reddit_media_domain") && getString(d, "domain") == "i.redd.it" {
		postType = "image"
		media.URL = FormatURL(getString(d, "url"))
		setPreviewDimensions(d, &media)
		return
	}

	// 9. Default to link
	postType = "link"
	media.URL = getString(d, "url")
	return
}

func setPreviewDimensions(d map[string]interface{}, m *Media) {
	if m.Width > 0 && m.Height > 0 {
		return
	}
	if preview := getMap(d, "preview"); preview != nil {
		if images := getArray(preview, "images"); len(images) > 0 {
			if img, ok := images[0].(map[string]interface{}); ok {
				if source := getMap(img, "source"); source != nil {
					if m.Width == 0 {
						m.Width = getInt64(source, "width")
					}
					if m.Height == 0 {
						m.Height = getInt64(source, "height")
					}
				}
			}
		}
	}
}

func setPoster(d map[string]interface{}, m *Media) {
	if preview := getMap(d, "preview"); preview != nil {
		if images := getArray(preview, "images"); len(images) > 0 {
			if img, ok := images[0].(map[string]interface{}); ok {
				if source := getMap(img, "source"); source != nil {
					m.Poster = FormatURL(getString(source, "url"))
				}
			}
		}
	}
}

func parseGallery(d map[string]interface{}) []GalleryMedia {
	galleryData := getMap(d, "gallery_data")
	mediaMetadata := getMap(d, "media_metadata")

	// Try crosspost parent if not found
	if galleryData == nil || mediaMetadata == nil {
		if cpList, ok := d["crosspost_parent_list"].([]interface{}); ok && len(cpList) > 0 {
			if cp, ok := cpList[0].(map[string]interface{}); ok {
				if galleryData == nil {
					galleryData = getMap(cp, "gallery_data")
				}
				if mediaMetadata == nil {
					mediaMetadata = getMap(cp, "media_metadata")
				}
			}
		}
	}

	if galleryData == nil || mediaMetadata == nil {
		return nil
	}

	items := getArray(galleryData, "items")
	if len(items) == 0 {
		return nil
	}

	gallery := make([]GalleryMedia, 0, len(items))
	for _, item := range items {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		mediaID := getString(itemMap, "media_id")
		if mediaID == "" {
			continue
		}
		meta, ok := mediaMetadata[mediaID].(map[string]interface{})
		if !ok {
			continue
		}

		s := getMap(meta, "s")
		if s == nil {
			continue
		}

		var imgURL string
		mimeType := getString(meta, "m")
		if mimeType == "image/gif" {
			imgURL = getString(s, "gif")
		}
		if imgURL == "" {
			imgURL = getString(s, "u")
		}

		gm := GalleryMedia{
			URL:         FormatURL(imgURL),
			Width:       getInt64(s, "x"),
			Height:      getInt64(s, "y"),
			Caption:     getString(itemMap, "caption"),
			OutboundURL: getString(itemMap, "outbound_url"),
		}
		gallery = append(gallery, gm)
	}
	return gallery
}

func parseAwards(d map[string]interface{}) Awards {
	allAwardings := getArray(d, "all_awardings")
	if len(allAwardings) == 0 {
		return nil
	}
	awards := make(Awards, 0, len(allAwardings))
	for _, a := range allAwardings {
		aMap, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		iconURL := ""
		if icons := getArray(aMap, "resized_icons"); len(icons) > 0 {
			if icon, ok := icons[0].(map[string]interface{}); ok {
				iconURL = FormatURL(getString(icon, "url"))
			}
		}
		awards = append(awards, Award{
			Name:        getString(aMap, "name"),
			IconURL:     iconURL,
			Description: getString(aMap, "description"),
			Count:       getInt64(aMap, "count"),
		})
	}
	return awards
}

func parsePoll(d map[string]interface{}) *Poll {
	pollData := getMap(d, "poll_data")
	if pollData == nil {
		return nil
	}

	poll := &Poll{
		TotalVoteCount: uint64(getInt64(pollData, "total_vote_count")),
	}

	if endTS := getFloat64(pollData, "voting_end_timestamp"); endTS > 0 {
		poll.VotingEndTimestamp[0], poll.VotingEndTimestamp[1] = FormatTime(endTS / 1000)
	}

	options := getArray(pollData, "options")
	poll.Options = make([]PollOption, 0, len(options))
	for _, o := range options {
		oMap, ok := o.(map[string]interface{})
		if !ok {
			continue
		}
		opt := PollOption{
			Text: getString(oMap, "text"),
		}
		if id := getFloat64(oMap, "id"); id > 0 {
			opt.ID = uint64(id)
		}
		if vc, exists := oMap["vote_count"]; exists && vc != nil {
			if vcf, ok := vc.(float64); ok {
				count := uint64(vcf)
				opt.VoteCount = &count
			}
		}
		poll.Options = append(poll.Options, opt)
	}
	return poll
}

func parseFlair(d map[string]interface{}, prefix string) Flair {
	f := Flair{
		Text:            getString(d, prefix+"_text"),
		BackgroundColor: getString(d, prefix+"_background_color"),
	}
	// Reddit uses "transparent" as its sentinel for "no flair background"; treat
	// it as absent so the pill falls back to the themed var(--accent) instead of
	// rendering a transparent box (which would expose hardcoded black text on a
	// dark page).
	if f.BackgroundColor == "transparent" {
		f.BackgroundColor = ""
	}
	// Only pin a hardcoded contrast color when the flair carries its own
	// background. With no custom background the pill falls back to the themed
	// var(--accent)/var(--background) CSS, so leaving the foreground empty lets
	// the text color follow the active theme instead of being stuck on a fixed
	// black/white that mismatches dark themes.
	if f.BackgroundColor != "" {
		textColor := getString(d, prefix+"_text_color")
		if textColor == "dark" {
			f.ForegroundColor = "black"
		} else {
			f.ForegroundColor = "white"
		}
	}

	flairType := getString(d, prefix+"_type")
	switch flairType {
	case "richtext":
		if richFlair := getArray(d, prefix+"_richtext"); len(richFlair) > 0 {
			f.FlairParts = make([]FlairPart, 0, len(richFlair))
			for _, part := range richFlair {
				pMap, ok := part.(map[string]interface{})
				if !ok {
					continue
				}
				e := getString(pMap, "e")
				switch e {
				case "text":
					f.FlairParts = append(f.FlairParts, FlairPart{
						Type:  "text",
						Value: getString(pMap, "t"),
					})
				case "emoji":
					f.FlairParts = append(f.FlairParts, FlairPart{
						Type:  "emoji",
						Value: FormatURL(getString(pMap, "u")),
					})
				}
			}
		}
	case "text":
		if f.Text != "" {
			f.FlairParts = []FlairPart{{Type: "text", Value: f.Text}}
		}
	}
	return f
}

// ParsePostPage parses a post detail page JSON response.
// Reddit returns a 2-element array: [post listing, comment listing].
func ParsePostPage(data []byte) (Post, []Comment, error) {
	var listings []json.RawMessage
	if err := json.Unmarshal(data, &listings); err != nil {
		return Post{}, nil, fmt.Errorf("parse post page: %w", err)
	}
	if len(listings) < 2 {
		return Post{}, nil, fmt.Errorf("expected 2 listings, got %d", len(listings))
	}

	// First listing: the post
	var postListing struct {
		Data struct {
			Children []json.RawMessage `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listings[0], &postListing); err != nil {
		return Post{}, nil, fmt.Errorf("parse post listing: %w", err)
	}
	if len(postListing.Data.Children) == 0 {
		return Post{}, nil, fmt.Errorf("no post in listing")
	}
	post, err := ParsePost(postListing.Data.Children[0])
	if err != nil {
		return Post{}, nil, err
	}

	// Second listing: comments
	comments := ParseComments(listings[1], post.Permalink, post.Author.Name)

	return post, comments, nil
}

// ParseComments recursively parses a comment listing.
// Handles the case where replies is an empty string "".
func ParseComments(data json.RawMessage, postLink, postAuthor string) []Comment {
	var listing struct {
		Data struct {
			Children []json.RawMessage `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &listing); err != nil {
		return nil
	}

	comments := make([]Comment, 0, len(listing.Data.Children))
	for _, child := range listing.Data.Children {
		c := buildComment(child, postLink, postAuthor)
		comments = append(comments, c)
	}
	return comments
}

// ParseMoreChildren parses Reddit's /api/morechildren.json response — a FLAT
// list of comment "things" addressing exactly the child IDs we requested. We
// rebuild the tree client-side using parent_id so each item nests under its
// real parent: any item whose parent is also in the response becomes a reply
// of that parent (preserving Reddit's inline expansion of grandchildren),
// while items whose parent lives upstream (the original "more" stub's parent,
// not in this payload) surface as roots — those are what the caller splices
// into the visible DOM next to the loaded button.
func ParseMoreChildren(data []byte, postLink, postAuthor string) ([]Comment, error) {
	var resp struct {
		JSON struct {
			Errors [][]interface{} `json:"errors"`
			Data   struct {
				Things []json.RawMessage `json:"things"`
			} `json:"data"`
		} `json:"json"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	flat := make([]*Comment, 0, len(resp.JSON.Data.Things))
	for _, raw := range resp.JSON.Data.Things {
		c := buildComment(raw, postLink, postAuthor)
		if c.ID == "" && c.Kind != "more" {
			continue
		}
		cp := c
		flat = append(flat, &cp)
	}

	byID := make(map[string]bool, len(flat))
	for _, c := range flat {
		if c.ID != "" {
			byID[c.ID] = true
		}
	}

	// Two-pass tree build: index children by parent ID, then DFS-copy each
	// root with its full subtree. Avoids the value-copy-vs-late-attach trap
	// of single-pass mutation on []Comment slices — by the time we copy a
	// node into its parent's Replies, the parent→children index is already
	// complete so the recursive walk picks up every descendant.
	childrenOf := make(map[string][]*Comment, len(flat))
	roots := []*Comment{}
	for _, c := range flat {
		if c.ParentKind == "t1" && byID[c.ParentID] {
			childrenOf[c.ParentID] = append(childrenOf[c.ParentID], c)
			continue
		}
		roots = append(roots, c)
	}

	// seen tracks the IDs on the current DFS path so a cyclic parent graph in the
	// (untrusted) upstream payload — e.g. A.parent=B, B.parent=A — cannot drive
	// build into unbounded recursion and overflow the goroutine stack (a fatal,
	// unrecoverable crash, unlike a normal panic). Each node has exactly one
	// parent slot, so in an acyclic forest no node is ever revisited and this
	// never truncates a legitimate tree.
	seen := make(map[string]bool, len(flat))
	var build func(c *Comment) Comment
	build = func(c *Comment) Comment {
		out := *c
		if seen[c.ID] {
			return out
		}
		kids := childrenOf[c.ID]
		out.Replies = make([]Comment, 0, len(kids))
		if len(kids) == 0 {
			return out
		}
		seen[c.ID] = true
		for _, k := range kids {
			out.Replies = append(out.Replies, build(k))
		}
		delete(seen, c.ID)
		return out
	}

	result := make([]Comment, 0, len(roots))
	for _, r := range roots {
		result = append(result, build(r))
	}
	return result, nil
}

func buildComment(raw json.RawMessage, postLink, postAuthor string) Comment {
	var wrapper struct {
		Kind string                 `json:"kind"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return Comment{}
	}

	d := wrapper.Data
	c := Comment{
		ID:         getString(d, "id"),
		PostLink:   postLink,
		PostAuthor: postAuthor,
	}

	if wrapper.Kind == "more" {
		c.Kind = "more"
		c.MoreCount = getInt64(d, "count")
		c.ParentKind, c.ParentID = ParseParentID(getString(d, "parent_id"))
		if childrenRaw, ok := d["children"].([]interface{}); ok {
			c.Children = make([]string, 0, len(childrenRaw))
			for _, id := range childrenRaw {
				if s, ok := id.(string); ok && s != "" {
					c.Children = append(c.Children, s)
				}
			}
		}
		return c
	}

	c.Kind = "t1"

	// Parent
	c.ParentKind, c.ParentID = ParseParentID(getString(d, "parent_id"))

	// Author
	c.Author = Author{
		Name:          getString(d, "author"),
		Distinguished: getString(d, "distinguished"),
	}
	c.Author.Flair = parseFlair(d, "author_flair")

	// Score
	if getBool(d, "score_hidden") {
		c.Score = [2]string{"•", "Hidden"}
	} else {
		c.Score = FormatNum(getInt64(d, "score"))
	}

	// Time
	createdUTC := getFloat64(d, "created_utc")
	c.RelTime, c.Created = FormatTime(createdUTC)

	// Edited
	if edited, ok := d["edited"].(float64); ok && edited > 0 {
		c.Edited[0], c.Edited[1] = FormatTime(edited)
	}

	// Body
	bodyHTML := getString(d, "body_html")
	authorName := c.Author.Name

	if authorName == "[deleted]" && strings.Contains(bodyHTML, "[removed]") {
		c.Removed = true
		bodyHTML = `<p>[removed]</p>`
	} else if strings.Contains(bodyHTML, "[ Removed by Reddit ]") {
		c.Removed = true
		bodyHTML = `<p>[Removed by Reddit]</p>`
	} else if authorName == "[deleted]" && strings.Contains(bodyHTML, "[deleted]") {
		c.Removed = true
	} else {
		// Rewrite emotes if media_metadata present
		if mm := getMap(d, "media_metadata"); mm != nil {
			bodyHTML = RewriteEmotes(mm, bodyHTML)
		}
	}
	bodyHTML = RewriteURLs(bodyHTML)
	bodyHTML = EmbedCommentImages(bodyHTML)
	c.Body = template.HTML(bodyHTML)

	// Collapsed
	if getString(d, "distinguished") == "moderator" && getBool(d, "stickied") {
		c.Collapsed = true
	}

	// Awards
	c.Awards = parseAwards(d)

	// Replies — key: may be "" (empty string) instead of object
	if repliesRaw, ok := d["replies"]; ok && repliesRaw != nil {
		switch v := repliesRaw.(type) {
		case string:
			// Empty string means no replies
		case map[string]interface{}:
			repliesJSON, err := json.Marshal(v)
			if err == nil {
				c.Replies = ParseComments(repliesJSON, postLink, postAuthor)
			}
		}
	}

	return c
}

// ParseSubredditAbout parses a /r/{sub}/about.json response.
func ParseSubredditAbout(data []byte) (Subreddit, error) {
	var wrapper struct {
		Kind string                 `json:"kind"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return Subreddit{}, fmt.Errorf("parse subreddit about: %w", err)
	}

	d := wrapper.Data
	sub := Subreddit{
		Name:        getString(d, "display_name"),
		Title:       getString(d, "title"),
		Description: getString(d, "public_description"),
		Wiki:        getBool(d, "wiki_enabled"),
		NSFW:        getBool(d, "over18"),
	}

	// Info (description_html)
	if infoHTML := getString(d, "description_html"); infoHTML != "" {
		sub.Info = template.HTML(RewriteURLs(infoHTML))
	}

	// Icon: community_icon first, fallback to icon_img
	icon := getString(d, "community_icon")
	if icon == "" {
		icon = getString(d, "icon_img")
	}
	sub.RawIcon = icon
	sub.Icon = FormatURL(icon)

	// Members
	sub.Members = FormatNum(getInt64(d, "subscribers"))
	sub.Active = FormatNum(getInt64(d, "accounts_active"))

	return sub, nil
}

// ParseUserAbout parses a /user/{name}/about.json response.
func ParseUserAbout(data []byte) (User, error) {
	var wrapper struct {
		Kind string                 `json:"kind"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return User{}, fmt.Errorf("parse user about: %w", err)
	}

	d := wrapper.Data
	u := User{
		Name:        getString(d, "name"),
		Title:       getString(d, "subreddit_title"),
		Karma:       getInt64(d, "total_karma"),
		Description: getString(d, "subreddit_description"),
		NSFW:        getBool(d, "subreddit_over_18"),
	}

	u.Icon = FormatURL(getString(d, "icon_img"))
	u.Banner = FormatURL(getString(d, "subreddit_banner_img"))

	if createdUTC := getFloat64(d, "created_utc"); createdUTC > 0 {
		_, u.Created = FormatTime(createdUTC)
	}

	return u, nil
}

// ParseSearchResults parses a search result JSON response.
func ParseSearchResults(data []byte) ([]Post, []Subreddit, string, string, error) {
	// Search can return a listing or mixed results
	var raw struct {
		Kind string `json:"kind"`
		Data struct {
			Before   string            `json:"before"`
			After    string            `json:"after"`
			Children []json.RawMessage `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, "", "", fmt.Errorf("parse search: %w", err)
	}

	var posts []Post
	var subs []Subreddit

	for _, child := range raw.Data.Children {
		var peek struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(child, &peek) != nil {
			continue
		}

		switch peek.Kind {
		case "t3":
			if p, err := ParsePost(child); err == nil {
				posts = append(posts, p)
			}
		case "t5":
			var subWrapper struct {
				Data map[string]interface{} `json:"data"`
			}
			if json.Unmarshal(child, &subWrapper) == nil && subWrapper.Data != nil {
				d := subWrapper.Data
				sub := Subreddit{
					Name:        getString(d, "display_name"),
					Title:       getString(d, "title"),
					Description: getString(d, "public_description"),
					NSFW:        getBool(d, "over18"),
				}
				sub.Icon = FormatURL(getString(d, "community_icon"))
				if sub.Icon == "" {
					sub.Icon = FormatURL(getString(d, "icon_img"))
				}
				sub.Members = FormatNum(getInt64(d, "subscribers"))
				sub.Active = FormatNum(getInt64(d, "accounts_active"))
				subs = append(subs, sub)
			}
		}
	}

	return posts, subs, raw.Data.Before, raw.Data.After, nil
}

// ParseUserListing parses a user listing (posts/comments) JSON response.
func ParseUserListing(data []byte) ([]Post, []Comment, string, string, error) {
	var raw struct {
		Data struct {
			Before   string            `json:"before"`
			After    string            `json:"after"`
			Children []json.RawMessage `json:"children"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, nil, "", "", fmt.Errorf("parse user listing: %w", err)
	}

	posts := make([]Post, 0, len(raw.Data.Children))
	comments := make([]Comment, 0, len(raw.Data.Children))

	for _, child := range raw.Data.Children {
		var peek struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(child, &peek) != nil {
			continue
		}

		switch peek.Kind {
		case "t3":
			if p, err := ParsePost(child); err == nil {
				posts = append(posts, p)
			}
		case "t1":
			c := buildComment(child, "", "")
			comments = append(comments, c)
		}
	}

	return posts, comments, raw.Data.Before, raw.Data.After, nil
}

// --- JSON helper functions ---

func getString(d map[string]interface{}, key string) string {
	if v, ok := d[key].(string); ok {
		return v
	}
	return ""
}

func getBool(d map[string]interface{}, key string) bool {
	if v, ok := d[key].(bool); ok {
		return v
	}
	return false
}

func getInt64(d map[string]interface{}, key string) int64 {
	switch v := d[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

func getFloat64(d map[string]interface{}, key string) float64 {
	switch v := d[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	}
	return 0
}

func getNestedInt64(d map[string]interface{}, key string) int64 {
	return getInt64(d, key)
}

func getMap(d map[string]interface{}, key string) map[string]interface{} {
	if v, ok := d[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

func getArray(d map[string]interface{}, key string) []interface{} {
	if v, ok := d[key].([]interface{}); ok {
		return v
	}
	return nil
}
