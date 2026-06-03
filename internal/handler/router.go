package handler

import (
	"net/http"
	"time"

	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/hrlimit"
	"github.com/redmemo/redmemo/internal/media"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/prefetch"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/transport"
)

type Handler struct {
	ratelimit      *ratelimit.Manager
	hr             *hrlimit.Manager
	cache          *cache.Cache
	renderer       *render.Engine
	redditCli      *reddit.Client
	publicCli      *reddit.PublicClient
	oauthHolder    *oauth.TokenHolder
	postStore      *store.PostStore
	commentStore   *store.CommentStore
	subStore       *store.SubredditStore
	mediaStore     *store.MediaIndexStore
	settingsStore  *store.SettingsStore
	mediaProxy     *media.Proxy
	archiver       *archive.Service
	prefetcher     *prefetch.Scheduler
	subStatusStore *store.SubStatusStore
	subIconStore   *store.SubIconStore
	// spoofedClient carries the uTLS/HTTP-2 Reddit-app fingerprint for the few
	// upstream fetches the handler makes directly (e.g. HLS manifest rewriting)
	// instead of going through reddit/media clients. Built once; never use
	// net/http.DefaultClient for Reddit-facing requests — a plain Go TLS stack is
	// a fingerprint mismatch against every other outbound request.
	spoofedClient tls_client.HttpClient
	cfg           *config.Config
	siteDefaults  map[string]string
	auth          *AuthManager
	stats         statsCache
	// upstreamFlight coalesces concurrent identical Reddit fetches so N parallel
	// requests for the same /r/sub/sort page (or /comments/id) only spend one
	// OAuth quota unit. See singleflight.go.
	upstreamFlight *singleFlight
}

// WithAuth wires the settings auth gate. Optional — when nil the gate is
// skipped (tests / dev setups without a server secret) but
// requireSettingsAuth will fail closed.
func (h *Handler) WithAuth(a *AuthManager) *Handler {
	h.auth = a
	return h
}

