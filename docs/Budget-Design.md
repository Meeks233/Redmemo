# Budget Design

← [Wiki index](README.md) · Related: [HR Rate-Limit](HR-Rate-Limit.md) · [Natural Prefetch](Natural-Prefetch.md) · [Architecture](Architecture.md)

The upstream API hands every app a small per-window request budget. Spend it carelessly on one drive-by visitor and the whole instance browns out. RedMemo treats that budget as a **shared resource** with three cooperating defences: shrunken page sizes, a live navbar ring, and an HR-layer auto-throttle that degrades gracefully into the local archive.

## TL;DR — what this buys you

- **Bigger page per request, fewer requests overall.** The upstream OAuth budget is measured in **requests**, not items — a `limit=50` call and a `limit=5` call both count as one tick on the meter. So a `/r/<sub>` entry or `/search` page is targeted to fetch **50 posts** per upstream call by default. Operators who want a tighter cap can dial `page_limit` down to as little as 5 from the settings page.
- **Live navbar ring.** Every page renders a circular SVG ring next to the search box. The arc length is the **remaining fraction** of the current budget window; the centre number is the integer count of calls still on the meter.
- **Auto-calculated balance.** The ring's percentage isn't a guess — it's derived from the rate-limit metadata returned by the upstream API on the most recent successful call, surfaced through `internal/ratelimit/manager.go`.
- **HR-layer auto-throttle.** When the budget is low (or any HR tier trips), the next upstream attempt is denied **before** it leaves the box and the request degrades to the archive with an amber banner. No silent retries.

## Contrast with the legacy default

| | Legacy default | RedMemo |
|---|---|---|
| Page size per upstream call | **25** posts (fixed) | **50** posts default, operator-configurable 5–100 |
| Concurrent visitor cost | Any anonymous visitor can drain the shared budget, kicking everyone off | Shared global counter; HR caps per 5 s / 30 s / 5 min windows |
| When the budget is empty | Hard error surfaced to the user, no graceful fallback | Amber banner + archive serves the same route, link explains why |
| Visibility | None — you find out by getting errors | Navbar ring shows live remaining budget on every page |
| Recovery hygiene | Blind retries that keep burning quota | HR fails closed, exponential reprobe, no blind retry |

## How the ring renders

The `<svg class="nav-ring">` in [`internal/render/layout.templ`](../internal/render/layout.templ) is hydrated by [`internal/render/static/quotaRing.js`](../internal/render/static/quotaRing.js). After each upstream call, the response handler hands the headers to `ratelimit.Manager.OnRequestComplete(...)`, which exposes a JSON status snapshot the front-end consumes.

Colour states (driven by a CSS class on `#status_quota`):

| State | Class | Trigger |
|---|---|---|
| Healthy | `ring-ok` | remaining ≥ 50 % |
| Warning | `ring-warn` | remaining ∈ [10 %, 50 %) |
| Critical | `ring-crit` | remaining < 10 % |
| Cooldown | `ring-cooldown` | HR tier active — ring shows next-tier reset countdown |

Clicking the ring routes to an explainer page with the cooldown reason and the next reset wall-clock in plain English / 中文.

## Why a larger page size

Listing endpoints accept up to 100 items per request, and Reddit's OAuth budget is decremented per HTTP call regardless of how many items the response carries — confirmed by the `X-Ratelimit-Used` header, which moves by 1 per request whether you ask for 5 or 50. A small page therefore wastes budget twice:

1. **Same cost, fewer posts.** A `limit=5` call burns one quota tick for 5 posts; a `limit=50` call burns one tick for 50. The user who scrolls 30 posts pays 6 ticks at `limit=5` but only 1 tick at `limit=50`.
2. **Fresh tail material is essentially free.** Reddit already gathers the full window server-side; extra items in the response come back at no marginal cost to the operator's quota.
3. **The archive still benefits.** Every fetched post is fed into the local archive on the way out, so a 50-post fetch front-loads cold storage for searches and infinite scroll that follow.

`page_limit` remains operator-tunable down to 5 for situations where a tighter cap on archive growth or per-page bandwidth matters more than quota efficiency.

## Wiring (file map)

- Page-size constants — `internal/handler/archive.go` (`archivePageSize`), `internal/handler/search.go`.
- Budget tracking — `internal/ratelimit/manager.go` (`Status.Remaining`, `OnRequestComplete`).
- HR gate — `internal/hrlimit/` + see [HR Rate-Limit](HR-Rate-Limit.md).
- Ring markup — `internal/render/layout.templ`.
- Ring script — `internal/render/static/quotaRing.js`.

## Tuning — survives policy changes without a rebuild

Every upstream-budget knob below is **env-overridable at process start**. If the upstream provider moves the goalposts again, change a number in `.env` and `docker compose up -d` — no image rebuild, no recompile, no fork required. The container ships with conservative defaults; everything else is yours to dial.

| Knob | Env var | Default |
|---|---|---|
| Window size | `REDMEMO_RATELIMIT_WINDOW_SIZE` | `500` |
| Window duration | `REDMEMO_RATELIMIT_WINDOW_DURATION` | `10m` |
| Safety buffer (reserved for NP / prefetch) | `REDMEMO_RATELIMIT_SAFETY_BUFFER` | `50` |
| HR L1 burst | `REDMEMO_HRLIMIT_L1_WINDOW` / `_THRESHOLD` | `5s` / `5` |
| HR L2 sustained | `REDMEMO_HRLIMIT_L2_WINDOW` / `_THRESHOLD` | `30s` / `15` |
| HR L3 long-haul | `REDMEMO_HRLIMIT_L3_WINDOW` / `_THRESHOLD` | `5m` / `50` |
| HR layer master switch | `REDMEMO_HRLIMIT_ENABLED` | `on` |
| Ring poll interval | `quotaRing.js` constant | `5s` |

The defaults are deliberately conservative so a default install behaves politely on a residential IP. If your situation lets you safely raise the budget, bump `REDMEMO_RATELIMIT_WINDOW_SIZE` and the HR thresholds proportionally; if you want to tighten, lower them. The container reads these on every boot — operators do not need a new image to keep up with policy changes.

Lowering `WindowSize` or raising `SafetyBuffer` also shifts the ring red earlier — useful when you want a wider margin.
