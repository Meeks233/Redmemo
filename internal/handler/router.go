package handler

import (
	"net/http"

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
	"github.com/redmemo/redmemo/internal/useragent"
)

type Handler struct {
	ratelimit     *ratelimit.Manager
	hr            *hrlimit.Manager
	cache         *cache.Cache
	renderer      *render.Engine
	redditCli     *reddit.Client
	publicCli     *reddit.PublicClient
	oauthPool     *oauth.Pool
	postStore     *store.PostStore
	commentStore  *store.CommentStore
	subStore      *store.SubredditStore
	mediaStore    *store.MediaIndexStore
	settingsStore *store.SettingsStore
	mediaProxy     *media.Proxy
	archiver       *archive.Service
	prefetcher     *prefetch.Scheduler
	subStatusStore *store.SubStatusStore
	subIconStore   *store.SubIconStore
	uaPool         *useragent.Pool
	cfg            *config.Config
	siteDefaults   map[string]string
}

func New(
	rl *ratelimit.Manager,
	hr *hrlimit.Manager,
	c *cache.Cache,
	r *render.Engine,
	rc *reddit.Client,
	pc *reddit.PublicClient,
	op *oauth.Pool,
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
	uap *useragent.Pool,
	cfg *config.Config,
) *Handler {
	defaults, _ := sts.GetAll()
	if defaults == nil {
		defaults = make(map[string]string)
	}
	return &Handler{
		ratelimit:     rl,
		hr:            hr,
		cache:         c,
		renderer:      r,
		redditCli:     rc,
		publicCli:     pc,
		oauthPool:     op,
		postStore:     ps,
		commentStore:  cs,
		subStore:      ss,
		mediaStore:    ms,
		settingsStore: sts,
		mediaProxy:    mp,
		archiver:       arc,
		prefetcher:     pf,
		subStatusStore: sss,
		subIconStore:   sis,
		uaPool:         uap,
		cfg:           cfg,
		siteDefaults:  defaults,
	}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()

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
	mux.Handle("GET /hls.min.js", static)
	mux.Handle("GET /playHLSVideo.js", static)
	mux.Handle("GET /highlighted.js", static)
	mux.Handle("GET /copy.js", static)
	mux.Handle("GET /check_update.js", static)
	mux.Handle("GET /quotaRing.js", static)
	mux.Handle("GET /infiniteScroll.js", static)
	mux.Handle("GET /subPicker.js", static)
	mux.Handle("GET /videoAutoplay.js", static)
	mux.Handle("GET /videoPreload.js", static)
	mux.Handle("GET /lazyMedia.js", static)
	mux.Handle("GET /audioSync.js", static)
	mux.Handle("GET /redditModal.js", static)

	// Media proxy
	mux.HandleFunc("GET /proxy/media", h.handleMedia)

	// Legacy redlib media paths — reconstruct CDN URLs and serve via media proxy
	mux.HandleFunc("GET /img/", h.handleRedlibMedia)
	mux.HandleFunc("GET /preview/", h.handleRedlibMedia)
	mux.HandleFunc("GET /thumb/", h.handleRedlibMedia)
	mux.HandleFunc("GET /emoji/", h.handleRedlibMedia)

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

	// Settings
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("POST /settings", h.handleSettingsSave)

	// Wiki
	mux.HandleFunc("GET /r/{sub}/wiki/{page...}", h.handleWiki)

	// Random archived post (local-only, no upstream)
	mux.HandleFunc("GET /random", h.handleRandom)

	// Lightweight status check
	mux.HandleFunc("GET /api/status", h.handleStatus)
	mux.HandleFunc("GET /api/probe-sub", h.handleProbeSub)
	mux.HandleFunc("GET /api/audio_status", h.handleAudioStatus)

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

	// Debug error page preview
	mux.HandleFunc("GET /debug", h.handleDebug)

	// Redlib compatibility redirects
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/settings", http.StatusMovedPermanently)
	})

	return h.applyMiddleware(mux)
}