func New(
	rl *ratelimit.Manager,
	hr *hrlimit.Manager,
	c *cache.Cache,
	r *render.Engine,
	rc *reddit.Client,
	pc *reddit.PublicClient,
	op *oauth.TokenHolder,
	ps *store.PostStore,
	cs *store.CommentStore,
	ss *store.SubredditStore,
	ms *store.MediaIndexStore,
	sts *store.SettingsStore,
	mp *media.Proxy,
	arc *archive.Service,
	pf *prefetch.Scheduler,
	sss *store.SubStatusStore,
	sis *store.SubIconStore,
	cfg *config.Config,
) *Handler {
	defaults, _ := sts.GetAll()
	if defaults == nil {
		defaults = make(map[string]string)
	}
	return &Handler{
		ratelimit:      rl,
		hr:             hr,
		cache:          c,
		renderer:       r,
		redditCli:      rc,
		publicCli:      pc,
		oauthHolder:    op,
		postStore:      ps,
		commentStore:   cs,
		subStore:       ss,
		mediaStore:     ms,
		settingsStore:  sts,
		mediaProxy:     mp,
		archiver:       arc,
		prefetcher:     pf,
		subStatusStore: sss,
		subIconStore:   sis,
		spoofedClient:  transport.NewSpoofedClient(30 * time.Second),
		cfg:            cfg,
		siteDefaults:   defaults,
		upstreamFlight: newSingleFlight(),
	}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

	// SEO surfaces. robots.txt is always served (returns "Disallow: /" when
	// SEO.AllowIndexing is off); sitemap.xml 404s when off.
	mux.HandleFunc("GET /robots.txt", h.handleRobotsTxt)
	mux.HandleFunc("GET /sitemap.xml", h.handleSitemapXML)

	// Static assets
	static := h.staticHandler()
	mux.Handle("GET /style.css", static)
	mux.Handle("GET /themes/", static)
	mux.Handle("GET /favicon.ico", static)
	mux.Handle("GET /favicon.png", static)
	mux.Handle("GET /logo.png", static)
	mux.Handle("GET /logo.svg", static)
	mux.Handle("GET /apple-touch-icon.png", static)
	mux.Handle("GET /manifest.json", static)
	mux.Handle("GET /opensearch.xml", static)
	mux.Handle("GET /Inter.var.woff2", static)
	mux.Handle("GET /highlighted.js", static)
	mux.Handle("GET /copy.js", static)
	mux.Handle("GET /check_update.js", static)
	mux.Handle("GET /quotaRing.js", static)
	mux.Handle("GET /infiniteScroll.js", static)
	mux.Handle("GET /subPicker.js", static)
	mux.Handle("GET /videoAutoplay.js", static)
	mux.Handle("GET /videoPreload.js", static)
	mux.Handle("GET /lazyMedia.js", static)
	mux.Handle("GET /imageReload.js", static)
	mux.Handle("GET /audioSync.js", static)
	mux.Handle("GET /redditModal.js", static)
	mux.Handle("GET /searchAutocomplete.js", static)

	// One bundle for every media-page script (lazyMedia/videoAutoplay/
	// videoPreload/audioSync/imageReload) — see render.MediaBundle.
	mux.Handle("GET /media.bundle.js", h.mediaBundleHandler())

	// Media proxy
	mux.HandleFunc("GET /proxy/media", h.handleMedia)

	// Image CDN Proxy
	mux.HandleFunc("GET /img/", h.handleImageProxy)
	mux.HandleFunc("GET /preview/", h.handleImageProxy)
	mux.HandleFunc("GET /thumb/", h.handleImageProxy)
	mux.HandleFunc("GET /emoji/", h.handleImageProxy)
	mux.HandleFunc("GET /style/", h.handleImageProxy)

	// Video proxy — v.redd.it
	mux.HandleFunc("GET /vid/", h.handleVideoProxy)
	mux.HandleFunc("GET /hls/", h.handleVideoProxy)

	// Page routes
	mux.HandleFunc("GET /{$}", h.handleFrontPage)
	mux.HandleFunc("GET /r/{sub}", h.handleSubreddit)
	mux.HandleFunc("GET /r/{sub}/{sort}", h.handleSubredditSort)
	mux.HandleFunc("GET /r/{sub}/comments/{id}/{title...}", h.handlePost)
	mux.HandleFunc("GET /user/{name}/comments/{id}/{title...}", h.handleUserPost)
	mux.HandleFunc("POST /api/refresh/{sub}/{id}", h.handleRefreshPost)
	mux.HandleFunc("GET /user/{name}", h.handleUser)
	mux.HandleFunc("GET /user/{name}/{listing}", h.handleUser)
	mux.HandleFunc("GET /search", h.handleSearch)
	mux.HandleFunc("GET /r/{sub}/search", h.handleSubSearch)

	// Archive browser
	mux.HandleFunc("GET /archive", h.handleArchiveHub)
	mux.HandleFunc("GET /archive/r/{sub}", h.handleArchiveSub)

	// Settings — TOTP-gated. The same /settings POST endpoint serves both
	// auth submissions (when no valid ephemeral token is held) and the actual
	// settings save (when one is). The gate routes between the two.
	mux.HandleFunc("GET /settings", h.requireSettingsAuth(h.handleSettings))
	mux.HandleFunc("POST /settings", h.requireSettingsAuth(h.handleSettingsSave))

	// Wiki
	mux.HandleFunc("GET /r/{sub}/wiki/{page...}", h.handleWiki)

	// Random archived post (local-only, no upstream)
	mux.HandleFunc("GET /random", h.handleRandom)

	// Lightweight status check
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/probe-sub", h.handleProbeSub)
	mux.HandleFunc("GET /api/audio_status", h.handleAudioStatus)
	mux.HandleFunc("GET /api/audio_track", h.handleAudioTrack)
	mux.HandleFunc("GET /api/media_status", h.handleMediaStatus)

	// Legacy countdown redirect
	mux.HandleFunc("GET /countdown", func(w http.ResponseWriter, r *http.Request) {
		prefs := h.readPreferences(r)
		if prefs.EnableDebug == "on" {
			http.Redirect(w, r, "/debug", http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
		}
	})

	// Exhausted — no quota, no archive
	mux.HandleFunc("GET /fuckreddit", h.handleFuckReddit)

	// Debug page — guarded twice: (1) the EnableDebug user preference is the
	// public-facing kill switch (off → 303 to /settings inside handleDebug);
	// (2) when on, the same TOTP gate as /settings still applies so visitors
	// without the ephemeral cookie land on the digits-input page (unless
	// REDMEMO_AUTH_BYPASS is set, in which case the gate is short-circuited
	// instance-wide).
	mux.HandleFunc("GET /debug", h.requireSettingsAuth(h.handleDebug))

	// Redlib compatibility redirects
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings", http.StatusMovedPermanently)
	})

	return h.applyMiddleware(mux)
}
