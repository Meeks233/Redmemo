# Link Preview (Unfurl)

RedMemo turns a **bare external link** in a post selftext or comment body into a
Discord/Telegram-style preview card — site name, title, description, and a
thumbnail/banner/video — the way chat apps "unfurl" a pasted URL. Reddit-owned
links stay in-site links; direct image/video URLs stay inline media; only bare
external auto-links (visible text == href) are unfurled, so a user-written
labelled link is never touched. A link that can't be unfurled stays the plain
link it already was.

## Lazy, client-driven (why)

Previews are **not** fetched at page-render time. A "Small Projects" megathread of
30+ GitHub links would otherwise make the server fire a burst of cross-site
fetches from one IP — GitHub rate-limited it, so links past the first handful
degraded to plain text. Instead:

1. The server renders each bare external link as a lazy placeholder — the plain
   link plus a `data-unfurl` hint and the `link-preview-lazy` class
   (`render.markLazyLinks`). Nothing is fetched yet; no-JS / fetch-failure just
   shows the link.
2. `linkPreview.js` observes those anchors with an `IntersectionObserver` and,
   only once one scrolls near the viewport, asks `GET /api/unfurl?url=…` for that
   one link's metadata — **one at a time, max 2 concurrent**. Load spreads as the
   user scrolls, so no host gets bursted.
3. On `status:ok` the link is **replaced** by a card (raw URL never shown twice);
   on `status:failed` the plain link is left and not retried.

**Media stays on the client.** The card's preview image and video `src` point at
the *real* third-party URLs (`opengraph.githubassets.com`, `pbs.twimg.com`,
fixupx's video…), loaded/streamed directly by the viewer's browser — RedMemo does
**not** proxy preview media. Image hosts that gate by IP (GitHub) can 429 a burst;
the loader retries with jittered backoff before degrading a card to text-only.
Only the small, shared, cached **metadata** fetch is server-side (CORS makes a
cross-origin HTML read impossible in the browser).

## Display variants

`linkPreview.js` picks a layout from the metadata, mirroring the OpenGraph card
types:

- **small** — compact card, square logo/favicon thumbnail on the left (a site
  with only an icon, e.g. Stack Overflow's apple-touch-icon).
- **large** — a banner on top, text below (GitHub's 1280×640 repo card with
  stars/forks/avatar, news hero shots). Capped at ~540px on desktop, full-width
  on mobile, so a wide image doesn't dominate the thread.
- **video** — a full-width playable `<video>` on top (X/Twitter via fixupx's
  `og:video`), streamed by the browser. Telegram-style inline play.
- **text** — no usable image; title + description only.

The small-vs-large choice is made **authoritatively on the client** from the real
loaded image's aspect ratio (`linkPreview.js`): clearly landscape (≥1.6:1) →
large banner, square-ish → small logo thumbnail. The server sends an
`image_wide` *hint* (`unfurl.isWideImage`) to avoid a layout jump, derived from
og:image dimensions + known GitHub host patterns — deliberately **not** from
`twitter:card=summary_large_image` alone, since GitHub sets that on org/user
profile pages whose og:image is a square avatar (which would otherwise stretch a
logo into a banner — the AuthPlane case).

## Metadata failover chain (`unfurl.fetcher.Fetch`)

Server-side, behind `GET /api/unfurl`, cached so a link is fetched once across
all viewers:

1. **Host-fixup mirror** — bot-hostile social hosts that serve no OpenGraph are
   rewritten to their crawler-facing embed mirror, then unfurled like any OG
   page. `x.com` / `twitter.com` / `nitter.net` → `fixupx.com` (the fxtwitter
   family), which also exposes `og:video` for tweet videos. The card always
   points at the *original* link.
2. **Direct OpenGraph fetch** — the page is fetched with the project's uTLS-
   spoofed transport and a crawler UA (`facebookexternalhit`/`Twitterbot`/
   `TelegramBot`); `og:*` / `twitter:*` / `<title>` tags are parsed
   (`golang.org/x/net/html`). Privacy-preserving; always tried first.
3. **Jina Reader fallback** (`r.jina.ai`, opt-in via `jina_fallback`) — for pages
   a direct crawl can't reach (Cloudflare/anti-bot interstitials such as Stack
   Overflow's "Just a moment…"). Sends the link URL to a third party, hence the
   separate toggle.

The fetch is guarded by an **SSRF boundary** (`unfurl.isPublicHTTPURL`): a URL
that resolves to a private/loopback/link-local/cloud-metadata address is never
fetched (nor handed to the Jina reader) — important because `/api/unfurl` takes
the URL as a query param. An instance-wide **outbound concurrency cap** (3) backs
up the client's own throttle.

## Caching

Results live in the `link_preview` table (migrations v38/v39), keyed by
`reddit.CanonicalKey(url)`. An `ok` row serves for 14 days; a `failed` row is a
SHORT negative cache (1h) so it stops re-fetching on every viewport hit but a
**transient** failure (a host 429/timeout during a busy fetch burst) self-heals
on the next view rather than entombing the link for the full window. For the same
reason the `/api/unfurl` response is cached by the browser (`max-age=3600`) only
when `ok`; a `failed` response is sent `no-store` so the client never replays a
stale failure. `image_wide` and `video_url` carry the display-variant signals.

## Styling

A flexbox media-object themed purely with the portable theme vars. Surface +
neutral border are a translucent wash of `--text` via `color-mix` (not
`--foreground`, which equals the page background on the OLED/black themes and
left the card invisible) — `--text` always contrasts with the background, so a
low-alpha wash lightens dark themes and darkens light ones, covering every
built-in theme with zero per-theme overrides; the accent left edge is the one
solid brand colour. The card text rules are qualified with the `.link-preview`
ancestor to out-specify the markdown body's `.md a, .md a *` rule (the card is an
`<a>` inside `.md`), which would otherwise paint the card text in `--accent`.

## Configuration

```yaml
unfurl:
  enabled: true        # master switch for the whole feature
  jina_fallback: true  # opt into the r.jina.ai third-party reader (metadata only)
  timeout: 8s          # per-link server-side fetch ceiling
```

Overridable via `REDMEMO_UNFURL_*` env vars. Set `enabled: false` to render plain
links with no cards (the `/api/unfurl` endpoint then returns `failed`).

## Code map

| Concern | Location |
| --- | --- |
| OG/Twitter/video parse, host fixups, Jina fallback, SSRF | `internal/unfurl/unfurl.go`, `ssrf.go` |
| DB cache + single-flight + outbound cap, `ResolveOne` | `internal/unfurl/service.go` |
| Cache table | `internal/store/link_preview.go`, migrations v38/v39 |
| Lazy placeholder marking (server) | `internal/render/preview.go` (`markLazyLinks`) |
| Lazy loader + card build + variants (client) | `internal/render/static/linkPreview.js` |
| Card styling + variants | `internal/render/static/style.css` (`.link-preview*`) |
| Metadata endpoint | `internal/handler/unfurl.go`, route `GET /api/unfurl` |
