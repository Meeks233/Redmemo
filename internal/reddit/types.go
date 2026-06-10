package reddit

import "html/template"

// Post represents a Reddit post with all display-ready fields.
type Post struct {
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	Community       string        `json:"subreddit"`
	Body            template.HTML // selftext_html after rewrite_urls()
	Author          Author        `json:"author"`
	Permalink       string        `json:"permalink"`
	LinkTitle       string        `json:"link_title"`
	Poll            *Poll         `json:"poll"`
	Score           [2]string     // [formatted, raw] via FormatNum
	UpvoteRatio     int64         // upvote_ratio * 100
	PostType        string        // "video"|"gif"|"image"|"self"|"gallery"|"link"
	Flair           Flair
	Flags           Flags
	Thumbnail       Media
	Media           Media
	Domain          string    `json:"domain"`
	RelTime         string    // relative time "3h ago"
	Created         string    // absolute time "May 08 2026, 12:00:00 UTC"
	CreatedTS       uint64    // Unix timestamp
	ArchivedRelTime string    // relative archive time "2d ago"
	ArchivedTime    string    // absolute archive time
	NumDuplicates   uint64    `json:"num_duplicates"`
	Comments        [2]string // [formatted, raw] via FormatNum
	Gallery         []GalleryMedia
	Awards          Awards
	NSFW            bool    `json:"over_18"`
	OutURL          *string // url_overridden_by_dest
	WSURL           string  `json:"websocket_url"`
	// Removed is set when Reddit's response marks the post as removed/deleted
	// (removed_by_category set, or selftext == "[removed]"/"[deleted]"). The
	// archive layer uses it to skip overwriting a previously-good local copy,
	// and the renderer uses it to show the Time Machine badge.
	Removed bool `json:"removed,omitempty"`
	// RepostCount is the number of posts in this post's repost cluster — same
	// content surfacing again and again under near-identical titles, whether
	// by one spammer or syndicated by many accounts. Computed at fetch /
	// archive-query time via FoldReposts (and the matching SQL DISTINCT ON
	// repost_key path), never archived to json_data, never parsed from
	// upstream. 0 ⇒ unfolded; 1 ⇒ singleton; >1 ⇒ N variants exist and the
	// renderer expands the card to show each one's per-sub navigation row.
	RepostCount int `json:"-"`
	// RepostMembers carries one row per cluster variant (including the head
	// itself at index 0) — each row's permalink, sub, title, author, score
	// stays navigable so the user can jump to any specific repost. The
	// shared media renders ONCE via the head's Media / Gallery; only the
	// per-row navigation text is duplicated, so no single asset is fetched
	// more than once when the cluster expands.
	RepostMembers []RepostMember `json:"-"`
}

// RepostMember is one navigable variant inside a repost cluster (FoldReposts
// output). The cluster head's own data appears at RepostMembers[0]; later
// entries are the folded-away duplicates surfaced as per-sub links.
type RepostMember struct {
	Community string
	Permalink string
	Title     string
	Author    Author
	Score     [2]string
	RelTime   string
	Created   string
}

// Comment represents a Reddit comment with recursive replies.
type Comment struct {
	ID          string
	Kind        string // "t1" = comment, "more" = load more
	ParentID    string // parsed from "t1_xxx" → "xxx"
	ParentKind  string // parsed from "t1_xxx" → "t1"
	PostLink    string
	PostAuthor  string
	Body        template.HTML // body_html after rewrite_emotes() + rewrite_urls()
	Author      Author
	Score       [2]string
	RelTime     string
	Created     string
	Edited      [2]string // (relative, absolute); empty if not edited
	Replies     []Comment // recursive
	Highlighted bool      // id matches URL comment_id
	Awards      Awards
	Collapsed   bool // stickied mod comment or filtered user
	IsFiltered  bool
	MoreCount   int64    // descendant count when kind=="more"
	Children    []string // child IDs the "more" stub represents (kind=="more" only)
	Prefs       Preferences
	// Removed is set when this comment's body was removed/deleted upstream
	// ("[removed]", "[ Removed by Reddit ]" or author=="[deleted]" with empty
	// body). The renderer shows a Time Machine badge so the local row stays
	// visible while flagging that the upstream copy is gone.
	Removed bool
}

// Author represents a post or comment author.
type Author struct {
	Name          string
	Flair         Flair
	Distinguished string // "", "moderator", "admin"
}

// Flair represents a post or author flair with optional rich text parts.
type Flair struct {
	FlairParts      []FlairPart
	Text            string
	BackgroundColor string
	ForegroundColor string // "dark" → "black", otherwise "white"
}

// FlairPart is a single component of a rich-text flair.
type FlairPart struct {
	Type  string // "text" | "emoji"
	Value string // text content or emoji image URL (after format_url)
}

// Flags holds boolean markers for a post.
type Flags struct {
	Spoiler  bool // data.spoiler
	NSFW     bool // data.over_18
	Stickied bool // data.stickied || data.pinned
}

// Media holds a media reference (image, video, thumbnail).
type Media struct {
	URL          string
	AltURL       string // HLS URL (video fallback)
	Width        int64
	Height       int64
	Poster       string // video poster image
	Duration     float64
}

// GalleryMedia represents one item in a gallery post.
type GalleryMedia struct {
	URL         string
	Width       int64  // metadata[media_id].s.x
	Height      int64  // metadata[media_id].s.y
	Caption     string // item.caption
	OutboundURL string // item.outbound_url
}

// Poll represents a Reddit poll attached to a post.
type Poll struct {
	Options            []PollOption
	VotingEndTimestamp [2]string // (relative, absolute) via FormatTime
	TotalVoteCount     uint64
}

