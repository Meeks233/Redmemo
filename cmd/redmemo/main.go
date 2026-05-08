package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/redmemo/redmemo/internal/cache"
	"github.com/redmemo/redmemo/internal/config"
	"github.com/redmemo/redmemo/internal/handler"
	"github.com/redmemo/redmemo/internal/media"
	"github.com/redmemo/redmemo/internal/oauth"
	"github.com/redmemo/redmemo/internal/prefetch"
	"github.com/redmemo/redmemo/internal/proxy"
	"github.com/redmemo/redmemo/internal/ratelimit"
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
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

	// 5. Init OAuth
	oauthClient := oauth.NewClient()
	oauthPool := oauth.NewPool(cfg.OAuth, oauthClient, tokenStore, redisCache)

	// 6. Init Reddit client
	redditAdapter := &oauthAdapter{pool: oauthPool}
	redditCli := reddit.NewClient(redditAdapter)

	// 7. Init modules
	rateLimiter := ratelimit.New(cfg.RateLimit, oauthPool)

	var reverseProxy *proxy.Proxy
	if cfg.Redlib.Enabled {
		reverseProxy, err = proxy.New(cfg.Redlib)
		if err != nil {
			log.Fatalf("proxy: %v", err)
		}
	}

	renderer, err := render.New(cfg.Render)
	if err != nil {
		log.Fatalf("render: %v", err)
	}

	mediaProxy := media.NewProxy(cfg.Media, mediaIndexStore, redisCache)
	evictor := media.NewEvictor(cfg.Media, mediaIndexStore)

	prefetcher := prefetch.New(
		cfg.Prefetch, rateLimiter, redditCli,
		postStore, commentStore, subStore, mediaProxy,
	)

	// 8. Start background tasks
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := oauthPool.Start(ctx); err != nil {
		log.Printf("oauth pool start: %v (continuing without tokens)", err)
	}
	rateLimiter.Start(ctx)
	evictor.Start(ctx)
	if cfg.Prefetch.Enabled {
		prefetcher.Start(ctx)
	}

	// 9. Register routes, start HTTP server
	h := handler.New(
		reverseProxy, rateLimiter, redisCache, renderer, redditCli,
		postStore, commentStore, subStore, mediaIndexStore, mediaProxy, cfg,
	)

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      h.Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 10. Graceful shutdown
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
