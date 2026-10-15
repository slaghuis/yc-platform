#!/usr/bin/env bash
# Wait for dev stack services to become healthy. Exits non-zero if any
# service is not ready within the timeout.
set -euo pipefail

timeout=90
interval=2
failed=0

wait_until() {
    local name=$1
    shift
    local elapsed=0
    printf "  Waiting for %s..." "$name"
    while [ $elapsed -lt $timeout ]; do
        if "$@" >/dev/null 2>&1; then
            echo " ready"
            return 0
        fi
        sleep $interval
        elapsed=$((elapsed+interval))
        printf "."
    done
    echo " TIMEOUT"
    failed=$((failed+1))
}

echo "▸ Waiting for dev stack..."
wait_until "Postgres" docker exec yc-postgres pg_isready -U postgres
wait_until "NATS"     curl -fsS http://localhost:8222/healthz
wait_until "S3"       curl -fsS http://localhost:9000/healthz
wait_until "Jaeger"   curl -fsS http://localhost:16686/
wait_until "MailHog"  curl -fsS http://localhost:8025/
if [ "${1:-}" = "--app" ]; then
    wait_until "yc-gateway"  curl -fsS http://localhost:8090/readyz
    wait_until "yc-identity" curl -fsS http://localhost:8081/readyz
    wait_until "yc-monitor"  curl -fsS http://localhost:8082/readyz
    wait_until "yc-product"  curl -fsS http://localhost:8083/readyz
    wait_until "yc-pricing"  curl -fsS http://localhost:8084/readyz
    wait_until "yc-image"    curl -fsS http://localhost:8085/readyz
    wait_until "yc-preference" curl -fsS http://localhost:8086/readyz
    wait_until "yc-cart"     curl -fsS http://localhost:8087/readyz
fi

if [ $failed -gt 0 ]; then
    echo ""
    echo "✗ $failed service(s) not ready. Check: docker compose ps && docker compose logs"
    exit 1
fi

echo ""
echo "✓ Dev stack ready."
echo ""
echo "  Postgres:  localhost:5432 (postgres / devpass)"
echo "  NATS:      localhost:4222 (admin: http://localhost:8222)"
echo "  S3:        http://localhost:9000 (devkey / devsecret, bucket yc-images)"
echo "  S3 admin:  http://localhost:9001"
echo "  Jaeger:    http://localhost:16686"
echo "  MailHog:   http://localhost:8025"
