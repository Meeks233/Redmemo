# Configuration Reference

← [Wiki index](README.md) · Related: [Default User Settings](Default-User-Settings.md) · [Quick Deployment](Quick-Deployment.md)

## Config file vs. environment variables

**`config.yaml` is fully optional.** RedMemo boots on built-in defaults plus `REDMEMO_*` environment variables; the YAML file is only useful when you need to pin a value that has no env-var equivalent (the `trusted_proxy_cidrs` list, the `hrlimit.*` knobs, the rarely-used static OAuth token list). Default deployments — including the bundled `docker-compose.yml` — are env-only.

When a YAML file *is* present, the load order is: built-in defaults → `config.yaml` → `REDMEMO_*` env vars. A missing file is not an error.

User-facing settings (theme, layout, `front_page_subs`, `prefetch_subs`, `enable_debug`, `disable_initiative_upstream_access`, …) are persisted in Postgres. Every one of them can be **seeded or pinned** at startup via `REDMEMO_DEFAULT_<KEY>` — those values are written to the DB with the highest-priority `env_override` source on every boot and **override whatever the user / legacy sync had stored**. Remove the env var and the row is automatically demoted, letting user changes stick again. See [Default User Settings](Default-User-Settings.md).

Two compatibility shims make migration from Redlib painless:

- Any `REDLIB_*` (and to a lesser degree `LIBREDDIT_*`) variable is automatically translated into the matching `REDMEMO_*` variable at startup unless the latter is already set. Precedence: `REDMEMO_*` > `REDLIB_*` > `LIBREDDIT_*`.
- `PORT` / `REDLIB_PORT` is translated into `REDMEMO_SERVER_LISTEN=:<port>` (Heroku-style).

**Minimum required env vars** (no YAML fallback for these — they have no built-in defaults):

| Env var | Description |
|---------|-------------|
| `REDMEMO_POSTGRES_DSN` | Full Postgres DSN. |
| `REDMEMO_REDIS_ADDR` | `host:port` for Redis. |
| `REDMEMO_SERVER_SECRET` | Pre-shared TOTP-enrolment secret. Startup refuses to launch without it (unless `REDMEMO_AUTH_BYPASS=on`). |

## Boolean values

Booleans accept `on` / `off`. `true` / `false` are **not** recognised — use `on` / `off` everywhere.

## Server settings

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `server.listen` | `REDMEMO_SERVER_LISTEN` | `:8080` | Listen address. Accepts `:port` or `host:port`. |
| `server.read_timeout` | `REDMEMO_SERVER_READ_TIMEOUT` | `30s` | HTTP server read timeout (Go duration). |
| `server.write_timeout` | `REDMEMO_SERVER_WRITE_TIMEOUT` | `60s` | HTTP server write timeout. |
| `server.trusted_proxy_cidrs` | `REDMEMO_SERVER_TRUSTED_PROXY_CIDRS` | `[]` | Comma-separated CIDR list whose `X-Forwarded-For` is trusted when deriving the client IP for `/settings` lockout. |

## Auth / TOTP gate

| YAML key | Env var | Required | Description |
|----------|---------|----------|-------------|
| `auth.server_secret` | `REDMEMO_SERVER_SECRET` | **yes (unless bypass)** | Pre-shared secret required before TOTP enrolment. |
| `auth.bypass_auth` | `REDMEMO_AUTH_BYPASS` | `off` | When `on`, the TOTP gate is disabled instance-wide — `/settings` and `/debug` are reachable without any cookie. Intended for trusted homelab deployments behind an outer auth layer (Tailscale, VPN, reverse-proxy SSO). **Never set on a public-facing instance.** |

`/debug` is gated twice:
1. The user-facing `enable_debug` preference (off → 303 to `/settings`).
2. The same TOTP gate as `/settings` — visitors without the ephemeral cookie land on the digits-input page. `REDMEMO_AUTH_BYPASS=on` short-circuits this layer.

## PostgreSQL

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `postgres.dsn` | `REDMEMO_POSTGRES_DSN` | **required** | Full DSN, e.g. `postgres://redmemo:pw@postgres:5432/redmemo?sslmode=disable`. |
| `postgres.max_open_conns` | — | `50` | Connection pool ceiling. |
| `postgres.max_idle_conns` | — | `10` | Idle connection target. |

## Redis

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `redis.addr` | `REDMEMO_REDIS_ADDR` | **required** | `host:port`. |
| `redis.password` | `REDMEMO_REDIS_PASSWORD` | (empty) | Optional. |
| `redis.db` | — | `0` | DB number. |
| `redis.max_memory_mb` | — | `256` | `maxmemory` enforced via `allkeys-lru`. |

## Media store

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `media.root_path` | `REDMEMO_MEDIA_ROOT_PATH` | `/data/media` | On-disk root. Layout: `<root>/<hash[:2]>/<hash>`. |
| `media.max_size_gb` | `REDMEMO_MEDIA_MAX_SIZE_GB` | `50` | Soft cap. Eviction starts at `max_size_gb × eviction_threshold`. |
| `media.eviction_check_interval` | `REDMEMO_MEDIA_EVICTION_CHECK_INTERVAL` | `5m` | How often the eviction goroutine wakes up. |
| `media.eviction_threshold` | `REDMEMO_MEDIA_EVICTION_THRESHOLD` | `0.8` | Float in `[0, 1]`. Trigger eviction at this fraction of `max_size_gb`. |

## OAuth tokens

