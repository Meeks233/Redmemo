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
	// UpstreamRemoved is sticky: once Reddit reports the post as removed/deleted
	// we never overwrite the local JSON again. The post page renders the
	// archived copy with a Time Machine badge and stops scheduling refetches.
	UpstreamRemoved bool
	// RepostCount is the size of this row's repost cluster — same author +
	// same normalized title — populated only by queries that GROUP / DISTINCT
	// ON repost_key (ArchiveSearch). 0 ⇒ the query didn't fold; 1 ⇒ singleton;
	// >1 ⇒ N copies were collapsed and the renderer shows a "+N reposts" badge.
	RepostCount int
}

type StoredComments struct {
	PostURLPath  string
	JSONData     []byte
	CommentCount int
	FetchedAt    time.Time
}

// L3RuminateCandidate is one post whose upstream-reported comment count has
// grown since the last successful L3 fetch — the "someone replied since we
// last archived" signal that re-admits an otherwise media-done / cycle-frozen
// post into the L3 batch (rumination). CurrentComments is the count L1's most
// recent scan stored in posts.json_data (Comments[1]); LastComments is the
// count recorded in the post's most recent ok L3 prefetch_runs payload. By
// construction CurrentComments > LastComments for every row the query returns.
type L3RuminateCandidate struct {
	Post            *StoredPost
	CurrentComments int
	LastComments    int
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
	// Score is the dynamic existence/eviction score (migration v22): a resident
	// file carries a value in [0,100] (higher = evict sooner), an absent one the
	// -1 "not physically cached" sentinel. Presence checks read Score >= 0
	// instead of stat()-ing the disk.
	Score float64
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
	// HasIcon distinguishes "upstream confirmed there is no icon" (false) from
	// "we have one or have never asked" (true). False is a sticky terminal
	// verdict: L4 refresh skips it forever. Transient fetch failures must NOT
	// touch this field — they leave the prior verdict in place.
	HasIcon bool
	// About cache (separate expiry from icon). All nullable: zero values
	// mean "about has never been fetched".
	AboutJSON      []byte
	AboutFetchedAt *time.Time
	AboutExpiresAt *time.Time
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
