package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/handler"
	"github.com/redmemo/redmemo/internal/hrlimit"
	"github.com/redmemo/redmemo/internal/legacy"
	"github.com/redmemo/redmemo/internal/media"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/prefetch"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/useragent"
	"github.com/redmemo/redmemo/internal/versionintel"
)

func main() {
	configPath := "config.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// 1. Load config
	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("config: loaded %s", cfg)

	// 2. Init PostgreSQL
	db, err := store.New(cfg.Postgres.DSN, cfg.Postgres.MaxOpenConns, cfg.Postgres.MaxIdleConns)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer db.Close()
	log.Println("postgres: connected and migrated")

	// 3. Init Redis
	redisCache, err := cache.New(cfg.Redis)
	if err != nil {
		log.Fatalf("redis: %v", err)
	}
	defer redisCache.Close()
	log.Println("redis: connected")

	// 4. Init stores
	postStore := store.NewPostStore(db)
	commentStore := store.NewCommentStore(db)
	mediaIndexStore := store.NewMediaIndexStore(db)
	tokenStore := store.NewTokenStore(db)
	deviceProfileStore := store.NewDeviceProfileStore(db)
	subStore := store.NewSubredditStore(db)
	subIconStore := store.NewSubIconStore(db)
	settingsStore := store.NewSettingsStore(db)

	// 5. Rebuild site_settings on every startup
	//    Priority: env_override > legacy_sync > existing KV > default
	//
	// Step A: upstream sync (writes with source="legacy_sync", won't overwrite env_override)
	if cfg.Legacy.SyncEnabled {
		result, err := legacy.SyncSettings(cfg.Legacy)
		if err != nil {
			log.Printf("legacy sync: %v (continuing with existing settings)", err)
		} else if result != nil {
			n, err := settingsStore.SetBatchIfLowerPriority(result.Settings, "legacy_sync")
			if err != nil {
				log.Printf("legacy sync: persist failed: %v", err)
			} else {
				log.Printf("legacy sync: %d settings from %s (%d updated in DB)",
					len(result.Settings), result.Source, n)
			}
		}
	}
	// Step B: env var overrides (always win — scans all REDMEMO_DEFAULT_* vars)
	envSettings := config.ScanExplicitSettings()
	// Demote DB rows whose env var was removed since last startup
	if demoted, err := settingsStore.DemoteOrphans(envSettings); err != nil {
		log.Printf("settings: demote orphans failed: %v", err)
	} else if demoted > 0 {
		log.Printf("settings: demoted %d env_override rows (env var removed)", demoted)
	}
	if len(envSettings) > 0 {
		if err := settingsStore.SetBatch(envSettings, "env_override"); err != nil {
			log.Printf("settings: failed to persist env overrides: %v", err)
		} else {
			log.Printf("settings: persisted %d env var overrides to DB", len(envSettings))
		}
	}

	// 6. Init shared context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 7. Init UA pool
	uaPool := useragent.NewPool(settingsStore)

	// 8. Init OAuth
	deviceProfile, err := oauth.ResolveDeviceProfile(deviceProfileStore)
	if err != nil {
		log.Fatalf("oauth: resolve device profile: %v", err)
	}
	log.Printf("oauth: pinned device profile (android=%d, app=%s, device=%s)",
		deviceProfile.AndroidVersion, deviceProfile.AppVersion, deviceProfile.DeviceID)
	oauthClient := oauth.NewClient(uaPool, deviceProfile)
	versionTracker := versionintel.NewTracker(&http.Client{Timeout: 15 * time.Second})
	oauthHolder := oauth.NewTokenHolder(cfg.OAuth, oauthClient, tokenStore, deviceProfileStore, versionTracker, redisCache, uaPool)

	// sessionUA returns the active OAuth session's bound User-Agent, blocking
	// through the cold-start window (see TokenHolder.WaitForUserAgent). Every
	// Reddit-facing client shares this one closure so a single identity drives
	// every outbound request; falling back to a browser UA pool would emit a
	// second UA from the session IP — a stealth tell. The browser UA pool is now
	// dead code on these paths.
	sessionUA := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return oauthHolder.WaitForUserAgent(ctx)
	}

	// 9. Init Reddit clients
	redditAdapter := &oauthAdapter{holder: oauthHolder}
	redditCli := reddit.NewClient(redditAdapter)
	publicCli := reddit.NewPublicClient(sessionUA)

	// 10. Init modules
	rateLimiter := ratelimit.New(cfg.RateLimit, oauthHolder)
	hrLimiter := hrlimit.NewManager(redisCache.Client(), cfg.HRLimit)

	renderer, err := render.New(cfg.Render)
	if err != nil {
		log.Fatalf("render: %v", err)
	}

	// Media fetches must reuse the active OAuth session's UA so a single
	// identity drives every Reddit-facing request. During the cold-start
	// window the closure blocks on WaitForUserAgent instead of falling back
	// to a pool UA — emitting a different UA than the (about-to-be)
	// authoritative session from one IP is a stealth tell.
	// One-shot purge of pre-v20 URL-hash media files. Idempotent — the
	// sentinel inside cfg.Media.RootPath keeps subsequent startups from
	// touching anything once the wipe has run.
	if err := media.WipeLegacyRootIfNeeded(cfg.Media.RootPath); err != nil {
		log.Printf("media: legacy cleanup: %v", err)
	}

	mediaProxy := media.NewProxy(cfg.Media, mediaIndexStore, redisCache, sessionUA)
	evictor := media.NewEvictor(cfg.Media, mediaIndexStore)

	archiver := archive.NewService(postStore, commentStore, subStore)
	subStatusStore := store.NewSubStatusStore(db)
	archiver.SetSubStatusStore(subStatusStore)

	prefetcher := prefetch.New(
		cfg.Prefetch, oauthHolder, &settingsAdapter{store: settingsStore},
		redditCli, publicCli, archiver, mediaProxy, subStatusStore, postStore,
		subIconStore, hrLimiter,
	)

	// 11. Start background tasks
	if err := oauthHolder.Start(ctx); err != nil {
		log.Printf("oauth holder start: %v (continuing without tokens)", err)
	}
	rateLimiter.Start(ctx)
	evictor.Start(ctx)
	prefetcher.Start(ctx)

	// One-off cleanup: drop legacy silent video-only cache entries that a
	// muxed (audio) copy has since superseded.
	go mediaProxy.SweepSupersededPlainRows()

	// 12. Register routes, start HTTP server
	h := handler.New(
		rateLimiter, hrLimiter, redisCache, renderer, redditCli, publicCli, oauthHolder,
		postStore, commentStore, subStore, mediaIndexStore, settingsStore,
		mediaProxy, archiver, prefetcher, subStatusStore, subIconStore, uaPool, cfg,
	)

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      h.Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 13. Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		cancel()
		oauthHolder.Stop()
		rateLimiter.Stop()
		prefetcher.Stop()
		srv.Shutdown(context.Background())
	}()

	log.Printf("RedMemo listening on %s", cfg.Server.Listen)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	log.Println("RedMemo stopped")
}

