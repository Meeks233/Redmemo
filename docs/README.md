# RedMemo Wiki

User-facing handbook for RedMemo — a self-hosted Reddit **archive station**: it wears Redlib's UI, fetches via the Reddit API, and keeps a permanent local copy of every post and media blob it sees, so your archive survives upstream deletions and rate-limits. The repo-root [`README.md`](../README.md) is the 60-second pitch; everything detailed lives here.

**▶ New here?** See it running first — **[redmemo.meekslab.cc](https://redmemo.meekslab.cc)** (public demo, `/settings` TOTP-gated) — then start with **[Migration from Redlib](Migration-from-Redlib.md)** for the mental model and **[Quick Deployment](Quick-Deployment.md)** to stand up your own.

## Getting started

- [Quick Deployment](Quick-Deployment.md) — homelab and public Compose profiles
- [Migration from Redlib](Migration-from-Redlib.md) — what's the same, what's different, what to expect

## How it works

- [Architecture](Architecture.md) — four-level failover chain (ASCII diagram)
- [Persistence Layer](Persistence.md) — Postgres tables + content-addressed media store
- [Natural Prefetch (NP)](Natural-Prefetch.md) — passive background crawler (ASCII state machine), plus the `/settings` prefetch-field grammar
- [Archive Control](Archive-Control.md) — whitelist / blacklist filter for which subs get stored
- [HR Rate-Limit Layer](HR-Rate-Limit.md) — three-tier global cap shared via Redis
- [Budget Design](Budget-Design.md) — 50-per-call page size (quota is per-request, not per-item), navbar quota ring, auto-throttle vs Redlib's fixed 25-per-call drain
- [Link Preview (Unfurl)](Link-Preview.md) — Telegram-style preview cards for bare external links in post/comment bodies

## Configuration

- [Configuration Reference](Configuration.md) — every `REDMEMO_*` env var (`config.yaml` is fully optional — env-only is the recommended setup)
- [Default User Settings](Default-User-Settings.md) — `REDMEMO_DEFAULT_*` instance-wide overrides
- [Auth / TOTP Gate](Auth-TOTP.md) — enrolment flow, server secret, trusted devices, lockout, rotation, bypass mode

## Search and URLs

- [Search & URL Reference](Search-Reference.md) — e621-style unified grammar shared by `/search` and `/archive`
- [`/random` endpoint](Search-Reference.md#random-endpoint) — random archived post by the same unified grammar (JSON envelope, media redirect, or `mode:raw` bytes)

