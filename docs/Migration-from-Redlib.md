# Migration from Redlib — Key Differences

← [Wiki index](README.md)

If you're coming from a running Redlib instance, the user-facing UI is intentionally the same — themes, layouts, cookies, route shape, search syntax all carry over (and `REDLIB_*` env vars are auto-translated, see [Legacy redlib sync](Configuration.md#legacy-redlib-sync)). The underlying philosophy, however, is meaningfully different.

## 1. Passive archive site (upstream-restricted by design)

Redlib is a **live proxy**: every page view triggers an upstream Reddit call, and if Reddit blocks the request the user gets an error. RedMemo flips that model — it is first and foremost an **archive station** that happens to fetch fresh content when it can.

- Outbound traffic is hard-capped by the HR three-tier limiter (5 s / 30 s / 5 min tumbling windows). When any tier trips, foreground requests **degrade to the local archive** with a small amber "You are browsing archived content" banner instead of erroring out.
- Explicit `/archive/...` routes never attempt upstream and never consult HR — they serve from Postgres unconditionally.
- The same applies when the OAuth quota is exhausted, when Redis is unreachable (HR fails closed), or when the upstream call itself fails: archive fallback first, `/fuckreddit` only if the user explicitly clicks the banner.
- This means RedMemo behaves correctly even when self-hosted on a residential IP that Reddit eventually rate-limits — the archive keeps serving while NP slowly refills it.

## 2. New authentication model (server secret + TOTP)

Redlib has no admin auth — `/settings` is a public cookie-write surface for anyone with a browser. RedMemo adds two layers in front of `/settings`:

- **`REDMEMO_SERVER_SECRET`** — a pre-shared instance secret that gates **enrolment**. Without it the server refuses to start, and `/settings` can't be re-enrolled by a casual visitor even if they reach the page.
- **TOTP** — a per-instance authenticator-app secret stored in Postgres (`totp_enrollment` table). Verification is rate-limited per IP with a 3-strike lockout (`auth_strikes` table), and the trusted-proxy CIDR list (`server.trusted_proxy_cidrs`) decides whether `X-Forwarded-For` is honoured when deriving the per-request IP.
- Operators get an administrative CLI reset command for the case where the authenticator device is lost.

In short: Redlib trusts the network; RedMemo trusts an authenticator app + a secret you set at deploy time.

## 3. Persistent storage (Postgres post archive + canonical media cache)

Redlib's storage model is "Redis HTML cache, that's it" — restart the process and the cache is empty. RedMemo's storage model is "Redis is a hot cache, **Postgres is the system of record**, the media root is a content-addressed CDN".

See [Persistence Layer](Persistence.md) for the full schema and dedup design.

## 4. Refactor in Go (templ SSR, no JS framework)

Redlib is written in **Rust** on top of Hyper, with Askama for templating. RedMemo is a full ground-up **Go** rewrite:

- **Go** for the back-end (`internal/`, `cmd/redmemo`) — same `bogdanfinn/tls-client` for the outbound transport, so the TLS ClientHello and HTTP/2 (Akamai h2) fingerprints still match the official Reddit Android client.
- **templ** for SSR — the entire UI is server-side rendered into static HTML by Go templates that mirror Redlib's Askama tree. No JS framework, no client-side hydration, no SPA shell.
- The four-layer Natural Prefetch scheduler, the HR rate-limit gate, the `media_content`/`media_url` content-addressed store and the TOTP gate are all native Go code with no Rust counterpart in Redlib.
- Build artefacts are a single static Go binary (plus the Postgres + Redis + nginx side-car services in compose).

## 5. e621-compatible unified search

Redlib forwards the search box straight to Reddit's Lucene parser. RedMemo replaces it with a single **e621-style** parser (`internal/searchquery`) that drives **both** the live `/search` and the offline `/archive` from the same typed query — constraints Reddit's API can't express degrade to a local post-filter over the JSON results, so the two surfaces stay consistent.

Example: `sub:rust rating:nsfw score:>1000` becomes `subreddit:rust nsfw:yes` upstream, with the score threshold applied as a local post-filter.

See [Search & URL Reference](Search-Reference.md) for the full token table.
