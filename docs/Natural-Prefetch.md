# Natural Prefetch (NP)

← [Wiki index](README.md) · Related: [Architecture](Architecture.md) · [HR Rate-Limit](HR-Rate-Limit.md)

NP is a producer/consumer pipeline that quietly fills the archive without burst patterns. All outbound traffic — both the L1 Reddit-API listing fetch and the L2 CDN media downloads — flows through one **NP dispatcher** goroutine that applies a 1–3 s random delay between calls and pauses 30 s after any user-triggered upstream request.

## Layers

| Layer | Trigger | Cost / round | Behaviour |
|-------|---------|--------------|-----------|
| **L1** Shallow archive | One cycle per sub-timeframe bucket; cycle period = bucket base ± jitter; one fetch per sub per cycle, randomly spaced within the cycle | 1 Reddit API request per sub per cycle | Walks the sub's resolved listing (`/r/{sub}/{sort}.json?t=`) with `after` cursor. UPSERTs into `posts`, preserves `media_done`. Identifies new posts and hands them to L2. |
| **L2** Media archive | Runs immediately after each L1 fetch | 0 Reddit API requests | Sorts new posts newest-first, downloads image/video/gallery via CDN, marks `media_done = true`. Verifies files still exist on disk and re-downloads if evicted. |
| **L3** Deep archive | Passive — user visits a post page | 1 Reddit API request on demand | Comments are only ever fetched when a human asks for them. |
| **L4** Icon cache | Startup + every 1 h + on `/archive` view | 1 Reddit API request per sub when stale | Keeps `sub_icons` fresh (default TTL 30 days). Icon image itself is a CDN download. |

## Depth (which layers run per sub)

NP exposes a **depth** dimension on top of the L1 / L2 / L3 layer split: it controls whether each crawled sub also runs media downloads and/or comment fetches. Resolved per sub, override > global:

| Value | L1 (listings) | L2 (media) | L3 (comments) |
|-------|---------------|------------|---------------|
| `none` | yes | no | no |
| `l2`   | yes | yes | no |
| `l3`   | yes | no  | yes (each post in the wave) |
| `l2+l3` | yes | yes | yes (visit-like, default) |

- Global default: `prefetch_default_depth` (storage key) / `REDMEMO_DEFAULT_PREFETCH_DEFAULT_DEPTH` (env). The settings page renders it as the **Default depth** select on the NP fieldset.
- Per-sub override: append `depth:<value>` inside a prefetch override clause, e.g. `golang=depth:l2+l3&sort:top` or `rust=depth:none`. Override wins per-sub. Unknown values are dropped.
- Common pattern: set the global default to `none` and opt specific subs into media+comments via `<sub>=depth:l2+l3` — this is how the settings page documents single-sub deep crawls.

## Driving NP from /settings — the prefetch field

The entire crawl list lives in **one text box** on the settings page (Natural Prefetch → subreddits). It is a single `+`-separated stream where each clause is either:

- a **bare subreddit name** — `cats` — crawled with the global Default sort / timeframe / depth, or
- a **per-sub override clause** — `cats=sort:rising&time:day&depth:l2+l3` — the same sub crawled with its own sort / timeframe / depth.

```
field   := <clause>(+<clause>)*
clause  := <sub>                       # bare — inherit the global defaults
         | <sub>=<k>:<v>(&<k>:<v>)*     # override one or more dimensions for this sub
```

Recognised override keys (case-insensitive; unknown pieces are silently dropped):

| Key | Aliases | Values | Effect |
|-----|---------|--------|--------|
| `sort` | — | `hot` `new` `top` `rising` `controversial` | listing sort for this sub |
| `time` | `t`, `timeframe` | `hour` `day` `week` `month` `year` `all` | listing timeframe — also selects the cadence bucket (see below) |
| `depth` | `d` | `none` `l2` `l3` `l2+l3` | which layers run (see the Depth table above; `l1` / `off` are accepted as aliases for `none`) |

How clauses resolve:

- A clause overrides **only** the dimensions you name; anything you omit falls back to the global Default sort / timeframe / depth.
- Override clauses always **win over** the global defaults for that sub.
- The parser is **lenient**: an unknown key, a misspelled value, or a malformed `k:v` pair is dropped on its own without killing the rest of the clause. A clause that ends up with *no* usable override at all is dropped whole.
- **Duplicate sub names** collapse to the last occurrence.
- The `+` inside `depth:l2+l3` is understood as part of the value, not a clause separator.

