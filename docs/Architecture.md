# Architecture

← [Wiki index](README.md) · Related: [Persistence](Persistence.md) · [Natural Prefetch](Natural-Prefetch.md) · [HR Rate-Limit](HR-Rate-Limit.md)

## Failover chain

Every front-end request walks a four-step ladder. The first step that returns wins.

```
                       ┌──────────────────────────────┐
                       │       Incoming request       │
                       └──────────────┬───────────────┘
                                      │
                       ┌──────────────▼───────────────┐
                       │  1. Redis HTML cache         │
                       │     listings 5m / posts 1h   │
                       └──────┬───────────────┬───────┘
                          hit │          miss │
                              │               ▼
                              │   ┌────────────────────────────┐
                              │   │  2. Reddit API call        │
                              │   │     OAuth quota + HR gate  │
                              │   └────┬──────────────────┬────┘
                              │     ok │             fail │
                              │        │                  ▼
                              │        │   ┌────────────────────────┐
                              │        │   │  3. PostgreSQL archive │
                              │        │   │  + amber "archived"    │
                              │        │   │    banner              │
                              │        │   └────┬─────────────┬─────┘
                              │        │    hit │        miss │
                              │        │        │             ▼
                              │        │        │   ┌──────────────────┐
                              │        │        │   │ 4. /fuckreddit   │
                              │        │        │   │    (only if user │
                              │        │        │   │     clicks)      │
                              │        │        │   └──────────────────┘
                              ▼        ▼        ▼
                       ┌──────────────────────────────┐
                       │       Rendered HTML          │
                       └──────────────────────────────┘
```

1. **Redis HTML cache** — TTL 5 min for listings, 1 h for post pages. Zero upstream cost.
2. **Reddit API** — gated by both the OAuth quota window and the HR cooldown gate.
3. **PostgreSQL archive** — if upstream cannot be made (no budget, HR cooldown, network failure), cached `posts` / `comments` rows are rendered with an amber "You are browsing archived content" banner. Clicking the banner takes the user to `/fuckreddit?reason=...`.
4. **`/fuckreddit` page** — terminal degrade surface, only reached if the user explicitly clicks the banner.

## Component map

```
   ┌──────────────┐    ┌──────────────┐    ┌──────────────┐    ┌──────────────┐
   │   redmemo    │◀──▶│    redis     │    │   postgres   │    │    nginx     │
   │  (Go + templ)│    │  HTML cache, │    │  archive +   │    │  X-Accel-    │
   │              │    │  HR counters │    │  config + TOTP    │  Redirect    │
   └──────┬───────┘    └──────────────┘    └──────────────┘    └──────┬───────┘
          │                                                            ▲
          │       outbound (stealth transport)                            │ media blobs
          ▼                                                            │ on disk
   ┌──────────────┐                                          ┌──────────────────┐
   │  Reddit API  │                                          │ <root>/<hh>/<sha>│
   │  + CDN       │                                          │  content-addr    │
   └──────────────┘                                          └──────────────────┘
```

- **redmemo** never sits on the media IO path — it writes the file once and lets nginx serve it via `X-Accel-Redirect`.
- **Redis** holds only volatile state (HTML cache, HR counters, active OAuth token id, settings-cookie cache). AOF is enabled (`--appendonly yes --appendfsync everysec`) so HR cooldown survives a restart with at most ~1 s of loss.
- **Postgres** is the system of record. All schema changes are forward-only migrations in `internal/store/migrate.go`.

## Security

RedMemo applies defence-in-depth across authentication, injection prevention, and media integrity. Implementation details are maintained internally.
