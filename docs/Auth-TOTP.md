# Auth / TOTP Gate

← [Wiki index](README.md) · Related: [Configuration](Configuration.md#auth--totp-gate)

RedMemo gates the **operator surfaces** — `/settings` and `/debug` — behind a
two-factor flow: a pre-shared **server secret** plus a **TOTP** (RFC 6238,
HMAC-SHA1, 6 digits, 30s period) second factor. Everything else (browsing the
archive, `/random`, media proxy) is public. The gate protects the small set of
endpoints that change instance state or expose internals — nothing else.

## Why two factors

The **server secret** (`REDMEMO_SERVER_SECRET`) is a static, operator-held
string. On its own it is a single shared password, so it only ever pre-gates the
*enrolment* of the real second factor. The **TOTP secret** is generated once,
server-side, on first successful server-secret entry, shown as a QR **exactly
once**, and never reproduced by any endpoint. From then on routine access asks
only for the rolling 6-digit code.

## Enrolment flow

The gate is a small state machine (`internal/handler/auth.go`). A fresh visitor
walks the stages in order; an enrolled instance jumps straight to the code prompt.

| Stage | Shown when | Asks for |
|-------|-----------|----------|
| `safe_env` | first contact, before any secret-exposing stage | "Is this environment safe?" consent (cookie, 24h) |
| `server_secret` | no TOTP secret persisted yet | the `REDMEMO_SERVER_SECRET` value |
| `enroll` | secret persisted but not yet confirmed | scan the **one-time QR**, then the current 6-digit code |
| `totp` | enrolment confirmed | the current 6-digit code |

The safe-environment prompt only guards the two stages that put a long-lived
secret on screen (`server_secret`, `enroll`); the routine code prompt skips it.

An **interrupted** enrolment (secret persisted, QR shown, but the first code
never entered) is recoverable — the gate re-shows the QR (`enroll`) on the next
visit rather than stranding the owner at a bare code prompt for a secret they
never captured.

## Routine access & session tokens

A correct code mints an **ephemeral session token**: an `HttpOnly` cookie, bound
to the client IP it was issued to (a cookie replayed from another host is
rejected), verified on every `/settings` and `/debug` request, expiring
server-side with **no sliding window**. Lifetime is operator-configurable via the
`settings_token_ttl` setting — default **10 min**, clamped to **60 min**.

## Trust this device

Ticking **"Trust this device"** at the code prompt mints a separate, DB-backed
**trusted-device** cookie so that browser opens `/settings` without a fresh code:

- **Sliding 30-day window** — every validated request pushes the expiry forward,
  so an actively-used browser never lapses while an idle one dies on its own.
- **IP-bound** — like the session token, validation requires the presenting
  client IP to match the address the cookie was minted for.
- **Max 3 live devices** per instance; a 4th request is refused with a grace
  warning surfaced at `/settings?trusted=limit`.
- **Abuse tripwire** — 3 rejected trusted-cookie checks inside a 30-min rolling
  window **seal all trust** (validation *and* issuance) instance-wide until the
  window cools.
- Managed from `/settings` (list + revoke); `POST /settings/trusted/revoke`
  removes one. Revoking the device you are currently on logs that browser out on
  the spot.

Exactly **one** credential is handed out per unlock: if a trusted slot is taken,
no redundant session token is minted.

## Lockout & brute-force backstops

| Guard | Trigger | Effect |
|-------|---------|--------|
| Per-IP lockout | 3 wrong codes/secrets in one round | IP parked until the next 30s TOTP window; response 303s to `/fuckreddit?reason=auth_locked` |
| Global backstop | 10 failures across **all** IPs in one window | every IP locked for the window — covers the shared-IP-behind-proxy case where the per-IP bucket collapses |
| Single-use codes | a TOTP code replayed within its validity | refused as `totp_replay` (a valid-but-reused code reads as compromise) |

Behind a reverse proxy, set [`server.trusted_proxy_cidrs`](Configuration.md#server-settings)
so the per-IP lockout tracks real client IPs instead of the proxy's single
address — otherwise every client collapses into one bucket and only the global
backstop discriminates a burst.

## Rotation & reset

- **Rotate in-band**: at the `server_secret` stage of an already-enrolled
  instance, supplying the server secret **plus the current 6-digit code** mints a
  fresh TOTP secret and re-shows the QR. Proof of the current code is mandatory —
  a leaked server secret alone cannot silently rotate the second factor.
- **Admin reset**: `redmemo --reset-totp` wipes the persisted TOTP secret (the
  next `server_secret` POST enrols fresh). Both rotation and reset **revoke every
  trusted device**, since a 30-day trusted cookie minted under the old secret
  would otherwise outlive it.

## Bypass mode (homelab only)

`REDMEMO_AUTH_BYPASS=on` disables the TOTP gate entirely — `/settings` and
`/debug` are reachable with no cookie, and `REDMEMO_SERVER_SECRET` becomes
optional. POSTs still get a same-origin (`Origin`/`Referer`) CSRF check, the only
remaining brake. Intended **only** for a deployment already fenced by an outer
auth layer (Tailscale ACL, VPN, reverse-proxy SSO). **Never set it on a
public-facing instance** — the bundled `deploy/docker-compose.public.yml`
deliberately leaves it unset.

## Code map

| Concern | Location |
| --- | --- |
| TOTP primitive (RFC 6238, QR) | `internal/totp/totp.go` |
| Gate state machine, lockout, sessions, trust | `internal/handler/auth.go` |
| Secret persistence (`site_settings`) | `internal/store/totp.go` |
| Trusted-device table | `internal/store/trusted_device.go` |
| `--reset-totp` wipe | `cmd/redmemo/main.go` |
| Route wiring (`/settings`, `/debug`, `/settings/trusted/revoke`) | `internal/handler/router.go` |
</content>
</invoke>
