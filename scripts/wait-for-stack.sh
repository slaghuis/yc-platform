#!/usr/bin/env bash
# Wait for dev stack services to become healthy.
set -euo pipefail

timeout=60
interval=2
elapsed=0

wait_for() {
    local name=$1
    local url=$2
    printf "  Waiting for %s..." "$name"
    while [ $elapsed -lt $timeout ]; do
        if curl -fsS "$url" >/dev/null 2>&1; then
            echo " ready"
            return 0
        fi
        sleep $interval
        elapsed=$((elapsed+interval))
        printf "."
    done
    echo " TIMEOUT"
    return 1
}

echo "▸ Waiting for dev stack..."
wait_for "NATS"    "http://localhost:8222/healthz"   || true
wait_for "MinIO"   "http://localhost:9000/minio/health/ready" || true
wait_for "Jaeger"  "http://localhost:16686/"         || true
wait_for "MailHog" "http://localhost:8025/"          || true

# Postgres doesn't have an HTTP endpoint; probe with pg_isready via docker
printf "  Waiting for Postgres..."
elapsed=0
while [ $elapsed -lt $timeout ]; do
    if docker exec yc-postgres pg_isready -U postgres >/dev/null 2>&1; then
        echo " ready"
        break
    fi
    sleep $interval
    elapsed=$((elapsed+interval))
    printf "."
done

echo ""
echo "✓ Dev stack ready."
echo ""
echo "  Postgres:  localhost:5432 (postgres / devpass)"
echo "  NATS:      localhost:4222 (admin: http://localhost:8222)"
echo "  MinIO:     http://localhost:9001 (devkey / devsecret)"
echo "  Jaeger:    http://localhost:16686"
echo "  MailHog:   http://localhost:8025"