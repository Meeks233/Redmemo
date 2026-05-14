package reddit

import "html/template"

// Post represents a Reddit post with all display-ready fields.
type Post struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	Community     string         `json:"subreddit"`
	Body          template.HTML  // selftext_html after rewrite_urls()
	Author        Author         `json:"author"`
	Permalink     string         `json:"permalink"`
	LinkTitle     string         `json:"link_title"`
	Poll          *Poll          `json:"poll"`
	Score         [2]string      // [formatted, raw] via FormatNum
	UpvoteRatio   int64          // upvote_ratio * 100
	PostType      string         // "video"|"gif"|"image"|"self"|"gallery"|"link"
	Flair         Flair
	Flags         Flags
	Thumbnail     Media
	Media         Media
	Domain        string         `json:"domain"`
	RelTime       string         // relative time "3h ago"
	Created       string         // absolute time "May 08 2026, 12:00:00 UTC"
	CreatedTS     uint64         // Unix timestamp
	ArchivedRelTime string       // relative archive time "2d ago"
	ArchivedTime    string       // absolute archive time
	NumDuplicates uint64         `json:"num_duplicates"`
	Comments      [2]string      // [formatted, raw] via FormatNum
	Gallery       []GalleryMedia
	Awards        Awards
	NSFW          bool           `json:"over_18"`
	OutURL        *string        // url_overridden_by_dest
	WSURL         string         `json:"websocket_url"`
}

// Comment represents a Reddit comment with recursive replies.
type Comment struct {
	ID          string
	Kind        string     // "t1" = comment, "more" = load more
	ParentID    string     // parsed from "t1_xxx" → "xxx"
	ParentKind  string     // parsed from "t1_xxx" → "t1"
	PostLink    string
	PostAuthor  string
	Body        template.HTML // body_html after rewrite_emotes() + rewrite_urls()
	Author      Author
	Score       [2]string
	RelTime     string
	Created     string
	Edited      [2]string  // (relative, absolute); empty if not edited
	Replies     []Comment  // recursive
	Highlighted bool       // id matches URL comment_id
	Awards      Awards
	Collapsed   bool       // stickied mod comment or filtered user
	IsFiltered  bool
	MoreCount   int64      // child count when kind=="more"
	Prefs       Preferences
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
	DownloadName string // "redlib_{permalink_base}_{media_url_base}"
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
	Options           []PollOption
	VotingEndTimestamp [2]string // (relative, absolute) via FormatTime
	TotalVoteCount    uint64
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
	Name        string    // display_name
	Title       string
	Description string    // public_description
	Info        template.HTML // description_html after rewrite_urls()
	Icon        string    // community_icon, fallback icon_img, after format_url()
	RawIcon     string    // original icon URL before format_url()
	Members     [2]string // FormatNum(subscribers)
	Active      [2]string // FormatNum(accounts_active)
	Wiki        bool      // wiki_enabled
	NSFW        bool      // over18
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
	FrontPage                      string
	FrontPageSubs                  string
	FrontPageSubsMode              string
	Layout                         string
	Wide                           string
	BlurSpoiler                    string
	ShowNSFW                       string
	BlurNSFW                       string
	HideHLSNotification            string
	VideoQuality                   string
	HideSidebarAndSummary          string
	UseHLS                         string
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
	EnableDebug                    string
	EnableNaturalPrefetch          string
	PrefetchSubs                   string
	PrefetchThreshold              string
	ScrollInterval                 string
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
