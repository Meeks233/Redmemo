# RedMemo

> A self-hosted Reddit **archive station** with permanent local storage, built on the shoulders of [Redlib](https://github.com/redlib-org/redlib) and its ancestor [Libreddit](https://github.com/libreddit/libreddit).

![RedMemo browsing r/golang](docs/img/hero.png)

<sub>RedMemo serving <code>/r/golang</code> — UI inherited verbatim from Redlib, content served from the local archive when upstream is rate-limited.</sub>

---

**10-second pitch.** Take Redlib's UI, rewrite the back-end in Go, and treat every fetched post, comment and image as something to *keep forever*. Same routes, themes and cookies you already know from Redlib — plus a Postgres + content-addressed media archive underneath, a passive natural-prefetch scheduler, and a TOTP-gated `/settings` panel.

- 🗄 **Persistent** — every post & media blob ever seen lives in Postgres + an on-disk content-addressed store. Reddit deletions don't take your archive with them.
- 🐢 **Passive** — when upstream is blocked or rate-limited, requests degrade to the local archive with a small banner, never a hard 5xx.
- 🔐 **Gated** — `/settings` is locked behind a pre-shared server secret + TOTP, with 3-strike per-IP lockout.
- 🦫 **Go + templ** — server-side rendered; no JS framework, no client hydration, no client-side state.

## Documentation

The handbook lives in **[`docs/`](docs/README.md)**. Quick jumps:

- **[Quick Deployment](docs/Quick-Deployment.md)** — homelab and public Compose profiles
- **[Migration from Redlib](docs/Migration-from-Redlib.md)** — what's the same, what's different
- **[Architecture](docs/Architecture.md)** — four-level failover chain
- **[Persistence Layer](docs/Persistence.md)** — Postgres tables + media dedup
- **[Natural Prefetch](docs/Natural-Prefetch.md)** — passive background crawler
- **[HR Rate-Limit](docs/HR-Rate-Limit.md)** — global three-tier cap
- **[Configuration Reference](docs/Configuration.md)** — every `REDMEMO_*` env var
- **[Default User Settings](docs/Default-User-Settings.md)** — `REDMEMO_DEFAULT_*` overrides
- **[Search & URL Reference](docs/Search-Reference.md)** — e621-style unified grammar

## TL;DR deploy

```bash
mkdir redmemo && cd redmemo
curl -O https://raw.githubusercontent.com/redmemo/redmemo/main/deploy/docker-compose.homelab.yml
curl -O https://raw.githubusercontent.com/redmemo/redmemo/main/deploy/init.sql
mv docker-compose.homelab.yml docker-compose.yml
echo "PG_PASSWORD=$(openssl rand -hex 24)" > .env
docker compose up -d
```

Visit `http://<host>:8080/`. For a public, TOTP-gated profile and the full env-var matrix, see [Quick Deployment](docs/Quick-Deployment.md).

![TOTP gate on /settings](docs/img/totp.png)

<sub>The TOTP prompt guarding <code>/settings</code> on the public profile. 3-strike per-IP lockout, enrolment gated by <code>REDMEMO_SERVER_SECRET</code>.</sub>

## Credits

RedMemo would not exist without:

- **[Redlib](https://github.com/redlib-org/redlib)** — the entire front-end (templates, styles, themes, route shape, user-settings model) descends from Redlib. A reference copy lives in `_redlib_ref/`.
- **[Libreddit](https://github.com/libreddit/libreddit)** — the original alternative front-end Redlib was forked from, and the ultimate source of the UI everyone recognises.

## License

RedMemo inherits the AGPL-3.0 license of Redlib and Libreddit for the code paths derived from them. New code follows the same license unless explicitly stated.
