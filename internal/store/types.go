package store

import "time"

type StoredPost struct {
	URLPath      string
	Subreddit    string
	PostID       string
	Title        string
	JSONData     []byte
	RenderedHTML *string
	Author       string
	Score        int
	CreatedUTC   time.Time
	FirstSeen    time.Time
	LastUpdated  time.Time
	Source       string // "redlib_proxy" | "oauth_fallback" | "prefetch" | "natural_prefetch"
	MediaDone    bool
}

type StoredComments struct {
	PostURLPath  string
	JSONData     []byte
	CommentCount int
	FetchedAt    time.Time
}

type MediaMeta struct {
	OriginalURL  string
	Hash         string
	FilePath     *string
	MIMEType     string
	FileSize     int64
	FirstSeen    time.Time
	LastAccessed time.Time
	AccessCount  int64
	// AudioState is only meaningful for muxed video rows (key prefix "muxed:").
	// nil = never checked, "has_audio" = mux succeeded, "silent" = no audio
	// track exists on Reddit (skip mux), "failed" = audio mux failing and in
	// the L5 retry queue, "abandoned" = retries exhausted, no longer actively
	// retried. "failed"/"abandoned" rows carry an emergency silent copy.
	AudioState *string
	// AudioFailCount counts failed mux attempts; LastAudioAttemptAt is when
	// the most recent attempt finished. Both only meaningful for muxed rows.
	AudioFailCount     int
	LastAudioAttemptAt *time.Time
}

type StoredToken struct {
	ID            int
	ClientID      string
	ClientSecret  string
	AccessToken   string
	ExpiresAt     *time.Time
	RateRemaining *int
	RateResetAt   *time.Time
	Backend       string
	Enabled       bool
	LastUsed      *time.Time
	CreatedAt     time.Time
	HeadersJSON   *string
}

type StoredSubreddit struct {
	Name        string
	Title       string
	Description string
	IconURL     string
	Members     int
	JSONData    []byte
	LastUpdated time.Time
}

type SubredditStatus struct {
	Name      string
	Status    string // live, dead, private, quarantined, unknown
	Reason    string
	LastLive  time.Time
	FailCount int
	CheckedAt time.Time
	NSFW      *bool // nil = never evaluated; once true it stays true
}

type SubIcon struct {
	Name      string
	IconURL   string
	LocalPath *string
	Hash      *string
	FetchedAt time.Time
	ExpiresAt time.Time
	// About cache (separate expiry from icon). All nullable: zero values
	// mean "about has never been fetched".
	AboutJSON       []byte
	AboutFetchedAt  *time.Time
	AboutExpiresAt  *time.Time
}

type StoredPrefetchConfig struct {
	Subreddit     string
	SortBy        string
	MaxPages      int
	FetchComments bool
	FetchMedia    bool
	Priority      int
	Enabled       bool
}