// oauthAdapter bridges oauth.TokenHolder → reddit.TokenProvider interface.
type oauthAdapter struct {
	holder *oauth.TokenHolder
}

func (a *oauthAdapter) Token() *reddit.TokenInfo {
	mt := a.holder.Token()
	if mt == nil {
		return nil
	}
	headers := make(map[string]string)
	for k, v := range mt.Identity.Headers {
		headers[k] = v
	}
	return &reddit.TokenInfo{
		ID:          mt.StoredToken.ID,
		AccessToken: mt.StoredToken.AccessToken,
		Headers:     headers,
	}
}

func (a *oauthAdapter) OnRequestComplete(tokenID int, resp *fhttp.Response) {
	a.holder.OnRequestComplete(tokenID, resp)
}

func (a *oauthAdapter) NotifyUnauthorized() {
	a.holder.NotifyUnauthorized()
}

// settingsAdapter bridges store.SettingsStore → prefetch.SettingsProvider interface.
type settingsAdapter struct {
	store *store.SettingsStore
}

func (a *settingsAdapter) Get(key string) string {
	v, ok, err := a.store.Get(key)
	if err != nil || !ok {
		return ""
	}
	return v
}

func (a *settingsAdapter) Set(key, value string) error {
	return a.store.SetBatch(map[string]string{key: value}, "prefetch")
}
