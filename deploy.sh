#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log()  { echo -e "${GREEN}[deploy]${NC} $*"; }
warn() { echo -e "${YELLOW}[deploy]${NC} $*"; }
err()  { echo -e "${RED}[deploy]${NC} $*"; }

# --- 1. Build redmemo image (while old containers keep running) ---
log "Building redmemo image..."
docker compose build redmemo

# --- 2. Ensure infrastructure is up ---
log "Starting infrastructure..."
docker compose up -d postgres redis redlib

# --- 3. Wait for health checks ---
log "Waiting for postgres..."
for i in $(seq 1 30); do
    if docker compose exec -T postgres pg_isready -U redmemo -q 2>/dev/null; then
        break
    fi
    if [ "$i" -eq 30 ]; then
        err "Postgres failed to start"
        docker compose logs postgres | tail -10
        exit 1
    fi
    sleep 1
done
log "Postgres ready"

log "Waiting for redis..."
for i in $(seq 1 30); do
    if docker compose exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; then
        break
    fi
    if [ "$i" -eq 30 ]; then
        err "Redis failed to start"
        exit 1
    fi
    sleep 1
done
log "Redis ready"

# --- 4. Flush Redis cache ---
log "Flushing Redis cache..."
docker compose exec -T redis redis-cli FLUSHALL
log "Redis cache cleared"

# --- 5. Recreate only redmemo with new image (infra stays up) ---
log "Restarting redmemo..."
docker compose up -d --no-deps --force-recreate redmemo

# --- 6. Verify startup ---
sleep 3
if docker compose ps redmemo | grep -q "Up"; then
    log "RedMemo container is running"
else
    err "RedMemo failed to start:"
    docker compose logs --tail=30 redmemo
    exit 1
fi

# --- 7. Health check ---
for i in $(seq 1 10); do
    if curl -sf http://127.0.0.1:8080/info > /dev/null 2>&1; then
        log "Health check passed"
        break
    fi
    if [ "$i" -eq 10 ]; then
        warn "Health check pending, container may still be initializing"
        warn "Check: curl http://127.0.0.1:8080/info"
    fi
    sleep 1
done

# --- 8. Clean up orphans ---
docker compose up -d --remove-orphans 2>/dev/null || true

echo ""
log "=== Deploy complete ==="
log "  RedMemo:  http://127.0.0.1:8080"
log "  Redlib:   http://127.0.0.1:8081"
log "  Logs:     docker compose logs -f redmemo"
log "  Stop:     docker compose down"
