# Persistence Layer

← [Wiki index](README.md) · Related: [Architecture](Architecture.md)

PostgreSQL is the system of record. All schema changes are forward-only migrations in `internal/store/migrate.go`.

## Main tables

| Table | Purpose |
|-------|---------|
| `posts` | Permanent append-only post archive (`json_data` JSONB + cached `rendered_html`). Carries `source` (`oauth_fallback` / `prefetch` / `natural_prefetch` / `redlib_proxy`), `media_done` flag for NP L2, `last_updated` for sitemap regeneration. |
| `comments` | Multiple snapshot revisions per post keyed by `(post_url_path, fetched_at)`; the newest snapshot is served by default. |
| `subreddits` | About metadata + JSON dump per sub. |
| `media_content` | Content-addressed asset row keyed by `sha256(file_bytes)`. Holds `file_path` (NULL = evicted), `mime_type`, `audio_state` for v.redd.it, eviction counters. |
| `media_url` | URL alias table. Keyed by `CanonicalKey(rawURL)` (scheme + host + path, query stripped) → `content_hash`. |
| `oauth_tokens` | Persistent OAuth pool with live `rate_remaining` / `rate_reset_at`. |
| `prefetch_config` | NP target list with sort, max pages, fetch toggles, priority. |
| `sub_icons` | L4 icon cache with TTL (default 30 days). |
| `totp_enrollment` / `auth_strikes` | TOTP secret + per-IP lockout state for the `/settings` gate. |

## Three-layer media dedup

```
   ┌──────────────────────────────────────────────────────────┐
   │  raw URL: preview.redd.it/abc.jpg?width=320&s=...        │
   └──────────────────────────┬───────────────────────────────┘
                              │  strip query string
                              ▼
   ┌──────────────────────────────────────────────────────────┐
   │  canonical_key: preview.redd.it/abc.jpg                  │
   │  → media_url row                                         │
   └──────────────────────────┬───────────────────────────────┘
                              │  sha256(file_bytes)
                              ▼
   ┌──────────────────────────────────────────────────────────┐
   │  content_hash: 9f86d081... → media_content row           │
   │  file_path: <root>/9f/9f86d081...                        │
   └──────────────────────────────────────────────────────────┘
```

1. **`canonical_key`** strips the URL query so `?width=320&s=…` and `?width=640&s=…` collapse to one row.
2. **`content_hash`** is `sha256(file_bytes)` — the on-disk identity, so cross-post mirrors share one blob.
3. **bigger-wins** — when a fresh fetch under an existing canonical key produces a larger file (thumbnail first, source later), the URL repoints to the new content and the old file is reclaimed.

## On-disk layout

Files live at `<root>/<hash[:2]>/<hash>` with no extension. The MIME type lives in `media_content.mime_type`. Nginx serves blobs via `X-Accel-Redirect`; Go never touches the IO path after writing.

Eviction is `file_size_MB × hours_since_last_access`, triggered when usage exceeds `media.eviction_threshold` (default 80 % of `max_size_gb`). Evicted files have `file_path` NULLed but the row stays — the next request re-downloads transparently.