// PollOption is a single poll choice.
type PollOption struct {
	ID        uint64
	Text      string
	VoteCount *uint64 // nil when voting is still open
}

// Award represents a Reddit award on a post or comment.
type Award struct {
	Name        string
	IconURL     string // resized_icons[0].url after format_url()
	Description string
	Count       int64
}

// Awards is a typed slice of Award for attaching methods.
type Awards []Award

// Subreddit holds subreddit metadata.
type Subreddit struct {
	Name        string // display_name
	Title       string
	Description string        // public_description
	Info        template.HTML // description_html after rewrite_urls()
	Icon        string        // community_icon, fallback icon_img, after format_url()
	RawIcon     string        // original icon URL before format_url()
	Members     [2]string     // FormatNum(subscribers)
	Active      [2]string     // FormatNum(accounts_active)
	Wiki        bool          // wiki_enabled
	NSFW        bool          // over18
}

// User holds Reddit user profile data.
type User struct {
	Name        string
	Title       string
	Icon        string
	Karma       int64
	Created     string
	Banner      string
	Description string
	NSFW        bool
}

// Preferences stores user display preferences read from cookies.
type Preferences struct {
	AvailableThemes                []string // derived from embedded CSS filenames
	Theme                          string
	AutoThemeDay                   string // theme woken under prefers-color-scheme: light (default "light"); only used when Theme=="auto"
	AutoThemeNight                 string // theme woken under prefers-color-scheme: dark  (default "black"); only used when Theme=="auto"
	Lang                           string // UI language code (e.g. "en", "zh")
	FrontPage                      string
	FrontPageSubs                  string
	Layout                         string
	Wide                           string
	BlurSpoiler                    string
	ShowNSFW                       string
	ShowLocalNSFWSubs              string // default "off" — when "off", NSFW subs are hidden from the archive nav (/archive) listing
	BlurNSFW                       string
	HideSidebarAndSummary          string
	AutoplayVideos                 string
	FixedNavbar                    string // default "on"
	DisableVisitRedditConfirmation string
	CommentSort                    string
	PostSort                       string
	Subscriptions                  []string // cookie value split by "+"
	Filters                        []string // cookie value split by "+"
	HideAwards                     string
	HideScore                      string
	RemoveDefaultFeeds             string
	FetchSubAbout                  string
	EnableDebug                    string
	EnableNaturalPrefetch          string
	PrefetchSubs                   string
	PrefetchThreshold              string
	PrefetchSort                   string // global default sort for NP L1: hot|new|top|rising|controversial (default "hot")
	PrefetchTimeframe              string // global default t for NP L1 (only honored by top/controversial): hour|day|week|month|year|all (empty = none)
	PrefetchSubModes               string // per-sub overrides, one rule per line: "sub=sort[:timeframe]" (e.g. "golang=new\nrust=top:week")
	PrefetchDefaultDepth           string // "none" | "l2" | "l3" | "l2+l3" (default "l2+l3"). Global NP depth: "none" leaves only L1 main fetch, "l2" adds media downloads, "l3" adds comment fetches without media, "l2+l3" runs the full visit-like flow. Per-sub overrides (prefetch_sub_modes) may set depth:... to deviate from this global default for a single subreddit.
	PrefetchL3MinComments          string // integer >= 0 (default "0" = disabled). Posts with num_comments below this value are skipped by both standalone and bound L3 — surface noise filter for archives that should not waste budget on threads of size 1.
	ScrollInterval                 string
	LazyMedia                      string // default "on" — defer media requests until the post enters the viewport
	VideoQuality                   string // preferred max v.redd.it height: "source" (default) | "1080" | "720" | "480" | "360" | "240"
	MuteAllVideos                  string // default "off" — start every video muted
	MuteNSFWVideos                 string // default "on"  — start NSFW videos muted (ignored when MuteAllVideos is on)
	DisableInitiativeUpstreamAccess string // default "off" — when "on", user-driven session-token requests never hit Reddit, only the local archive (CDN media still flows, governed by the global limiter)
	SettingsTokenTTL               string // /settings auth-cookie lifetime in minutes — discrete choices "5","10" (default),"15","30","60"; capped at 60
	PageLimit                      string // posts per upstream listing request (/r/{sub} + /search) — integer in [5, 100], default "50". Reddit's OAuth quota is per-request, so larger pages are strictly cheaper.
}

// Params holds common query parameters for listing endpoints.
type Params struct {
	T      string // timeframe: hour/day/week/month/year/all
	Q      string // search query
	Sort   string
	After  string
	Before string
}

// RateLimitInfo holds rate limit state parsed from Reddit API response headers.
type RateLimitInfo struct {
	Remaining float64
	Reset     int64
	Used      int64
}

// SubredditResponse wraps a subreddit listing fetch result.
type SubredditResponse struct {
	Posts  []Post
	Before string
	After  string
	Sub    Subreddit
}

// PostResponse wraps a post detail fetch result.
type PostResponse struct {
	Post     Post
	Comments []Comment
}

// UserResponse wraps a user profile fetch result.
type UserResponse struct {
	User   User
	Posts  []Post
	Before string
	After  string
}

// SearchResponse wraps a search result.
type SearchResponse struct {
	Posts      []Post
	Subreddits []Subreddit
	Before     string
	After      string
}

// SearchParams holds search-specific query parameters.
type SearchParams struct {
	Query      string
	Sort       string
	Timeframe  string
	After      string
	Before     string
	RestrictSR bool
	Type       string
}