Operators can seed the same content at boot: `REDMEMO_DEFAULT_PREFETCH_SUBS` (bare list), `REDMEMO_DEFAULT_PREFETCH_SUB_MODES` (override clauses), `REDMEMO_DEFAULT_PREFETCH_DEFAULT_DEPTH` (global depth).

### Examples

```
golang+rust+linux
```
Crawl three subs, all on the global defaults.

```
news=time:hour&sort:hot
```
Crawl r/news on the hourly cadence with hot sort; every other dimension stays at the default.

```
golang=depth:l2+l3&sort:top+rust=depth:none
```
r/golang gets full media + comments on top sort; r/rust fetches listings only (no media, no comments).

```
cats+dogs+golang=sort:rising&time:day+rust=depth:l2+l3&sort:top
```
Mixed list: cats and dogs on defaults; golang on rising/day; rust full-depth on top.

> Common pattern: set the global **Default depth** to `None` and opt individual subs into media/comments with `<sub>=depth:l2+l3` — cheap by default, deep only where you want it.

## Per-timeframe bucket cadence

L1 no longer runs a single global cycle. Each subreddit's resolved timeframe (per-sub `time:` clause in `prefetch_sub_modes`, else the global `prefetch_timeframe`, else `day`) maps it to one of six fixed-period buckets:

| Bucket | Base period | Notes |
|--------|-------------|-------|
| `hour`  | 6 h    | Finest cadence — for fast-churn subs (news / live events) |
| `day`   | 12 h   | Default when no timeframe is configured |
| `week`  | 48 h   | |
| `month` | 15 days | |
| `year`  | 180 days | |
| `all`   | 365 days | Coldest — for archives that rarely change |

The bucket is read from the *raw* timeframe even when the sort doesn't honour it. A sub configured as `sort:hot&time:week` still fires on a weekly cadence — the timeframe is dropped only before it reaches the listing URL, not from the cadence decision.

Each bucket period is jittered by ±20 % each cycle, so the actual wall-clock between two cycles of the same bucket is roughly base × [0.80, 1.20]. Within one cycle, subs are shuffled into a fresh random order; each sub's sleep gap is the *remaining cycle budget* divided by *subs left*, then jittered again by ±20 %. The result is that two cycles of a Day bucket containing [a, b, c] might run as `[2.1 h: b, 5.4 h: a, 9.8 h: c]` then `[3.0 h: c, 4.6 h: a, 8.3 h: b]` — different order, different gaps, same nominal 12 h cadence.

The per-sub gap is floored at `minBucketGap` (30 s) so unlucky jitter can't squeeze two fetches into a dispatcher cooldown. The cycle period is floored at `gap × N` and at `minCyclePeriod` (1 min) so a misconfigured override can never produce a sub-second cycle.

```
   ┌──────────────────────────────────────────────────────────────┐
   │  Coordinator goroutine (one per Scheduler)                    │
   │  - reconciles bucket loops with prefetch_subs / sub_modes     │
   │  - on settings change: cancels old loops, launches new ones   │
   └────────────────────────────┬─────────────────────────────────┘
                                │ groups subs by timeframe bucket
                                ▼
       ┌────────────┐ ┌────────────┐ ┌────────────┐
       │ hour bucket│ │ day bucket │ │ week bucket│   …  (up to 6)
       │  loop      │ │  loop      │ │  loop      │
       └──────┬─────┘ └──────┬─────┘ └──────┬─────┘
              │              │              │
              │  one cycle = shuffle subs, sleep+fetch+L2 per sub
              │              │              │
              ▼              ▼              ▼
            ┌──────────────────────────────────┐
            │   NP dispatch queue (single)     │
            │   - 4-8 s cooldown per call      │
            │   - HR budget gate (L1 only)     │
            │   - 25-40 s pause if user active │
            └──────────────────┬───────────────┘
                               ▼
                        Reddit API + CDN

   L3 (comments): on-demand only, fired from the post handler,
                  never scheduled here.
   L4 (icons):    independent hourly loop, sharing the dispatcher.
   L5 (audio):    drains once per L1 bucket cycle, after L2.
```

All bucket loops feed work items into the single NP dispatch queue, so the total outbound rate is still bounded by the dispatcher's per-call cooldown and the HR budget — no matter how many buckets are active at once.

Cursors persist per-bucket (`_prefetch_bucket_state_<tf>`) and per `(sub, sort, t)` key. They advance through a sub's listing across cycles; when a cursor exhausts within a cycle, the bucket clears the per-cycle exhaustion map and re-walks from the head on the next cycle so new "hot" / "top" content is still captured.
