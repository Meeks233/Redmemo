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

Full enrolment flow, trusted devices, lockout and rotation: [Auth / TOTP Gate](Auth-TOTP.md).

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

OAuth sessions are managed entirely in the DB by the token holder, which bootstraps a session on first start and persists refresh state across restarts. **No declaration is required.**

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

There is no separate on/off toggle: **the crawl list is the switch.** A non-empty `prefetch_subs` (settings key, in the DB) enables the layer; a blank one — including input that was pure punctuation/whitespace and got filtered down to nothing — leaves it idle. Set it from `/settings` or seed it at boot via:

| Env var | Format | Example |
|---------|--------|---------|
| `REDMEMO_DEFAULT_PREFETCH_SUBS` | unified search grammar | `sub:golang+rust+linux` |
| `REDMEMO_DEFAULT_PREFETCH_THRESHOLD` | `1..99` | `50` |
| `REDMEMO_DEFAULT_PREFETCH_DEFAULT_DEPTH` | `none` / `l2` / `l3` / `l2+l3` | `l2+l3` |
| `REDMEMO_DEFAULT_PREFETCH_L3_MIN_COMMENTS` | `0..100000` | `0` (compose presets ship `50`) |
| `REDMEMO_DEFAULT_PREFETCH_SUB_MODES` | per-sub overrides, e.g. `golang=depth:l2+l3&sort:top+rust=depth:none` | _(empty)_ |

`REDMEMO_DEFAULT_PREFETCH_L3_MIN_COMMENTS` is the L3 noise floor: any archived post with fewer than this many comments is frozen out of L3 — the count comes from the post JSON locally, no upstream probe. `0` disables the filter. The value must be a non-negative integer in `[0, 100000]`; an invalid value causes the container to **refuse to start** (loud failure beats silent fallback). Combined with the L3 cycle-freeze (a post archived during L3 cycle N is automatically skipped during L3 cycle N+1 regardless of count, on L3's own independent cadence), this lets operators say "don't waste budget on 1-line threads" while still letting hot threads through.

The settings page has no enable checkbox: filling the prefetch box turns the layer on, clearing it turns it off. Once at least one sub is listed, every sub's L1 listing runs, and **Default depth** controls whether L2 (media) and L3 (comments) run on top of L1 — `none` keeps the deployment to listings only; `l2+l3` runs both layers independently — an L2 media cache wave and a separate self-standing L3 comment cycle in parallel (default for local LAN). Per-sub overrides in the prefetch box can add a `depth:` clause to deviate for a single sub, e.g. global default `none` plus `golang=depth:l2+l3&sort:top` opts only r/golang into media+comments while every other sub stays at L1-only.

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

**On by default** — decentralized discovery is the whole point: search engines must be able to surface which instance mirrors which Natural-Prefetch subs so people can find a live mirror without a central directory. The archive surfaces, `/sitemap.xml` and `/np.json` all advertise the instance's chosen NP subs. Set `allow_indexing=off` only for a **private** instance: that flips `/robots.txt` to `Disallow: /`, 404s `/sitemap.xml` + `/np.json`, and emits `<meta name=robots content="noindex,nofollow">` on every page.

The advertised sub set is the **union** of (a) subs already archived and (b) subs configured for Natural Prefetch (`prefetch_subs`) — so a freshly stood-up instance is discoverable by its chosen subs immediately, before the first crawl cycle lands.

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `seo.allow_indexing` | `REDMEMO_SEO_ALLOW_INDEXING` | `on` | Master switch for indexing the archive surfaces + `/np.json`. |
| `seo.canonical_host` | `REDMEMO_SEO_CANONICAL_HOST` | (empty) | Public origin used for absolute URLs in `sitemap.xml`, `/np.json` and `<link rel="canonical">`. |

### `/np.json` — decentralized discovery feed

A stable, machine-readable advert of this instance's Natural-Prefetch sub list, for aggregators/directories that map *sub → mirror* without scraping HTML. Served at the site root, allowed in `robots.txt`, cross-origin readable (`Access-Control-Allow-Origin: *`), 404s when indexing is off.

```json
{
  "brand": "RedMemo",
  "host": "https://memo.example.com",
  "archive_url": "https://memo.example.com/archive",
  "count": 2,
  "subs": ["transgender", "ftm"],
  "sub_links": [
    {"sub": "transgender", "url": "https://memo.example.com/archive/r/transgender", "archived": true},
    {"sub": "ftm", "url": "https://memo.example.com/archive/r/ftm", "archived": false}
  ]
}
```

## Link previews (unfurl)

Telegram/Discord-style preview **cards** for bare external links in post and
comment bodies. Full design in [Link Preview](Link-Preview.md).

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `unfurl.enabled` | `REDMEMO_UNFURL_ENABLED` | `on` | Master switch. When `off`, bodies render plain links and `/api/unfurl` returns `failed`. |
| `unfurl.jina_fallback` | `REDMEMO_UNFURL_JINA_FALLBACK` | `on` | Opt into the `r.jina.ai` reader as a last-resort fetcher for anti-bot pages a direct OpenGraph crawl can't reach. **Sends the link URL to a third party** — a separate opt-in from the privacy-preserving direct fetch. |
| `unfurl.timeout` | `REDMEMO_UNFURL_TIMEOUT` | `8s` | Per-link server-side metadata fetch ceiling (Go duration). |

Card metadata is fetched lazily (client-driven, one link at a time as it scrolls
into view) and cached in Postgres, so a link is fetched once across all viewers.
Preview images/videos load directly in the viewer's browser — RedMemo does not
proxy them.

## Legacy redlib sync

A one-time helper for users migrating from an existing Redlib instance.

| YAML key | Env var | Default | Description |
|----------|---------|---------|-------------|
| `legacy.sync_enabled` | `REDMEMO_LEGACY_SYNC` | `on` | Disable once everyone has migrated. |
| `legacy.instance` | `REDMEMO_LEGACY_INSTANCE` | empty → `http://redlib:8080` | Override the docker DNS name if your legacy instance lives elsewhere. |
