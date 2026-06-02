# Natural Prefetch (NP)

← [Wiki index](README.md) · Related: [Architecture](Architecture.md) · [HR Rate-Limit](HR-Rate-Limit.md)

NP is a producer/consumer pipeline that quietly fills the archive without burst patterns. All outbound traffic — both the L1 Reddit-API listing fetch and the L2 CDN media downloads — flows through one **NP dispatcher** goroutine that applies a 1–3 s random delay between calls and pauses 30 s after any user-triggered upstream request.

Full design notes in [`docs/prefetch.md`](../docs/prefetch.md).

## Layers

| Layer | Trigger | Cost / round | Behaviour |
|-------|---------|--------------|-----------|
| **L1** Shallow archive | Big cycle every 12–24 h, up to 8 rounds per cycle, 15–30 min between rounds | 1 Reddit API request | Walks `hot` listing with `after` cursor for ~200 posts per cycle. UPSERTs into `posts`, preserves `media_done`. Identifies new posts and hands them to L2. |
| **L2** Media archive | Runs immediately after each L1 round | 0 Reddit API requests | Sorts new posts newest-first, downloads image/video/gallery via CDN, marks `media_done = true`. Verifies files still exist on disk and re-downloads if evicted. |
| **L3** Deep archive | Passive — user visits a post page | 1 Reddit API request on demand | Comments are only ever fetched when a human asks for them. |
| **L4** Icon cache | Startup + every 1 h + on `/archive` view | 1 Reddit API request per sub when stale | Keeps `sub_icons` fresh (default TTL 30 days). Icon image itself is a CDN download. |

## Producer / consumer state machine

```
   ┌───────────────────────────────────────────────────────────┐
   │            NP scheduler tick (every 30 s)                 │
   └────────────────────────────┬──────────────────────────────┘
                                │  next big cycle due?
                                ▼
            ┌──────────────────────────────────────────┐
            │  big cycle start (12-24 h cadence)       │
            │  picks one sub from prefetch_config      │
            └────────────────────┬─────────────────────┘
                                 │
                                 ▼
                  ┌──────────────────────────────┐
                  │   ┌──────────────────────┐   │
                  │   │  L1 producer round   │◀──┼────┐
                  │   │  enqueue 1 API call  │   │    │
                  │   └──────────┬───────────┘   │    │
                  │              │ posts         │    │ next round
                  │              ▼               │    │ 15-30 min
                  │   ┌──────────────────────┐   │    │ later
                  │   │  L2 producer round   │   │    │
                  │   │  enqueue N CDN dl    │   │    │
                  │   └──────────┬───────────┘   │    │
                  │              │               │    │
                  │              └───────────────┼────┘
                  │   (up to 8 rounds / cycle)   │
                  └──────────────┬───────────────┘
                                 │ all rounds done
                                 ▼
            ┌──────────────────────────────────────────┐
            │  sleep until next big cycle              │
            └──────────────────────────────────────────┘

   ─────────────────────────────────────────────────────────────
                  Dispatcher goroutine (single consumer)
   ─────────────────────────────────────────────────────────────
   ┌─────────────┐    ┌──────────────────────────────────────┐
   │  L1 + L2    │───▶│  - 1-3 s jitter between calls        │
   │  work queue │    │  - 30 s pause after user upstream    │
   │             │    │  - HR counters incremented, but NP   │
   │  L4 icons   │───▶│    is not penalised by HR cooldown   │
   └─────────────┘    └──────────────────────────────────────┘

   L3 (comments): on-demand only, fired from the post handler,
                  never scheduled here.
```

L1 and L2 never call the network directly — they submit work items to the dispatcher and block until it runs them, so a single pacing layer governs everything.
