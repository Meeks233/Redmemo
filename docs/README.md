# RedMemo Wiki

User-facing handbook for RedMemo. The repo-root [`README.md`](../README.md) is the 60-second pitch; everything detailed lives here.

## Getting started

- [Quick Deployment](Quick-Deployment.md) — homelab and public Compose profiles
- [Migration from Redlib](Migration-from-Redlib.md) — what's the same, what's different, what to expect

## How it works

- [Architecture](Architecture.md) — four-level failover chain (ASCII diagram)
- [Persistence Layer](Persistence.md) — Postgres tables + content-addressed media store
- [Natural Prefetch (NP)](Natural-Prefetch.md) — passive background crawler (ASCII state machine)
- [HR Rate-Limit Layer](HR-Rate-Limit.md) — three-tier global cap shared via Redis
- [Budget Design](Budget-Design.md) — 50-per-call page size (quota is per-request, not per-item), navbar quota ring, auto-throttle vs Redlib's fixed 25-per-call drain

## Configuration

- [Configuration Reference](Configuration.md) — every `REDMEMO_*` env var
- [Default User Settings](Default-User-Settings.md) — `REDMEMO_DEFAULT_*` instance-wide overrides
- [Auth / TOTP gate](Configuration.md#auth--totp-gate) — server secret, lockout, bypass mode

## Search and URLs

- [Search & URL Reference](Search-Reference.md) — e621-style unified grammar shared by `/search` and `/archive`
- [`/random` endpoint](Search-Reference.md#random-endpoint) — `t:` filter language

