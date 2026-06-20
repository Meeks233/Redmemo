package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
	"github.com/redmemo/redmemo/internal/reddit"
	"github.com/redmemo/redmemo/internal/render"
	"github.com/redmemo/redmemo/internal/store"
	"github.com/redmemo/redmemo/internal/unfurl"
	"github.com/redmemo/redmemo/internal/versionintel"
)

// Injected at release time via -ldflags "-X main.version=..." by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	log.Printf("redmemo: version=%s commit=%s built=%s", version, commit, date)

	// Propagate the build-injected version into the render layer, which is the
	// single source of truth for the version shown in page footers. Set before
	// any page is served so every request reflects the release tag.
	render.Version = version

	configPath := "config.yaml"
	resetTOTP := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--reset-totp", "-reset-totp":
			resetTOTP = true
		case "-h", "--help":
			log.Printf("usage: redmemo [config.yaml] [--reset-totp]")
			return
		default:
			if !strings.HasPrefix(arg, "-") {
				configPath = arg
			}
		}
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
	mediaUnavailableStore := store.NewMediaUnavailableStore(db)
	tokenStore := store.NewTokenStore(db)
	deviceProfileStore := store.NewDeviceProfileStore(db)
	subStore := store.NewSubredditStore(db)
	subIconStore := store.NewSubIconStore(db)
	settingsStore := store.NewSettingsStore(db)
	totpStore := store.NewTOTPStore(db)
	trustedDeviceStore := store.NewTrustedDeviceStore(db)

	// --reset-totp: one-shot administrative wipe of the enrolled TOTP secret.
	// The next browser visit re-prompts for the server secret and re-renders
	// the (new) QR. Exits immediately so a misconfigured cron / sysadmin
	// retry never accidentally boots the server in a half-reset state.
	if resetTOTP {
		if err := totpStore.Reset(); err != nil {
			log.Fatalf("reset-totp: %v", err)
		}
		// A "Trust this device" cookie outlives the secret it was minted
		// under. An operator resets the second factor precisely when they suspect
		// compromise, so the reset MUST also de-authorise every trusted device —
		// otherwise a stolen trusted cookie keeps full /settings access across it.
		if n, err := trustedDeviceStore.DeleteAll(); err != nil {
			log.Fatalf("reset-totp: revoke trusted devices: %v", err)
		} else if n > 0 {
			log.Printf("reset-totp: revoked %d trusted device(s).", n)
		}
		log.Println("reset-totp: cleared. Next /settings visit will re-enroll.")
		return
	}

	// Server-secret gate. Without it the settings UI is unreachable, so we
	// refuse to listen at all rather than silently expose an open endpoint.
	// REDMEMO_AUTH_BYPASS=on opts out — the gate is short-circuited so no
	// secret is needed; this is meant for trusted homelab deployments behind
	// an outer auth layer (Tailscale, VPN, reverse-proxy SSO).
	if !cfg.Auth.BypassAuth && strings.TrimSpace(cfg.Auth.ServerSecret) == "" {
		log.Fatalf("auth: REDMEMO_SERVER_SECRET (or auth.server_secret) is required; refusing to start. Set REDMEMO_AUTH_BYPASS=on to skip the TOTP gate entirely (homelab only).")
	}
	if cfg.Auth.BypassAuth {
		log.Printf("auth: REDMEMO_AUTH_BYPASS=on — /settings and /debug are OPEN. Ensure an outer auth layer (Tailscale, VPN, reverse-proxy SSO) is in place.")
	}
	authMgr := handler.NewAuthManager(cfg.Auth.ServerSecret, totpStore, trustedDeviceStore)

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
	// Step B: env var overrides (always win — scans all REDMEMO_DEFAULT_* vars).
	// Run them through the SAME normaliser the /settings form uses so
	// REDMEMO_DEFAULT_PREFETCH_SUBS=golang becomes stored as "sub:golang" (matching
	// what a UI save would produce) and obvious typos (VIDEO_QUALITY=garbage,
	// SCROLL_INTERVAL=abc, ...) surface in the log instead of silently poisoning
	// the DB. Dead-sub filtering is intentionally skipped here — substatus is
	// not yet initialised, and form save handles it on the next user touch.
	envSettings := config.ScanExplicitSettings()
	envSettings, rejected := handler.NormalizeSettings(envSettings)
	for _, r := range rejected {
		// Certain keys are operator-critical: silently dropping a bad value
		// would leave the deployment running with the build-in default and
		// hide the misconfiguration. Refuse to start in that case so the
		// docker container restarts loudly and the operator notices.
		if handler.IsFatalSettingKey(r.Key) {
			log.Fatalf("settings: REDMEMO_DEFAULT_%s=%q invalid — %s (refusing to start)",
				strings.ToUpper(r.Key), r.Value, r.Reason)
		}
		log.Printf("settings: REDMEMO_DEFAULT_%s=%q ignored — %s",
			strings.ToUpper(r.Key), r.Value, r.Reason)
	}
	// The managed keys (homepage / NP / archive-control) are NOT applied through
	// the plain env_override path — they are reconciled by latest-writer-wins in
	// Step C below. Pull them out of envSettings so Step B neither overwrites the
	// live row nor lets DemoteOrphans touch it; their env value is carried into
	// the reconcile via managedEnv instead.
	managedEnv := make(map[string]string)
	for _, k := range handler.ManagedSettingKeys {
		if v, ok := envSettings[k]; ok {
			managedEnv[k] = v
			delete(envSettings, k)
		}
	}
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
	// Step C: reconcile the managed keys by latest-writer-wins between the env
	// default and the user's /settings save. The operator's REDMEMO_DEFAULT_*
	// seeds the feature, but whichever side was touched most recently wins — so a
	// manual change sticks across rebuilds, and a later compose edit still takes
	// effect. (See handler.ReconcileManagedSettings.)
	if written, err := handler.ReconcileManagedSettings(settingsStore, managedEnv, time.Now()); err != nil {
		log.Printf("settings: reconcile managed settings failed: %v", err)
	} else if written > 0 {
		log.Printf("settings: reconciled %d managed setting(s) by latest-writer-wins", written)
	}

	// 6. Init shared context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 7. Init OAuth
	deviceProfile, err := oauth.ResolveDeviceProfile(deviceProfileStore)
	if err != nil {
		log.Fatalf("oauth: resolve device profile: %v", err)
	}
	log.Printf("oauth: pinned device profile (android=%d, app=%s, device=%s)",
		deviceProfile.AndroidVersion, deviceProfile.AppVersion, deviceProfile.DeviceID)
	oauthClient := oauth.NewClient(deviceProfile)
	versionTracker := versionintel.NewTracker(&http.Client{Timeout: 15 * time.Second})
	oauthHolder := oauth.NewTokenHolder(cfg.OAuth, oauthClient, tokenStore, deviceProfileStore, versionTracker, redisCache)

	// sessionUA returns the active OAuth session's bound User-Agent, blocking
	// through the cold-start window (see TokenHolder.WaitForUserAgent). Every
	// Reddit-facing client shares this one closure so a single identity drives
	// every outbound request. There is deliberately no browser-UA fallback:
	// emitting a second UA from the session IP would be a stealth tell.
	sessionUA := func() string {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return oauthHolder.WaitForUserAgent(ctx)
	}

	// 8. Init Reddit clients
	redditAdapter := &oauthAdapter{holder: oauthHolder}
	redditCli := reddit.NewClient(redditAdapter)
	publicCli := reddit.NewPublicClient(sessionUA)

	// 9. Init modules
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

	mediaProxy := media.NewProxy(cfg.Media, mediaIndexStore, mediaUnavailableStore, redisCache, sessionUA)
	evictor := media.NewEvictor(cfg.Media, mediaIndexStore)
	mediaProxy.SetCleanupLog(evictor.Events)

	// Let the long-video gate consult the on-disk cache: a long clip whose
	// bytes are already resident skips the click-to-load placeholder and renders
	// as a live <video> that streams from local cache — no upstream fetch to
	// defer, nothing the gate would protect.
	renderer.SetMediaCachedFn(mediaProxy.IsCached)

	// Link-preview unfurling: external links in post/comment bodies are marked
	// for lazy, client-driven preview cards. The body just carries data-unfurl
	// hints; the browser asks /api/unfurl (served by this Service) for one
	// preview at a time as cards scroll into view, and loads preview images/video
	// directly. The Service holds the cache, single-flight, SSRF guard, and
	// outbound concurrency cap. nil when the operator disables the feature.
	var unfurlSvc *unfurl.Service
	if cfg.Unfurl.Enabled {
		unfurlSvc = unfurl.New(store.NewLinkPreviewStore(db), unfurl.Config{
			Enabled:      cfg.Unfurl.Enabled,
			JinaFallback: cfg.Unfurl.JinaFallback,
			Timeout:      cfg.Unfurl.Timeout,
		})
	}

	archiver := archive.NewService(postStore, commentStore, subStore)
	subStatusStore := store.NewSubStatusStore(db)
	archiver.SetSubStatusStore(subStatusStore)

	// Seed the Archive Control filter from the settings DB (which already
	// reflects any REDMEMO_DEFAULT_ARCHIVE_CONTROL applied above). The /settings
	// save path hot-swaps it again whenever the user edits the value.
	if v, ok, _ := settingsStore.Get("archive_control"); ok {
		archiver.SetControlFromString(v)
	}

	prefetchRunStore := store.NewPrefetchRunStore(db)
	prefetcher := prefetch.New(
		cfg.Prefetch, oauthHolder, &settingsAdapter{store: settingsStore},
		redditCli, publicCli, archiver, mediaProxy, subStatusStore, postStore,
		prefetchRunStore, subIconStore, hrLimiter,
	)

	// 10. Start background tasks
	if err := oauthHolder.Start(ctx); err != nil {
		log.Printf("oauth holder start: %v (continuing without tokens)", err)
	}
	evictor.Start(ctx)
	prefetcher.Start(ctx)
	authMgr.StartTrustedSweeper(ctx)

	// One-off cleanup: drop legacy silent video-only cache entries that a
	// muxed (audio) copy has since superseded.
	go mediaProxy.SweepSupersededPlainRows()

	// 11. Register routes, start HTTP server
	h := handler.New(
		hrLimiter, redisCache, renderer, redditCli, publicCli, oauthHolder,
		postStore, commentStore, subStore, mediaIndexStore, settingsStore,
		mediaProxy, archiver, prefetcher, evictor, subStatusStore, subIconStore, cfg,
	).WithAuth(authMgr).WithUnfurl(unfurlSvc)

	srv := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      h.Routes(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// 12. Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("received %v, shutting down...", sig)
		cancel()
		oauthHolder.Stop()
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
