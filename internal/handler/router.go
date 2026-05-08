package handler

import (
	"net/http"
	"sync"
	"time"

	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/media"
	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
)

type Handler struct {
	proxy        *proxy.Proxy
	ratelimit    *ratelimit.Manager
	cache        *cache.Cache
	renderer     *render.Engine
	redditCli    *reddit.Client
	postStore    *store.PostStore
	commentStore *store.CommentStore
	subStore     *store.SubredditStore
	mediaProxy   *media.Proxy
	cfg          *config.Config

	// Redlib health check cache
	healthMu        sync.RWMutex
	healthAlive     bool
	healthCheckedAt time.Time
}

func New(
	p *proxy.Proxy,
	rl *ratelimit.Manager,
	c *cache.Cache,
	r *render.Engine,
	rc *reddit.Client,
	ps *store.PostStore,
	cs *store.CommentStore,
	ss *store.SubredditStore,
	mp *media.Proxy,
	cfg *config.Config,
) *Handler {
	return &Handler{
		proxy:        p,
		ratelimit:    rl,
		cache:        c,
		renderer:     r,
		redditCli:    rc,
		postStore:    ps,
		commentStore: cs,
		subStore:     ss,
		mediaProxy:   mp,
		cfg:          cfg,
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

	// Media proxy
	mux.HandleFunc("GET /proxy/media", h.handleMedia)

	// Redlib media paths — proxy directly to redlib
	mux.HandleFunc("GET /img/", h.handleRedlibMedia)
	mux.HandleFunc("GET /preview/", h.handleRedlibMedia)
	mux.HandleFunc("GET /thumb/", h.handleRedlibMedia)
	mux.HandleFunc("GET /emoji/", h.handleRedlibMedia)
	mux.HandleFunc("GET /touch-icon-iphone.png", h.handleRedlibMedia)
	mux.HandleFunc("GET /check_update.js", h.handleRedlibMedia)

	// Video proxy — v.redd.it
	mux.HandleFunc("GET /vid/", h.handleVideoProxy)
	mux.HandleFunc("GET /hls/", h.handleVideoProxy)

	// Page routes
	mux.HandleFunc("GET /{$}", h.handleFrontPage)
	mux.HandleFunc("GET /r/{sub}", h.handleSubreddit)
	mux.HandleFunc("GET /r/{sub}/{sort}", h.handleSubredditSort)
	mux.HandleFunc("GET /r/{sub}/comments/{id}/{title...}", h.handlePost)
	mux.HandleFunc("GET /user/{name}", h.handleUser)
	mux.HandleFunc("GET /user/{name}/{listing}", h.handleUser)
	mux.HandleFunc("GET /search", h.handleSearch)
	mux.HandleFunc("GET /r/{sub}/search", h.handleSubSearch)

	// Settings
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("POST /settings", h.handleSettingsSave)
	mux.HandleFunc("GET /settings/restore", h.handleSettingsRestore)

	// Subscriptions / filters (cookie operations)
	mux.HandleFunc("POST /r/{sub}/subscribe", h.handleSubscribe)
	mux.HandleFunc("POST /r/{sub}/unsubscribe", h.handleUnsubscribe)
	mux.HandleFunc("POST /r/{sub}/filter", h.handleFilter)
	mux.HandleFunc("POST /r/{sub}/unfilter", h.handleUnfilter)

	// Wiki
	mux.HandleFunc("GET /r/{sub}/wiki/{page...}", h.handleWiki)

	return h.applyMiddleware(mux)
}
