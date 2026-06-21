# Decentralized Discovery & SEO

← [Wiki index](README.md) · Related: [Natural Prefetch](Natural-Prefetch.md) · [Configuration → SEO](Configuration.md#seo)

## Why this exists

RedMemo has no central registry. There is no master list of instances, no index server, no account that knows where everything lives. That is the point — a registry is a single thing to seize, subpoena, or switch off. Instead, every self-hosted instance is an island that quietly mirrors the subreddits its operator cares about.

That design has one hard problem: **discovery**. If someone needs a surviving mirror of a subreddit that upstream deleted or locked, how do they find the one instance, among many they've never heard of, that happens to archive it? Asking a central directory is exactly the dependency we refuse to build.

The answer is to invert it. Rather than a directory that points *inward* at instances, every instance points *outward* and **advertises what it mirrors** — in the open, in formats the existing public web already indexes. The discovery layer is then just the search engines and aggregators that are already crawling the web. No new central party. A seeker types the sub name into any search engine and the instances that carry it surface on their own.

This is why **SEO is on by default** (see [Configuration → SEO](Configuration.md#seo)): an instance that hides from crawlers contributes nothing to the network's findability. A public node earns its place by being discoverable. Private LAN/Tailnet instances, which serve nobody but their operator, turn it off.

## The discovery contract

Each public instance advertises its **Natural-Prefetch (NP) sub list** — the subreddits it has chosen to mirror — through four crawler-facing surfaces. All of them advertise the **union** of subs *already archived* and subs *configured but not yet populated*, so a freshly stood-up instance is findable by its chosen subs from minute one, before its first crawl cycle even lands.

| Surface | Audience | What it carries |
|---------|----------|-----------------|
| `/np.json` | Aggregators, scripts, directory sites | Machine-readable JSON: brand, host, archive URL, the full NP sub list, and a per-sub link with an `archived` flag. CORS-open so browser-based tools can read it. |
| `/sitemap.xml` | Search-engine crawlers | Every `/archive/r/<sub>` URL, priority-ranked by archive depth, with `lastmod`. |
| `/archive` hub + `/archive/r/<sub>` | Search-engine result snippets | Human-readable pages stamped with a `<meta name=description>` listing the subs, plus JSON-LD (`CollectionPage` + `ItemList` + `BreadcrumbList`) that maps *instance → subs* in the knowledge graph. |
| `/robots.txt` | All crawlers | Opens the archive surfaces + `/np.json`, points to the sitemap, and keeps everything else (Reddit duplicates, settings, media proxies) out. |

Only the surfaces that describe **this instance** — what it mirrors — are opened. The mirrored Reddit content itself (`/r/`, post pages) stays `noindex`/`Disallow`: it's a duplicate of Reddit, thin-content and DMCA-exposed, and indexing it would bury the one page that matters (the archive) under thousands that don't.

### `/np.json` — the machine-readable advert

This is the primitive an aggregator or directory site crawls to map *sub → mirror* without scraping HTML. The schema is intentionally flat and additive — new fields may be appended, existing ones never change meaning.

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

`archived: true` means the sub already has stored posts; `false` means it's configured for NP but still warming up. Served at the site root, allowed in `robots.txt`, cross-origin readable, `404`s when indexing is off.

## How a seeker finds an instance

1. A subreddit gets deleted, quarantined, or locked upstream.
2. The seeker searches the open web for the sub name (or queries a community-run aggregator that periodically crawls known `/np.json` endpoints).
3. Public RedMemo instances that mirror that sub surface — their archive pages are indexed under the sub name, and their `/np.json` lists it.
4. The seeker lands on `…/archive/r/<sub>` and browses the surviving copy. No central directory was consulted at any step.

The network's resilience is emergent: take down any one instance and the others that mirror the same sub are still indexed independently. There is no chokepoint because there is no center.

## Operating a discoverable node

Set these on a public instance (see [Quick Deployment](Quick-Deployment.md) → public profile):

- **Configure the NP sub list** (`REDMEMO_DEFAULT_PREFETCH_SUBS`, or `/settings`) — this *is* what you advertise. An empty list advertises nothing.
- **Set `REDMEMO_SEO_CANONICAL_HOST`** to your real `https://` origin so `/np.json`, `/sitemap.xml`, and `<link rel=canonical>` emit absolute URLs that crawlers and aggregators can follow.
- **Leave `REDMEMO_SEO_ALLOW_INDEXING` on** (the default). Set it to `false` only for a private LAN/Tailnet box that should stay dark — that flips `/robots.txt` to `Disallow: /`, `404`s `/sitemap.xml` + `/np.json`, and keeps every page `noindex,nofollow`.

A private homelab instance is the mirror image: it serves only its operator, so it advertises nothing and the homelab Compose profile ships with indexing **off**.

## See also

- [Configuration → SEO](Configuration.md#seo) — the env vars and the full surface behavior
- [Natural Prefetch](Natural-Prefetch.md) — how the advertised sub list is chosen and crawled
- [Quick Deployment](Quick-Deployment.md) — public vs homelab Compose profiles
