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

# --- 1. Kill old redmemo process ---
log "Stopping old redmemo process..."
pkill -f './redmemo' 2>/dev/null && log "Killed old process" || log "No old process running"
sleep 1

# --- 2. Start infrastructure (postgres, redis, redlib) ---
log "Starting infrastructure containers..."
docker compose up -d postgres redis redlib

# --- 3. Wait for health checks ---
log "Waiting for postgres..."
for i in $(seq 1 30); do
    if docker compose exec -T postgres pg_isready -U redmemo -q 2>/dev/null; then
        break
    fi
    if [ "$i" -eq 30 ]; then
        err "Postgres failed to start"
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

# --- 5. Build redmemo ---
log "Building redmemo..."
go build -o ./redmemo ./cmd/redmemo
log "Build complete"

# --- 6. Start redmemo ---
log "Starting redmemo..."
nohup ./redmemo config.yaml > redmemo.log 2>&1 &
REDMEMO_PID=$!
log "RedMemo started (PID: $REDMEMO_PID)"

# --- 7. Verify startup ---
sleep 2
if kill -0 "$REDMEMO_PID" 2>/dev/null; then
    log "RedMemo is running"
else
    err "RedMemo failed to start, check redmemo.log:"
    tail -20 redmemo.log
    exit 1
fi

# --- 8. Quick health check ---
sleep 1
if curl -sf http://127.0.0.1:8080/info > /dev/null 2>&1; then
    log "Health check passed: http://127.0.0.1:8080"
else
    warn "Health check pending, redmemo may still be initializing"
    warn "Check: curl http://127.0.0.1:8080/info"
fi

echo ""
log "=== Deploy complete ==="
log "  RedMemo:  http://127.0.0.1:8080"
log "  Redlib:   http://127.0.0.1:8081"
log "  Logs:     tail -f redmemo.log"
log "  Stop:     pkill -f './redmemo'"
