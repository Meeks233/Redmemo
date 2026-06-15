# HR Rate-Limit Layer

← [Wiki index](README.md) · Related: [Architecture](Architecture.md) · [Natural Prefetch](Natural-Prefetch.md)

HR (Human and Robots) caps the **total** outbound Reddit traffic at three tumbling-window granularities. Counters in Redis are wall-clock-aligned, so multiple RedMemo instances behind a load balancer share one global budget without coordination.

## Tiers

| Tier | Window | Threshold | Cooldown when tripped |
|------|--------|-----------|------------------------|
| L1   | 5 s    | ≥ 5 requests | remainder of current window + the next full window |
| L2   | 30 s   | ≥ 15 requests | same |
| L3   | 5 min  | ≥ 50 requests | same |

## Important properties

- **Counters count both HR and NP traffic**, but **only HR requests are penalised**. Background prefetch continues even while HR is cooling down (it just contributes to tripping the cap).
- HR-cooldown does **not** hard-block the user. Instead the request is served from the archive with an amber banner that links to `/fuckreddit?reason=hr_cooldown_L1` (or similar).
- The HR gate is placed just before an upstream call would be issued, not at the outermost handler. Pure cache hits and explicit `/archive/...` routes never consult HR.
- **Failure mode**: if Redis is unreachable, HR fails **closed** (`reason=hr_redis_down`) — admitting traffic blind would let the cap leak. Redis is re-probed on exponential backoff (1 s → 30 s) and `/fuckreddit` actively pings to detect recovery.

## Atomicity

Counter / cooldown atomicity is guaranteed by a single Lua script that performs `INCR` + `EXPIRE` + (conditionally) `SET cooldown` per tier on every successful upstream call.

## Flow

```
   upstream call about to fire
            │
            ▼
   ┌──────────────────────────┐
   │ HR.Admit(reason)         │
   │  - read 3 cooldown keys  │
   │  - any active? → DENY    │
   └────────────┬─────────────┘
            ok │             denied
               ▼                ▼
   ┌──────────────────┐   ┌──────────────────────────┐
   │  fire request    │   │ serve archive + banner   │
   └────────┬─────────┘   │ link → /fuckreddit       │
            │             └──────────────────────────┘
            ▼
   ┌──────────────────────────┐
   │ HR.RecordUpstream()      │
   │  Lua: INCR + EXPIRE per  │
   │  tier; if threshold hit  │
   │  → SET cooldown key      │
   └──────────────────────────┘
```
