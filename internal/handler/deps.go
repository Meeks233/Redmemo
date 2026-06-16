package handler

import (
	"context"
	"time"

	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/hrlimit"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/store"
)

// Narrow interfaces for servePost's dependency chain, sized to the union of
// methods the handler package actually calls. Existing concrete types
// (*store.PostStore, *store.CommentStore, *cache.Cache, *reddit.Client,
// *oauth.TokenHolder, *archive.Service, *hrlimit.Manager) satisfy these.
// They exist so the post-removed fallback chain can be exercised end-to-end
// with in-memory fakes — see post_test.go.

type postStorer interface {
	Get(urlPath string) (*store.StoredPost, error)
	MarkUpstreamRemoved(urlPath string) error
	SaveHTML(urlPath string, html []byte) error
	ArchiveSearch(opts store.ArchiveSearchOpts) ([]*store.StoredPost, int64, error)
	ArchivedSubsByTop(minPosts int) ([]store.ArchivedSub, error)
	ArchivedSubsAlphabetical() ([]store.ArchivedSub, error)
	ArchivedSubsByNew(minPosts int) ([]store.ArchivedSub, error)
	DetectNSFWForSubs(names []string) (map[string]bool, error)
	ListBySubreddit(sub string, limit, offset int, excludeNSFW bool) ([]*store.StoredPost, error)
	CountBySubreddit(sub string, excludeNSFW bool) (int64, error)
	ListHomepage(sort string, opts store.ArchiveSearchOpts) ([]*store.StoredPost, error)
	RandomWalk(opts store.ArchiveSearchOpts, mediaOnly bool, origin, cursor float64, n int) ([]*store.StoredPost, float64, bool, error)
	Reshuffle() error
	SubredditCounts(names []string) (map[string]int, error)
	Count() (int64, error)
	SubredditCount() (int64, error)
	SubredditStats(minPosts, limit int) ([]store.SubredditStat, error)
	DistinctSubreddits() ([]string, error)
}

type commentStorer interface {
	GetLatest(postURLPath string) (*store.StoredComments, error)
}

type htmlCache interface {
	GetHTML(ctx context.Context, key string) ([]byte, error)
	PutHTML(ctx context.Context, key string, html []byte, ttl time.Duration) error
	InvalidateHTMLPrefix(ctx context.Context, urlPath string) error
	InvalidateAllHTML(ctx context.Context) error
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

type redditClient interface {
	FetchPost(ctx context.Context, sub, id, commentSort string) (reddit.Post, []reddit.Comment, error)
	FetchPostLimited(ctx context.Context, sub, id, commentSort string, limit int) (reddit.Post, []reddit.Comment, error)
	FetchMoreChildren(ctx context.Context, sub, postID string, childrenIDs []string, commentSort string) ([]reddit.Comment, error)
	FetchSubreddit(ctx context.Context, sub, sort, t, after, before string, limit int) ([]reddit.Post, string, string, error)
	FetchSearch(ctx context.Context, query, sub, sort, t, after, before string, limit int) ([]reddit.Post, []reddit.Subreddit, string, string, error)
	FetchSubredditAbout(ctx context.Context, sub string) (reddit.Subreddit, error)
	FetchUser(ctx context.Context, username, listing, sort, after string) (reddit.User, []reddit.Post, []reddit.Comment, error)
}

type tokenSource interface {
	HasAvailableTokens() bool
	WaitForUserAgent(ctx context.Context) string
	EarliestReset() (int, int)
	RemainingBudget(ctx context.Context) (int, error)
	TokenStatuses() []oauth.TokenStatusInfo
	WindowInfo() (resetAt time.Time, capacity int, remaining int)
}

type archiverService interface {
	ArchivePost(post *reddit.Post, subreddit, source string)
	ArchiveComments(postURLPath string, comments []reddit.Comment)
	ArchivePosts(posts []reddit.Post, subreddit, source string)
	ArchiveSubreddit(sub *reddit.Subreddit)
	SetControlFromString(raw string)
}

type hrManager interface {
	Admit(ctx context.Context) (admitted bool, reason string)
	RecordUpstream(ctx context.Context)
	CooldownReason(ctx context.Context) (reason string, untilUnix int64)
	RedisDownReset(ctx context.Context) (down bool, untilUnix int64)
}

// trustedDeviceStore is the slice of *store.TrustedDeviceStore the auth gate
// uses to mint, validate, list, revoke and sweep "Trust this device" long
// tokens. Declared consumer-side so AuthManager's logic (cap enforcement,
// revoke-invalidates-immediately, expiry cleanup) is unit-testable against an
// in-memory fake — see trusted_device_test.go.
type trustedDeviceStore interface {
	CountActive() (int, error)
	Create(tokenHash, prefix, ip string, expiresAt time.Time) error
	Check(tokenHash, clientIP string, newExpiry time.Time) (store.TrustVerdict, error)
	ListActive() ([]store.TrustedDevice, error)
	HashByID(id int64) (string, error)
	Revoke(id int64) (int64, error)
	DeleteExpired() (int64, error)
	DeleteAll() (int64, error)
}

// Compile-time guards that concrete production types still satisfy the
// interfaces above. If a future method removal breaks the contract these
// fail at build time instead of at runtime in a route handler.
var (
	_ postStorer      = (*store.PostStore)(nil)
	_ commentStorer   = (*store.CommentStore)(nil)
	_ redditClient    = (*reddit.Client)(nil)
	_ tokenSource     = (*oauth.TokenHolder)(nil)
	_ archiverService    = (*archive.Service)(nil)
	_ htmlCache          = (*cache.Cache)(nil)
	_ hrManager          = (*hrlimit.Manager)(nil)
	_ trustedDeviceStore = (*store.TrustedDeviceStore)(nil)
)