OAuth sessions are managed entirely in the DB by the token holder, which bootstraps an anonymous `mobile_spoof` session on first start and persists refresh state across restarts. **No declaration is required.** Declare a token in YAML only if you want to pin a specific Reddit `client_id` at boot:

```yaml
oauth:
  tokens:
    - client_id: "ohXpoqrZYub1kg"
      backend: "mobile_spoof"
```

| Field | Allowed values | Description |
|-------|----------------|-------------|
| `client_id` | string | Reddit OAuth client id. |
| `client_secret` | string | Optional — most mobile-spoof flows use the public client. |
| `backend` | `mobile_spoof` \| `generic_web` | Selects the OAuth flow + header set. `mobile_spoof` mimics the official Android app. |

## Android user-agent

Only consumed by the `mobile_spoof` backend. Priority: **`USER_AGENT` > `APP_VERSION` > `APP_DATE` > built-in default**.

| Env var | Description |
|---------|-------------|
| `REDMEMO_ANDROID_USER_AGENT` | Full UA string used verbatim — highest priority, disables randomisation. e.g. `Reddit/Version 2026.07.0/Build 2607141/Android 14`. |
| `REDMEMO_ANDROID_APP_VERSION` | Comma-separated `Version YYYY.WW.X/Build NNNNNNN` entries; one picked at random per token. |
| `REDMEMO_ANDROID_APP_DATE` | Date `YYYY-MM-DD`; auto-translated to a synthesised version+build. Ignored if `APP_VERSION` is set. |
| `REDMEMO_ANDROID_OS_VERSION` | Android major version. Fixed (`14`) or range (`12-15`). |

See [`docs/android-user-agent.md`](../docs/android-user-agent.md) for the full table of known builds.

## Rate-limit budget

The **OAuth-side** budget tracker (parsed from Reddit's `X-Ratelimit-Remaining/Reset` headers), separate from the [HR limiter](HR-Rate-Limit.md).

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `ratelimit.window_size` | `REDMEMO_RATELIMIT_WINDOW_SIZE` | `500` | Conservative cap per window. |
| `ratelimit.window_duration` | `REDMEMO_RATELIMIT_WINDOW_DURATION` | `10m` | Reddit's rolling window. |
| `ratelimit.safety_buffer` | `REDMEMO_RATELIMIT_SAFETY_BUFFER` | `50` | Reserved for NP / prefetch so user traffic always wins. |

## Natural Prefetch

Master switch and dispatcher tick:

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `prefetch.enabled` | `REDMEMO_PREFETCH_ENABLED` | `on` | Top-level kill switch for the prefetch subsystem. |
| `prefetch.check_interval` | `REDMEMO_PREFETCH_CHECK_INTERVAL` | `30s` | Dispatcher tick. |

The **crawl list** and the per-user **on/off toggle** live in the DB (settings keys `prefetch_subs` and `enable_natural_prefetch`) — set them from `/settings` or seed them at boot via:

| Env var | Format | Example |
|---------|--------|---------|
| `REDMEMO_DEFAULT_ENABLE_NATURAL_PREFETCH` | `on` / `off` | `on` |
| `REDMEMO_DEFAULT_PREFETCH_SUBS` | unified search grammar | `sub:golang+rust+linux` |
| `REDMEMO_DEFAULT_PREFETCH_THRESHOLD` | `1..99` | `50` |

## HR rate-limit layer

| YAML key | Env var | Default |
|----------|---------|---------|
| `hrlimit.enabled` | `REDMEMO_HRLIMIT_ENABLED` | `on` |
| `hrlimit.l1_window` | `REDMEMO_HRLIMIT_L1_WINDOW` | `5s` |
| `hrlimit.l1_threshold` | `REDMEMO_HRLIMIT_L1_THRESHOLD` | `5` |
| `hrlimit.l2_window` | `REDMEMO_HRLIMIT_L2_WINDOW` | `30s` |
| `hrlimit.l2_threshold` | `REDMEMO_HRLIMIT_L2_THRESHOLD` | `15` |
| `hrlimit.l3_window` | `REDMEMO_HRLIMIT_L3_WINDOW` | `5m` |
| `hrlimit.l3_threshold` | `REDMEMO_HRLIMIT_L3_THRESHOLD` | `50` |

## Render / branding

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `render.brand_name` | `REDMEMO_RENDER_BRAND_NAME` | `RedMemo` | Title / navbar brand. |
| `render.show_archive_badge` | `REDMEMO_RENDER_SHOW_ARCHIVE_BADGE` | `on` | When off, suppress the small "from archive" badge on pages served without an upstream call. |

## SEO

Off by default. When `allow_indexing=off`: `/robots.txt` is `Disallow: /`, `/sitemap.xml` 404s, every page emits `<meta name=robots content="noindex,nofollow">`.

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `seo.allow_indexing` | `REDMEMO_SEO_ALLOW_INDEXING` | `off` | Master switch for indexing the archive surfaces. |
| `seo.canonical_host` | `REDMEMO_SEO_CANONICAL_HOST` | (empty) | Public origin used for absolute URLs in `sitemap.xml` and `<link rel="canonical">`. |

## Legacy redlib sync

A one-time helper for users migrating from an existing Redlib instance.

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `legacy.sync_enabled` | `REDMEMO_LEGACY_SYNC` | `on` | Disable once everyone has migrated. |
| `legacy.instance` | `REDMEMO_LEGACY_INSTANCE` | empty → `http://redlib:8080` | Override the docker DNS name if your legacy instance lives elsewhere. |
