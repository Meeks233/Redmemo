# Quick Deployment

← [Wiki index](README.md)

RedMemo ships two ready-to-use Compose profiles. Pick the one that matches your network exposure, drop it into an empty directory, set the secrets in `.env`, then `docker compose up -d`. Both profiles boot on env vars alone — no `config.yaml` required.

## Homelab / LAN / Tailnet — auth bypassed

For a box behind Tailscale, a VPN, or a reverse-proxy SSO that already gates access. The TOTP prompt is off, `/debug` is open, and the instance is allowed to make live upstream calls.

```bash
mkdir redmemo && cd redmemo
curl -O https://raw.githubusercontent.com/Meeks233/Redmemo/main/deploy/docker-compose.homelab.yml
mv docker-compose.homelab.yml docker-compose.yml
echo "PG_PASSWORD=$(openssl rand -hex 24)" > .env
# Optional — pre-seed the NP crawl list (otherwise leave for /settings):
# echo "REDMEMO_DEFAULT_PREFETCH_SUBS=sub:golang+rust+selfhosted" >> .env
docker compose up -d
```

Visit `http://<host>:8080/settings` directly — no TOTP gate.

## Public instance — strict TOTP

For an internet-facing deployment behind nginx/Caddy + TLS. TOTP enforced, `/debug` hidden, on-demand upstream calls disabled (every page served from the local archive), SEO opt-in.

```bash
mkdir redmemo && cd redmemo
curl -O https://raw.githubusercontent.com/Meeks233/Redmemo/main/deploy/docker-compose.public.yml
mv docker-compose.public.yml docker-compose.yml
cat > .env <<EOF
PG_PASSWORD=$(openssl rand -hex 24)
SERVER_SECRET=$(openssl rand -hex 32)
EOF
docker compose up -d
```

First visit to `/settings` walks through the safe-environment ack → server-secret entry → TOTP QR enrolment. Lose the authenticator? `docker compose exec redmemo redmemo --reset-totp`.

## Required secrets

| Variable | Purpose |
|----------|---------|
| `PG_PASSWORD` | Password for the Postgres user used by the DSN. |
| `SERVER_SECRET` | Pre-shared secret required before TOTP enrolment / re-enrolment in `/settings`. Public profile only. |

Database (5432) and Redis (6379) ports are **not** exposed to the host — they live on the internal compose network only.

## Building from source

Requires Go ≥ 1.22.

```bash
git clone https://github.com/<you>/redmemo && cd redmemo
make build
./bin/redmemo -config config.yaml
```

The outbound HTTP transport embeds its own TLS stack — no system `openssl`/`boringssl` required.

## Where to go next

- [Configuration Reference](Configuration.md) — every env var
- [Default User Settings](Default-User-Settings.md) — `REDMEMO_DEFAULT_*` overrides
- [Architecture](Architecture.md) — what's running inside the container
