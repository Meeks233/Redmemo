package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/redmemo/redmemo/internal/archive"
	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/handler"
	"github.com/redmemo/redmemo/internal/legacy"
	"github.com/redmemo/redmemo/internal/media"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/prefetch"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/useragent"
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
	subStore := store.NewSubredditStore(db)
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
	oauthClient := oauth.NewClient()
	oauthPool := oauth.NewPool(cfg.OAuth, oauthClient, tokenStore, redisCache)

	// 9. Init Reddit clients
	redditAdapter := &oauthAdapter{pool: oauthPool}
	redditCli := reddit.NewClient(redditAdapter)
	publicCli := reddit.NewPublicClient(uaPool)

	// 10. Init modules
	rateLimiter := ratelimit.New(cfg.RateLimit, oauthPool)

	renderer, err := render.New(cfg.Render)
	if err != nil {
		log.Fatalf("render: %v", err)
	}

	mediaProxy := media.NewProxy(cfg.Media, mediaIndexStore, redisCache, uaPool)
	evictor := media.NewEvictor(cfg.Media, mediaIndexStore)

	archiver := archive.NewService(postStore, commentStore, subStore)
	subStatusStore := store.NewSubStatusStore(db)

	prefetcher := prefetch.New(
		cfg.Prefetch, oauthPool, &settingsAdapter{store: settingsStore},
		redditCli, publicCli, archiver, mediaProxy, subStatusStore,
	)

	// 11. Start background tasks
	if err := oauthPool.Start(ctx); err != nil {
		log.Printf("oauth pool start: %v (continuing without tokens)", err)
	}
	rateLimiter.Start(ctx)
	evictor.Start(ctx)
	prefetcher.Start(ctx)

	// 12. Register routes, start HTTP server
	h := handler.New(
		rateLimiter, redisCache, renderer, redditCli, publicCli, oauthPool,
		postStore, commentStore, subStore, mediaIndexStore, settingsStore,
		mediaProxy, archiver, prefetcher, subStatusStore, uaPool, cfg,
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
		oauthPool.Stop()
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

// oauthAdapter bridges oauth.Pool → reddit.TokenProvider interface.
type oauthAdapter struct {
	pool *oauth.Pool
}

func (a *oauthAdapter) GetBestToken() *reddit.TokenInfo {
	mt := a.pool.GetBestToken()
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
		UserAgent:   mt.Identity.UserAgent,
		Headers:     headers,
	}
}

func (a *oauthAdapter) OnRequestComplete(tokenID int, resp *http.Response) {
	a.pool.OnRequestComplete(tokenID, resp)
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
